package main

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type reportCall struct {
	PaneID string
	Value  string
	TTL    time.Duration
}

type fakeClient struct {
	agents      []AgentInfo
	agentErr    error
	plugins     []pluginInfo
	pluginErr   error
	reports     []reportCall
	clears      []string
	closed      int
	pluginCalls int
}

func (f *fakeClient) ListAgents(context.Context) ([]AgentInfo, error) {
	return f.agents, f.agentErr
}

func (f *fakeClient) ListPlugins(context.Context) ([]pluginInfo, error) {
	f.pluginCalls++
	return f.plugins, f.pluginErr
}

func (f *fakeClient) Report(_ context.Context, paneID, value string, ttl time.Duration) error {
	f.reports = append(f.reports, reportCall{PaneID: paneID, Value: value, TTL: ttl})
	return nil
}

func (f *fakeClient) Clear(_ context.Context, paneID string) error {
	f.clears = append(f.clears, paneID)
	return nil
}

func (f *fakeClient) Close() error {
	f.closed++
	return nil
}

func testDaemon(t *testing.T) (*daemon, *fakeClient) {
	t.Helper()
	dir := t.TempDir()
	p := paths{
		dir:      dir,
		socket:   filepath.Join(dir, "herdr.sock"),
		lock:     filepath.Join(dir, "daemon.lock"),
		pid:      filepath.Join(dir, "daemon.pid"),
		identity: filepath.Join(dir, "daemon.json"),
		state:    filepath.Join(dir, "state.json"),
		log:      filepath.Join(dir, "daemon.log"),
	}
	tracker := NewTracker()
	client := &fakeClient{}
	d := newDaemon(p, tracker, client, log.New(io.Discard, "", 0))
	d.now = func() time.Time { return testStart }
	d.inode = func(string) (uint64, error) { return 1, nil }
	d.save = tracker.Save
	d.watchPlugin = false
	d.pluginCheckPeriod = time.Hour
	d.lastInode = 1
	d.lastSave = testStart
	return d, client
}

func TestDaemonReportsDecisions(t *testing.T) {
	tests := []struct {
		name        string
		agents      []AgentInfo
		now         time.Time
		setup       func(*daemon)
		wantReports []reportCall
		wantClears  []string
	}{
		{
			name:        "reports a live turn",
			agents:      []AgentInfo{observation("w1:p1", "omp", statusWorking, 1)},
			now:         testStart,
			wantReports: []reportCall{{PaneID: "w1:p1", Value: "0:00", TTL: liveTTL}},
		},
		{
			name:   "reports completion without a ttl",
			agents: []AgentInfo{observation("w1:p1", "omp", "done", 2)},
			now:    testStart.Add(9 * time.Second),
			setup: func(d *daemon) {
				d.tracker.Panes["w1:p1"] = &PaneState{
					Agent:       "omp",
					Status:      statusWorking,
					Seq:         1,
					Running:     true,
					TurnStartMS: testStart.UnixMilli(),
					HadToken:    true,
				}
			},
			wantReports: []reportCall{{PaneID: "w1:p1", Value: "0:09"}},
		},
		{
			name:   "clears a token when the agent changes",
			agents: []AgentInfo{observation("w1:p1", "claude", "idle", 2)},
			now:    testStart.Add(time.Second),
			setup: func(d *daemon) {
				d.tracker.Panes["w1:p1"] = &PaneState{
					Agent:     "omp",
					Status:    "done",
					Seq:       1,
					Completed: true,
					HadToken:  true,
				}
			},
			wantClears: []string{"w1:p1"},
		},
		{
			name:   "clears a token when the agent has no turn",
			agents: []AgentInfo{observation("w1:p1", "omp", "idle", 1)},
			now:    testStart,
			setup: func(d *daemon) {
				d.tracker.Panes["w1:p1"] = &PaneState{
					Agent:       "omp",
					Status:      "idle",
					Seq:         1,
					HadToken:    true,
					Initialized: true,
				}
			},
			wantClears: []string{"w1:p1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := testDaemon(t)
			client.agents = tt.agents
			if tt.setup != nil {
				tt.setup(d)
			}
			require.Nil(t, d.tick(context.Background(), tt.now))
			assert.Equal(t, tt.wantReports, client.reports)
			assert.Equal(t, tt.wantClears, client.clears)
		})
	}
}

