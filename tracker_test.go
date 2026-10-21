package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testStart = time.Unix(1_700_000_000, 0)

func observation(pane, agent, status string, seq uint64) AgentInfo {
	return AgentInfo{PaneID: pane, Agent: agent, AgentStatus: status, StateChangeSeq: seq}
}

func observeOne(t *testing.T, tracker *Tracker, agent AgentInfo, now time.Time) []Decision {
	t.Helper()
	decisions, _ := tracker.ObserveAll([]AgentInfo{agent}, now)
	return decisions
}

func report(t *testing.T, tracker *Tracker, decisions []Decision) {
	t.Helper()
	require.Len(t, decisions, 1)
	require.False(t, decisions[0].Clear)
	_ = tracker.MarkReported(decisions[0].PaneID, decisions[0].Value)
}

func TestObserveAllWorkingTurn(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		want   string
	}{
		{name: "starts at zero", offset: 0, want: "0:00"},
		{name: "formats minutes", offset: 65 * time.Second, want: "1:05"},
		{name: "formats hours", offset: time.Hour + time.Minute + time.Second, want: "1:01:01"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			decisions, changed := tracker.ObserveAll(
				[]AgentInfo{observation("w1:p1", "omp", statusWorking, 1)},
				testStart,
			)
			require.Len(t, decisions, 1)
			assert.True(t, changed)
			assert.Equal(t, Decision{PaneID: "w1:p1", Value: "0:00", TTL: liveTTL}, decisions[0])
			assert.False(t, tracker.MarkReported("w1:p1", decisions[0].Value))

			decisions, changed = tracker.ObserveAll(
				[]AgentInfo{observation("w1:p1", "omp", statusWorking, 1)},
				testStart.Add(tt.offset),
			)
			require.Len(t, decisions, 1)
			assert.False(t, changed)
			assert.Equal(t, Decision{PaneID: "w1:p1", Value: tt.want, TTL: liveTTL}, decisions[0])
		})
	}
}

func TestObserveAllBlockedKeepsTurnRunning(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		want   string
	}{
		{name: "counts ten seconds", offset: 10 * time.Second, want: "0:10"},
		{name: "counts through a wait", offset: 90 * time.Second, want: "1:30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), testStart))

			decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusBlocked, 2), testStart.Add(tt.offset))
			require.Len(t, decisions, 1)
			assert.Equal(t, Decision{PaneID: "w1:p1", Value: tt.want, TTL: liveTTL}, decisions[0])
		})
	}
}

func TestObserveAllCompletionFreezesTurn(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "done", status: "done"},
		{name: "idle", status: "idle"},
		{name: "unknown", status: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), testStart))

			decisions := observeOne(t, tracker, observation("w1:p1", "omp", tt.status, 2), testStart.Add(20*time.Second))
			require.Len(t, decisions, 1)
			assert.Equal(t, Decision{PaneID: "w1:p1", Value: "0:20"}, decisions[0])
			assert.True(t, tracker.MarkReported("w1:p1", decisions[0].Value))

			assert.Empty(t, observeOne(t, tracker, observation("w1:p1", "omp", tt.status, 2), testStart.Add(21*time.Second)))

			decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 3), testStart.Add(30*time.Second))
			require.Len(t, decisions, 1)
			assert.Equal(t, Decision{PaneID: "w1:p1", Value: "0:00", TTL: liveTTL}, decisions[0])
		})
	}
}

func TestObserveAllWithoutTurnShowsNothing(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "idle", status: "idle"},
		{name: "blocked", status: statusBlocked},
		{name: "done", status: "done"},
		{name: "unknown", status: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			assert.Empty(t, observeOne(t, tracker, observation("w1:p1", "omp", tt.status, 1), testStart))
		})
	}
}

