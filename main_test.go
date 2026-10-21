package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePaths(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*testing.T) (map[string]string, string)
		wantErr string
	}{
		{
			name: "requires a socket path",
			setup: func(*testing.T) (map[string]string, string) {
				return map[string]string{"HERDR_SOCKET_PATH": ""}, ""
			},
			wantErr: "HERDR_SOCKET_PATH is not set",
		},
		{
			name: "uses the plugin state dir",
			setup: func(t *testing.T) (map[string]string, string) {
				stateDir := filepath.Join(t.TempDir(), "state")
				return map[string]string{
					"HERDR_SOCKET_PATH":      "/tmp/herdr.sock",
					"HERDR_PLUGIN_STATE_DIR": stateDir,
				}, stateDir
			},
		},
		{
			name: "falls back to xdg state home",
			setup: func(t *testing.T) (map[string]string, string) {
				xdg := t.TempDir()
				return map[string]string{
					"HERDR_SOCKET_PATH":      "/tmp/herdr.sock",
					"HERDR_PLUGIN_STATE_DIR": "",
					"XDG_STATE_HOME":         xdg,
				}, filepath.Join(xdg, "herdr-agent-time")
			},
		},
		{
			name: "falls back to the home directory",
			setup: func(t *testing.T) (map[string]string, string) {
				home := t.TempDir()
				return map[string]string{
					"HERDR_SOCKET_PATH":      "/tmp/herdr.sock",
					"HERDR_PLUGIN_STATE_DIR": "",
					"XDG_STATE_HOME":         "",
					"HOME":                   home,
				}, filepath.Join(home, ".local", "state", "herdr-agent-time")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, wantDir := tt.setup(t)
			for key, value := range env {
				t.Setenv(key, value)
			}

			paths, err := resolvePaths()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, wantDir, paths.dir)
			assert.Equal(t, env["HERDR_SOCKET_PATH"], paths.socket)

			info, err := os.Stat(paths.dir)
			require.NoError(t, err)
			assert.True(t, info.IsDir())
			assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

			id := filepath.Base(paths.state)
			id = id[len("state-") : len(id)-len(".json")]
			assert.Equal(t, filepath.Join(paths.dir, "daemon-"+id+".lock"), paths.lock)
			assert.Equal(t, filepath.Join(paths.dir, "daemon-"+id+".pid"), paths.pid)
			assert.Equal(t, filepath.Join(paths.dir, "daemon-"+id+".json"), paths.identity)
			assert.Equal(t, filepath.Join(paths.dir, "daemon-"+id+".log"), paths.log)
		})
	}
}

func TestResolvePathsSocketIsolation(t *testing.T) {
	tests := []struct {
		name     string
		socketA  string
		socketB  string
		wantSame bool
	}{
		{name: "same socket shares state", socketA: "/tmp/herdr-a.sock", socketB: "/tmp/herdr-a.sock", wantSame: true},
		{name: "different sockets isolate state", socketA: "/tmp/herdr-a.sock", socketB: "/tmp/herdr-b.sock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := t.TempDir()
			t.Setenv("HERDR_SOCKET_PATH", tt.socketA)
			t.Setenv("HERDR_PLUGIN_STATE_DIR", stateDir)
			pathsA, err := resolvePaths()
			require.NoError(t, err)

			t.Setenv("HERDR_SOCKET_PATH", tt.socketB)
			pathsB, err := resolvePaths()
			require.NoError(t, err)

			assert.Equal(t, tt.wantSame, pathsA.state == pathsB.state)
			if !tt.wantSame {
				assert.NotEqual(t, pathsA.lock, pathsB.lock)
				assert.NotEqual(t, pathsA.pid, pathsB.pid)
				assert.NotEqual(t, pathsA.identity, pathsB.identity)
				assert.NotEqual(t, pathsA.log, pathsB.log)
			}
		})
	}
}

func TestAcquireLock(t *testing.T) {
	tests := []struct {
		name      string
		holdFirst bool
		wantErr   bool
	}{
		{name: "acquires a free lock"},
		{name: "rejects a second lock", holdFirst: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.lock")
			if tt.holdFirst {
				first, err := acquireLock(path)
				require.NoError(t, err)
				defer func() { _ = first.Close() }()
			}

			lock, err := acquireLock(path)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN))
				assert.True(t, lockHeld(path))
				return
			}

			require.NoError(t, err)
			assert.True(t, lockHeld(path))
			require.NoError(t, lock.Close())
			assert.False(t, lockHeld(path))
		})
	}
}

func TestReadPID(t *testing.T) {
	tests := []struct {
		name    string
		content string
		write   bool
		want    int
		wantErr bool
	}{
		{name: "reads a pid", content: "123\n", write: true, want: 123},
		{name: "trims whitespace", content: " 42 ", write: true, want: 42},
		{name: "rejects a malformed pid", content: "abc", write: true, wantErr: true},
		{name: "rejects an empty pid", content: "", write: true, wantErr: true},
		{name: "rejects a zero pid", content: "0", write: true, wantErr: true},
		{name: "rejects a negative pid", content: "-1", write: true, wantErr: true},
		{name: "rejects a missing file", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.pid")
			if tt.write {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			}
			pid, err := readPID(path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, pid)
		})
	}
}