func TestDaemonSaveBackoff(t *testing.T) {
	tests := []struct {
		name         string
		times        []time.Duration
		failures     int
		wantAttempts int
	}{
		{
			name:         "retries after the backoff window",
			times:        []time.Duration{11 * time.Second, 12 * time.Second, 42 * time.Second},
			failures:     100,
			wantAttempts: 2,
		},
		{
			name:         "backs off after a failure",
			times:        []time.Duration{11 * time.Second, 12 * time.Second, 20 * time.Second},
			failures:     100,
			wantAttempts: 1,
		},
		{
			name:         "resets after a successful save",
			times:        []time.Duration{11 * time.Second, 42 * time.Second, 53 * time.Second},
			failures:     1,
			wantAttempts: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := testDaemon(t)
			client.agents = []AgentInfo{observation("w1:p1", "omp", statusWorking, 1)}
			d.saveInterval = 10 * time.Second
			d.saveRetryInterval = 30 * time.Second
			attempts := 0
			d.save = func(string) error {
				attempts++
				if attempts <= tt.failures {
					return errors.New("disk full")
				}
				return nil
			}
			for _, offset := range tt.times {
				require.Nil(t, d.tick(context.Background(), testStart.Add(offset)))
			}
			assert.Equal(t, tt.wantAttempts, attempts)
		})
	}
}

func TestDaemonIdleSaveSkipped(t *testing.T) {
	tests := []struct {
		name      string
		dirty     bool
		wantSaves int
	}{
		{name: "idle state does not save"},
		{name: "dirty state saves", dirty: true, wantSaves: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := testDaemon(t)
			d.dirty = tt.dirty
			d.tracker.Panes["w1:p1"] = &PaneState{Agent: "omp", Status: "idle", Seq: 1, Initialized: true}
			saves := 0
			d.save = func(string) error {
				saves++
				return nil
			}
			d.saveIfDue(testStart.Add(11 * time.Second))
			assert.Equal(t, tt.wantSaves, saves)
		})
	}
}

func TestDaemonPluginWatchdog(t *testing.T) {
	installed := []pluginInfo{{PluginID: pluginID, Enabled: true, PluginRoot: "/plugin"}}
	tests := []struct {
		name       string
		watch      bool
		plugins    []pluginInfo
		pluginErr  error
		pluginRoot string
		ticks      int
		wantExit   bool
		wantClear  bool
		wantRemove bool
		wantReason string
		wantCalls  int
	}{
		{
			name:       "keeps running when installed and enabled",
			watch:      true,
			plugins:    installed,
			pluginRoot: "/plugin",
			ticks:      2,
			wantCalls:  2,
		},
		{
			name:       "exits when disabled",
			watch:      true,
			plugins:    []pluginInfo{{PluginID: pluginID, Enabled: false, PluginRoot: "/plugin"}},
			pluginRoot: "/plugin",
			ticks:      1,
			wantExit:   true,
			wantClear:  true,
			wantRemove: true,
			wantReason: "disabled",
			wantCalls:  1,
		},
		{
			name:       "exits after two missing results",
			watch:      true,
			pluginRoot: "/plugin",
			ticks:      2,
			wantExit:   true,
			wantClear:  true,
			wantRemove: true,
			wantReason: "no longer installed",
			wantCalls:  2,
		},
		{
			name:       "exits when the installation changes",
			watch:      true,
			plugins:    []pluginInfo{{PluginID: pluginID, Enabled: true, PluginRoot: "/new"}},
			pluginRoot: "/old",
			ticks:      1,
			wantExit:   true,
			wantReason: "installation changed",
			wantCalls:  1,
		},
		{
			name:       "ignores a plugin.list error",
			watch:      true,
			pluginErr:  errors.New("temporarily unavailable"),
			pluginRoot: "/plugin",
			ticks:      2,
			wantCalls:  2,
		},
		{
			name:  "does not watch manual runs",
			ticks: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := testDaemon(t)
			d.watchPlugin = tt.watch
			d.pluginCheckPeriod = 0
			d.pluginRoot = tt.pluginRoot
			client := d.client.(*fakeClient)
			client.plugins = tt.plugins
			client.pluginErr = tt.pluginErr
			var exit *daemonExit
			for index := range tt.ticks {
				exit = d.tick(context.Background(), testStart.Add(time.Duration(index)*time.Second))
				if exit != nil {
					break
				}
			}
			if !tt.wantExit {
				assert.Nil(t, exit)
			} else {
				require.NotNil(t, exit)
				assert.Equal(t, tt.wantClear, exit.clear)
				assert.Equal(t, tt.wantRemove, exit.removeState)
				assert.Contains(t, exit.reason, tt.wantReason)
			}
			assert.Equal(t, tt.wantCalls, client.pluginCalls)
		})
	}
}