func TestObserveAllAgentReplacementClearsToken(t *testing.T) {
	tests := []struct {
		name              string
		replacementStatus string
		wantNext          []Decision
	}{
		{
			name:              "idle replacement",
			replacementStatus: "idle",
		},
		{
			name:              "working replacement starts fresh",
			replacementStatus: statusWorking,
			wantNext:          []Decision{{PaneID: "w1:p1", Value: "0:00", TTL: liveTTL}},
		},
		{
			name:              "done replacement stays quiet",
			replacementStatus: "done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), testStart))

			decisions := observeOne(t, tracker, observation("w1:p1", "claude", tt.replacementStatus, 1), testStart.Add(time.Second))
			require.Len(t, decisions, 1)
			assert.True(t, decisions[0].Clear)
			assert.True(t, tracker.MarkCleared("w1:p1"))

			decisions = observeOne(t, tracker, observation("w1:p1", "claude", tt.replacementStatus, 1), testStart.Add(2*time.Second))
			assert.Equal(t, tt.wantNext, decisions)
		})
	}
}

func TestObserveAllMissingPaneGrace(t *testing.T) {
	tests := []struct {
		name      string
		misses    int
		wantClear bool
	}{
		{name: "first miss keeps pane", misses: 1},
		{name: "second miss keeps pane", misses: 2},
		{name: "third miss clears pane", misses: 3, wantClear: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), testStart))

			var decisions []Decision
			var changed bool
			for i := range tt.misses {
				decisions, changed = tracker.ObserveAll(nil, testStart.Add(time.Duration(i+1)*time.Second))
				if i < tt.misses-1 {
					assert.Empty(t, decisions)
					assert.False(t, changed)
				}
			}

			if !tt.wantClear {
				assert.Empty(t, decisions)
				assert.False(t, changed)
				assert.Contains(t, tracker.Panes, "w1:p1")
				return
			}

			require.Len(t, decisions, 1)
			assert.True(t, decisions[0].Clear)
			assert.True(t, changed)
			assert.NotContains(t, tracker.Panes, "w1:p1")
		})
	}
}

func TestObserveAllMultipleAgents(t *testing.T) {
	tests := []struct {
		name   string
		agents []AgentInfo
		want   map[string]string
	}{
		{
			name: "writes every working pane",
			agents: []AgentInfo{
				observation("w1:p1", "omp", statusWorking, 1),
				observation("w1:p2", "claude", statusWorking, 1),
			},
			want: map[string]string{"w1:p1": "0:00", "w1:p2": "0:00"},
		},
		{
			name: "skips panes without a turn and empty ids",
			agents: []AgentInfo{
				observation("w1:p1", "omp", statusWorking, 1),
				observation("w1:p2", "claude", "idle", 1),
				observation("", "ignored", statusWorking, 1),
			},
			want: map[string]string{"w1:p1": "0:00"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			decisions, _ := tracker.ObserveAll(tt.agents, testStart)
			got := make(map[string]string, len(decisions))
			for _, decision := range decisions {
				got[decision.PaneID] = decision.Value
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTrackerPersistence(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, tracker *Tracker, start time.Time)
		action    func(t *testing.T, tracker *Tracker, start time.Time) []Decision
		want      []Decision
		wantPanes int
	}{
		{
			name: "replays a completed value",
			setup: func(t *testing.T, tracker *Tracker, start time.Time) {
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start))
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(42*time.Second)))
			},
			action: func(t *testing.T, tracker *Tracker, start time.Time) []Decision {
				return observeOne(t, tracker, observation("w1:p1", "omp", "idle", 3), start.Add(60*time.Second))
			},
			want:      []Decision{{PaneID: "w1:p1", Value: "0:42"}},
			wantPanes: 1,
		},
		{
			name: "resumes the same running turn",
			setup: func(t *testing.T, tracker *Tracker, start time.Time) {
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start))
			},
			action: func(t *testing.T, tracker *Tracker, start time.Time) []Decision {
				return observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start.Add(30*time.Second))
			},
			want:      []Decision{{PaneID: "w1:p1", Value: "0:30", TTL: liveTTL}},
			wantPanes: 1,
		},
		{
			name: "starts fresh when the sequence changed",
			setup: func(t *testing.T, tracker *Tracker, start time.Time) {
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start))
			},
			action: func(t *testing.T, tracker *Tracker, start time.Time) []Decision {
				return observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 8), start.Add(30*time.Second))
			},
			want:      []Decision{{PaneID: "w1:p1", Value: "0:00", TTL: liveTTL}},
			wantPanes: 1,
		},
		{
			name: "freezes at the last observed time",
			setup: func(t *testing.T, tracker *Tracker, start time.Time) {
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start))
				tracker.Panes["w1:p1"].LastSeenMS = start.Add(12 * time.Second).UnixMilli()
			},
			action: func(t *testing.T, tracker *Tracker, start time.Time) []Decision {
				return observeOne(t, tracker, observation("w1:p1", "omp", "done", 8), start.Add(5*time.Minute))
			},
			want:      []Decision{{PaneID: "w1:p1", Value: "0:12"}},
			wantPanes: 1,
		},
		{
			name: "clears a loaded token when the agent changes",
			setup: func(t *testing.T, tracker *Tracker, start time.Time) {
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start))
				report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(8*time.Second)))
			},
			action: func(t *testing.T, tracker *Tracker, start time.Time) []Decision {
				return observeOne(t, tracker, observation("w1:p1", "claude", "idle", 3), start.Add(9*time.Second))
			},
			want:      []Decision{{PaneID: "w1:p1", Clear: true}},
			wantPanes: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			tracker := NewTracker()
			tt.setup(t, tracker, testStart)
			require.NoError(t, tracker.Save(path))

			restored := NewTracker()
			require.NoError(t, restored.Load(path))
			assert.Equal(t, tt.want, tt.action(t, restored, testStart))
			assert.Len(t, restored.Panes, tt.wantPanes)
		})
	}
}