func TestSocketInode(t *testing.T) {
	tests := []struct {
		name    string
		write   bool
		wantErr bool
	}{
		{name: "reads an inode", write: true},
		{name: "rejects a missing socket", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "herdr.sock")
			if tt.write {
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			}
			inode, err := socketInode(path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotZero(t, inode)
		})
	}
}

func TestDaemonIdentityMatches(t *testing.T) {
	tests := []struct {
		name  string
		left  daemonIdentity
		right daemonIdentity
		want  bool
	}{
		{
			name:  "same executable and hash",
			left:  daemonIdentity{Executable: "/bin/sh", SHA256: "abc"},
			right: daemonIdentity{Executable: "/bin/sh", SHA256: "abc"},
			want:  true,
		},
		{
			name:  "different hash",
			left:  daemonIdentity{Executable: "/bin/sh", SHA256: "abc"},
			right: daemonIdentity{Executable: "/bin/sh", SHA256: "def"},
		},
		{
			name:  "different executable",
			left:  daemonIdentity{Executable: "/bin/sh", SHA256: "abc"},
			right: daemonIdentity{Executable: "/bin/zsh", SHA256: "abc"},
		},
		{
			name:  "different plugin root",
			left:  daemonIdentity{Executable: "/bin/sh", SHA256: "abc", PluginRoot: "/old"},
			right: daemonIdentity{Executable: "/bin/sh", SHA256: "abc", PluginRoot: "/new"},
		},
		{
			name:  "missing hash",
			left:  daemonIdentity{Executable: "/bin/sh"},
			right: daemonIdentity{Executable: "/bin/sh"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.left.matches(tt.right))
		})
	}
}

func TestDaemonIdentityRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "round trips", content: ""},
		{name: "rejects incomplete identity", content: `{"pid":1}`, wantErr: true},
		{name: "rejects invalid json", content: "{", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.json")
			if tt.content != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			} else {
				require.NoError(t, writeIdentity(path, daemonIdentity{
					PID:           123,
					Executable:    "/bin/sh",
					SHA256:        "abc",
					PluginRoot:    "/plugin",
					StartedUnixMS: 42,
				}))
			}
			identity, err := readIdentity(path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, daemonIdentity{
				PID:           123,
				Executable:    "/bin/sh",
				SHA256:        "abc",
				PluginRoot:    "/plugin",
				StartedUnixMS: 42,
			}, identity)
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		})
	}
}

func TestClearOrphanedTokens(t *testing.T) {
	tests := []struct {
		name        string
		state       *Tracker
		wantRequest bool
	}{
		{name: "no state file"},
		{
			name: "clears a token",
			state: func() *Tracker {
				tracker := NewTracker()
				tracker.Panes["w1:p1"] = &PaneState{Agent: "omp", HadToken: true}
				return tracker
			}(),
			wantRequest: true,
		},
		{
			name: "skips a pane without a token",
			state: func() *Tracker {
				tracker := NewTracker()
				tracker.Panes["w1:p1"] = &PaneState{Agent: "omp"}
				return tracker
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newFakeHerdr(t, 0, func(request apiRequest) apiResponse {
				return apiResponse{ID: request.ID, Result: json.RawMessage(`{}`)}
			})
			dir := t.TempDir()
			p := paths{
				dir:    dir,
				socket: server.path,
				state:  filepath.Join(dir, "state.json"),
			}
			if tt.state != nil {
				require.NoError(t, tt.state.Save(p.state))
			}
			clearOrphanedTokens(p)
			requests := server.requestLog()
			if !tt.wantRequest {
				assert.Empty(t, requests)
				return
			}
			require.Len(t, requests, 1)
			assert.Equal(t, "pane.report_metadata", requests[0].Method)
			params := requests[0].Params.(map[string]any)
			assert.Equal(t, "w1:p1", params["pane_id"])
			assert.Nil(t, params["tokens"].(map[string]any)[tokenName])
		})
	}
}

func TestStatusDuration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name          string
		state         *PaneState
		daemonRunning bool
		want          time.Duration
		wantStale     bool
	}{
		{
			name:  "completed value",
			state: &PaneState{Completed: true, DurationMS: 42_000},
			want:  42 * time.Second,
		},
		{
			name:          "live value with a running daemon",
			state:         &PaneState{Running: true, TurnStartMS: now.Add(-90 * time.Second).UnixMilli()},
			daemonRunning: true,
			want:          90 * time.Second,
		},
		{
			name: "stale value stops at last seen",
			state: &PaneState{
				Running:     true,
				TurnStartMS: now.Add(-90 * time.Second).UnixMilli(),
				LastSeenMS:  now.Add(-60 * time.Second).UnixMilli(),
			},
			want:      30 * time.Second,
			wantStale: true,
		},
		{
			name: "stale value clamps an invalid last seen",
			state: &PaneState{
				Running:     true,
				TurnStartMS: now.UnixMilli(),
				LastSeenMS:  now.Add(-time.Minute).UnixMilli(),
			},
			want:      0,
			wantStale: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration, stale := statusDuration(tt.state, tt.daemonRunning, now)
			assert.Equal(t, tt.want, duration)
			assert.Equal(t, tt.wantStale, stale)
		})
	}
}
