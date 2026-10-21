package main

import (
	"path/filepath"
	"testing"
	"time"
)

func observation(pane, agent, status string, seq uint64) AgentInfo {
	return AgentInfo{PaneID: pane, Agent: agent, AgentStatus: status, StateChangeSeq: seq}
}

func observeOne(t *testing.T, tracker *Tracker, agent AgentInfo, now time.Time) []Decision {
	t.Helper()
	decisions, _ := tracker.ObserveAll([]AgentInfo{agent}, now)
	return decisions
}

func TestWorkingTimerAndFormatting(t *testing.T) {
	tracker := NewTracker()
	start := time.Unix(1_700_000_000, 0)

	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	if len(decisions) != 1 {
		t.Fatalf("got %d decisions, want 1", len(decisions))
	}
	if decisions[0].Value != "0:00" || decisions[0].TTL != liveTTL {
		t.Fatalf("first decision = %+v, want value 0:00 and live TTL", decisions[0])
	}
	tracker.MarkReported("w1:p1", decisions[0].Value)

	decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start.Add(65*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "1:05" {
		t.Fatalf("after 65s decisions = %+v, want 1:05", decisions)
	}

	decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start.Add(3661*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "1:01:01" {
		t.Fatalf("after 1h1m1s decisions = %+v, want 1:01:01", decisions)
	}
}

func TestCompletionFreezesAndNewTurnResets(t *testing.T) {
	tracker := NewTracker()
	start := time.Unix(1_700_000_000, 0)

	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)

	decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusBlocked, 2), start.Add(10*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:10" {
		t.Fatalf("blocked decisions = %+v, want 0:10", decisions)
	}
	tracker.MarkReported("w1:p1", decisions[0].Value)

	decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 3), start.Add(20*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:20" || decisions[0].TTL != 0 {
		t.Fatalf("done decisions = %+v, want frozen 0:20", decisions)
	}
	if !tracker.MarkReported("w1:p1", decisions[0].Value) {
		t.Fatal("MarkReported should persist the pending completion")
	}

	if decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 3), start.Add(21*time.Second)); len(decisions) != 0 {
		t.Fatalf("done tick repeated decision: %+v", decisions)
	}
	if decisions = observeOne(t, tracker, observation("w1:p1", "omp", "idle", 4), start.Add(22*time.Second)); len(decisions) != 0 {
		t.Fatalf("idle after done repeated decision: %+v", decisions)
	}

	decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 5), start.Add(30*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:00" || decisions[0].TTL != liveTTL {
		t.Fatalf("new turn decisions = %+v, want fresh 0:00", decisions)
	}
	tracker.MarkReported("w1:p1", decisions[0].Value)
	decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 5), start.Add(35*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:05" {
		t.Fatalf("new turn after 5s decisions = %+v, want 0:05", decisions)
	}
}

func TestIdleWithoutWorkingShowsNothing(t *testing.T) {
	tracker := NewTracker()
	if decisions := observeOne(t, tracker, observation("w1:p1", "omp", "idle", 1), time.Unix(1_700_000_000, 0)); len(decisions) != 0 {
		t.Fatalf("got decisions for idle agent: %+v", decisions)
	}
	if decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusBlocked, 2), time.Unix(1_700_000_001, 0)); len(decisions) != 0 {
		t.Fatalf("got decisions for blocked agent with no turn: %+v", decisions)
	}
}

func TestAgentReplacementClearsOldToken(t *testing.T) {
	tracker := NewTracker()
	start := time.Unix(1_700_000_000, 0)
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)

	decisions = observeOne(t, tracker, observation("w1:p1", "claude", "idle", 1), start.Add(time.Second))
	if len(decisions) != 1 || !decisions[0].Clear {
		t.Fatalf("agent replacement decisions = %+v, want clear", decisions)
	}
	if !tracker.MarkCleared("w1:p1") {
		t.Fatal("MarkCleared should report a change")
	}
	if decisions = observeOne(t, tracker, observation("w1:p1", "claude", "idle", 1), start.Add(2*time.Second)); len(decisions) != 0 {
		t.Fatalf("replacement idle repeated decision: %+v", decisions)
	}
	decisions = observeOne(t, tracker, observation("w1:p1", "claude", statusWorking, 2), start.Add(3*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:00" {
		t.Fatalf("replacement working decisions = %+v, want fresh timer", decisions)
	}
}

func TestMissingPaneClearsAfterGrace(t *testing.T) {
	tracker := NewTracker()
	start := time.Unix(1_700_000_000, 0)
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)

	for i := 1; i <= 2; i++ {
		decisions, _ = tracker.ObserveAll(nil, start.Add(time.Duration(i)*time.Second))
		if len(decisions) != 0 {
			t.Fatalf("missing tick %d produced decision: %+v", i, decisions)
		}
	}
	decisions, changed := tracker.ObserveAll(nil, start.Add(3*time.Second))
	if len(decisions) != 1 || !decisions[0].Clear || !changed {
		t.Fatalf("third missing tick = %+v changed=%t, want clear", decisions, changed)
	}
	if _, ok := tracker.Panes["w1:p1"]; ok {
		t.Fatal("missing pane was not removed")
	}
}

func TestLoadCompletedReplaysToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	start := time.Unix(1_700_000_000, 0)
	tracker := NewTracker()
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)
	decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(42*time.Second))
	tracker.MarkReported("w1:p1", decisions[0].Value)
	if err := tracker.Save(path); err != nil {
		t.Fatal(err)
	}

	restored := NewTracker()
	if err := restored.Load(path); err != nil {
		t.Fatal(err)
	}
	decisions = observeOne(t, restored, observation("w1:p1", "omp", "idle", 3), start.Add(60*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:42" {
		t.Fatalf("restored completed decisions = %+v, want 0:42", decisions)
	}
}

func TestLoadRunningResumesOnlySameTurn(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)

	// Same sequence: keep the original start and include the downtime.
	tracker := NewTracker()
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := tracker.Save(path); err != nil {
		t.Fatal(err)
	}
	restored := NewTracker()
	if err := restored.Load(path); err != nil {
		t.Fatal(err)
	}
	decisions = observeOne(t, restored, observation("w1:p1", "omp", statusWorking, 7), start.Add(30*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:30" {
		t.Fatalf("resumed same turn = %+v, want 0:30", decisions)
	}

	// Different sequence: the agent changed while the daemon was down, so do
	// not carry the old start forward.
	tracker = NewTracker()
	decisions = observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)
	if err := tracker.Save(path); err != nil {
		t.Fatal(err)
	}
	restored = NewTracker()
	if err := restored.Load(path); err != nil {
		t.Fatal(err)
	}
	decisions = observeOne(t, restored, observation("w1:p1", "omp", statusWorking, 8), start.Add(30*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:00" {
		t.Fatalf("resumed changed turn = %+v, want fresh timer", decisions)
	}
}

func TestLoadRunningFrozenAtLastSeen(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	tracker := NewTracker()
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 7), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)
	tracker.Panes["w1:p1"].LastSeenMS = start.UnixMilli() + 12_000

	path := filepath.Join(t.TempDir(), "state.json")
	if err := tracker.Save(path); err != nil {
		t.Fatal(err)
	}
	restored := NewTracker()
	if err := restored.Load(path); err != nil {
		t.Fatal(err)
	}
	decisions = observeOne(t, restored, observation("w1:p1", "omp", "done", 8), start.Add(5*time.Minute))
	if len(decisions) != 1 || decisions[0].Value != "0:12" {
		t.Fatalf("frozen at last seen = %+v, want 0:12", decisions)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{-1, "0:00"},
		{0, "0:00"},
		{999, "0:00"},
		{1000, "0:01"},
		{59_999, "0:59"},
		{60_000, "1:00"},
		{3_599_000, "59:59"},
		{3_600_000, "1:00:00"},
		{3_661_000, "1:01:01"},
	}
	for _, test := range tests {
		if got := formatDuration(test.ms); got != test.want {
			t.Errorf("formatDuration(%d) = %q, want %q", test.ms, got, test.want)
		}
	}
}

func TestReinitializeReplaysCompletedToken(t *testing.T) {
	tracker := NewTracker()
	start := time.Unix(1_700_000_000, 0)
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)
	decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(9*time.Second))
	tracker.MarkReported("w1:p1", decisions[0].Value)

	if decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(10*time.Second)); len(decisions) != 0 {
		t.Fatalf("settled completion repeated: %+v", decisions)
	}
	tracker.Reinitialize()
	decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(11*time.Second))
	if len(decisions) != 1 || decisions[0].Value != "0:09" {
		t.Fatalf("reinitialized completion = %+v, want replayed 0:09", decisions)
	}
}

func TestLoadedTokenClearsWhenAgentChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	start := time.Unix(1_700_000_000, 0)
	tracker := NewTracker()
	decisions := observeOne(t, tracker, observation("w1:p1", "omp", statusWorking, 1), start)
	tracker.MarkReported("w1:p1", decisions[0].Value)
	decisions = observeOne(t, tracker, observation("w1:p1", "omp", "done", 2), start.Add(8*time.Second))
	tracker.MarkReported("w1:p1", decisions[0].Value)
	if err := tracker.Save(path); err != nil {
		t.Fatal(err)
	}

	restored := NewTracker()
	if err := restored.Load(path); err != nil {
		t.Fatal(err)
	}
	decisions = observeOne(t, restored, observation("w1:p1", "claude", "idle", 3), start.Add(9*time.Second))
	if len(decisions) != 1 || !decisions[0].Clear {
		t.Fatalf("loaded replacement decisions = %+v, want clear", decisions)
	}
}