func TestDaemonInodeChangeReconnects(t *testing.T) {
	tests := []struct {
		name       string
		lastInode  uint64
		nextInode  uint64
		wantClosed int
	}{
		{name: "closes on a change", lastInode: 1, nextInode: 2, wantClosed: 1},
		{name: "keeps the connection when unchanged", lastInode: 1, nextInode: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := testDaemon(t)
			d.lastInode = tt.lastInode
			d.inode = func(string) (uint64, error) { return tt.nextInode, nil }
			d.tracker.Panes["w1:p1"] = &PaneState{Agent: "omp", Initialized: true}
			require.Nil(t, d.tick(context.Background(), testStart))
			assert.Equal(t, tt.wantClosed, client.closed)
			if tt.wantClosed > 0 {
				assert.False(t, d.tracker.Panes["w1:p1"].Initialized)
			}
		})
	}
}

func TestDaemonListFailureLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		ticks     int
		wantExit  bool
		wantClear bool
	}{
		{name: "stays below the limit", limit: 3, ticks: 2},
		{name: "exits at the limit", limit: 2, ticks: 2, wantExit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := testDaemon(t)
			d.listFailureLimit = tt.limit
			client.agentErr = errors.New("socket unavailable")
			var exit *daemonExit
			for index := range tt.ticks {
				exit = d.tick(context.Background(), testStart.Add(time.Duration(index)*time.Second))
			}
			if !tt.wantExit {
				assert.Nil(t, exit)
				return
			}
			require.NotNil(t, exit)
			assert.False(t, exit.clear)
			assert.Contains(t, exit.reason, "agent.list failed")
		})
	}
}

func TestDaemonShutdownClearsTokens(t *testing.T) {
	tests := []struct {
		name       string
		exit       daemonExit
		wantClears []string
		wantState  bool
	}{
		{
			name:       "context shutdown clears and saves",
			exit:       daemonExit{clear: true},
			wantClears: []string{"w1:p1"},
			wantState:  true,
		},
		{
			name:       "uninstall clears and removes state",
			exit:       daemonExit{clear: true, removeState: true},
			wantClears: []string{"w1:p1"},
		},
		{
			name:      "installation change keeps tokens and state",
			exit:      daemonExit{},
			wantState: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := testDaemon(t)
			d.tracker.Panes["w1:p1"] = &PaneState{Agent: "omp", Completed: true, HadToken: true}
			require.NoError(t, os.WriteFile(d.paths.state, []byte(`{"version":1}`), 0o600))
			require.NoError(t, d.finish(context.Background(), tt.exit))
			assert.Equal(t, tt.wantClears, client.clears)
			_, err := os.Stat(d.paths.state)
			assert.Equal(t, tt.wantState, err == nil)
		})
	}
}

func TestDaemonRunStops(t *testing.T) {
	tests := []struct {
		name string
		run  func(*daemon) error
	}{
		{
			name: "canceled context",
			run: func(d *daemon) error {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return d.run(ctx, make(chan time.Time))
			},
		},
		{
			name: "closed tick channel",
			run: func(d *daemon) error {
				ticks := make(chan time.Time)
				close(ticks)
				return d.run(context.Background(), ticks)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, client := testDaemon(t)
			d.tracker.Panes["w1:p1"] = &PaneState{Agent: "omp", Completed: true, HadToken: true}
			require.NoError(t, tt.run(d))
			assert.Equal(t, []string{"w1:p1"}, client.clears)
		})
	}
}