func TestTrackerLoad(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		write     bool
		wantErr   bool
		wantPanes int
	}{
		{name: "missing file is empty state"},
		{name: "invalid JSON fails", content: "{", write: true, wantErr: true},
		{name: "empty state file is valid", content: `{"version":1}`, write: true},
		{name: "rejects an unsupported version", content: `{"version":2}`, write: true, wantErr: true},
		{
			name:      "restores panes",
			content:   `{"version":1,"panes":{"w1:p1":{"agent":"omp","completed":true,"duration_ms":42000}}}`,
			write:     true,
			wantPanes: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if tt.write {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			}
			tracker := NewTracker()
			err := tracker.Load(path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, tracker.Panes, tt.wantPanes)
		})
	}
}

func TestTrackerReinitialize(t *testing.T) {
	tests := []struct {
		name         string
		reinitialize bool
		want         []Decision
	}{
		{name: "settled completion stays quiet"},
		{
			name:         "reinitialize replays completion",
			reinitialize: true,
			want:         []Decision{{PaneID: "w1:p1", Value: "0:09"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker()
			report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), testStart))
			report(t, tracker, observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), testStart.Add(9*time.Second)))
			assert.Empty(t, observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), testStart.Add(10*time.Second)))

			if tt.reinitialize {
				tracker.Reinitialize()
			}
			assert.Equal(t, tt.want, observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), testStart.Add(11*time.Second)))
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		ms   int64
		want string
	}{
		{name: "negative clamps", ms: -1, want: "0:00"},
		{name: "zero", ms: 0, want: "0:00"},
		{name: "sub-second", ms: 999, want: "0:00"},
		{name: "one second", ms: 1000, want: "0:01"},
		{name: "fifty nine seconds", ms: 59_999, want: "0:59"},
		{name: "one minute", ms: 60_000, want: "1:00"},
		{name: "just under an hour", ms: 3_599_000, want: "59:59"},
		{name: "one hour", ms: 3_600_000, want: "1:00:00"},
		{name: "hours minutes seconds", ms: 3_661_000, want: "1:01:01"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatDuration(tt.ms))
		})
	}
}
