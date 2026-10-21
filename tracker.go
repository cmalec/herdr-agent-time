package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const (
	statusWorking = "working"
	statusBlocked = "blocked"

	liveTTL = 3 * time.Second
)

// AgentInfo is the subset of herdr's agent record the timer needs.
type AgentInfo struct {
	PaneID         string `json:"pane_id"`
	Agent          string `json:"agent"`
	AgentStatus    string `json:"agent_status"`
	StateChangeSeq uint64 `json:"state_change_seq"`
}

// PaneState is the timer state for one pane. The fields persisted to disk are
// exactly the ones needed to resume after a daemon or Herdr server restart.
type PaneState struct {
	Agent         string `json:"agent"`
	Status        string `json:"status"`
	Seq           uint64 `json:"seq"`
	Running       bool   `json:"running"`
	Completed     bool   `json:"completed"`
	TurnStartMS   int64  `json:"turn_start_ms"`
	DurationMS    int64  `json:"duration_ms"`
	LastSeenMS    int64  `json:"last_seen_ms"`
	HadToken      bool   `json:"had_token"`
	ReportPending bool   `json:"report_pending"`
	Shown         string `json:"shown,omitempty"`
	Missing       int    `json:"missing,omitempty"`

	// Initialized is runtime-only. A state loaded from disk starts false so the
	// first observation reconciles it with the live agent status.
	Initialized bool `json:"-"`
}

type stateFile struct {
	Version int                   `json:"version"`
	Panes   map[string]*PaneState `json:"panes"`
}

type Tracker struct {
	Panes map[string]*PaneState
}

func NewTracker() *Tracker {
	return &Tracker{Panes: make(map[string]*PaneState)}
}

// Load restores timer state. A completed value is kept, but ReportPending is
// set because Herdr does not restore metadata tokens after a server restart.
func (t *Tracker) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	if file.Panes == nil {
		return nil
	}
	for paneID, st := range file.Panes {
		if st == nil {
			continue
		}
		st.Initialized = false
		// A token may still exist on a live server even though the daemon
		// restarted. Keep the marker so a changed or vanished agent clears it.
		st.HadToken = st.HadToken || st.Completed || st.Running
		st.Shown = ""
		if st.Running && st.TurnStartMS <= 0 {
			st.Running = false
			st.Completed = false
		}
		if st.DurationMS < 0 {
			st.DurationMS = 0
		}
		st.ReportPending = st.Completed
		t.Panes[paneID] = st
	}
	return nil
}

func (t *Tracker) Save(path string) error {
	file := stateFile{Version: 1, Panes: t.Panes}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Reinitialize makes the next observation reconcile every pane with the live
// agent record. Call it after reconnecting to a restarted Herdr server.
func (t *Tracker) Reinitialize() {
	for _, st := range t.Panes {
		st.Initialized = false
	}
}

type Decision struct {
	PaneID string
	Value  string
	TTL    time.Duration
	Clear  bool
}

// ObserveAll updates the timer from one agent.list snapshot and returns the
// metadata changes to send. A pane is removed only after three consecutive
// snapshots omit it, so a brief server-restore gap does not drop its timer.
func (t *Tracker) ObserveAll(agents []AgentInfo, now time.Time) ([]Decision, bool) {
	nowMS := now.UnixMilli()
	live := make(map[string]struct{}, len(agents))
	var decisions []Decision
	changed := false

	for _, agent := range agents {
		if agent.PaneID == "" {
			continue
		}
		live[agent.PaneID] = struct{}{}
		decision, stateChanged := t.observe(agent, nowMS)
		if decision != nil {
			decisions = append(decisions, *decision)
		}
		if stateChanged {
			changed = true
		}
	}

	for paneID, st := range t.Panes {
		if _, ok := live[paneID]; ok {
			continue
		}
		st.Missing++
		if st.Missing < 3 {
			continue
		}
		if st.HadToken {
			decisions = append(decisions, Decision{PaneID: paneID, Clear: true})
		}
		delete(t.Panes, paneID)
		changed = true
	}

	return decisions, changed
}

func (t *Tracker) observe(agent AgentInfo, nowMS int64) (*Decision, bool) {
	st, exists := t.Panes[agent.PaneID]
	changed := false
	if !exists {
		st = &PaneState{Agent: agent.Agent}
		t.Panes[agent.PaneID] = st
		changed = true
	}
	st.Missing = 0

	if st.Agent != agent.Agent {
		hadToken := st.HadToken
		st.Agent = agent.Agent
		st.Status = ""
		st.Seq = 0
		st.Running = false
		st.Completed = false
		st.TurnStartMS = 0
		st.DurationMS = 0
		st.ReportPending = false
		st.HadToken = hadToken
		st.Shown = ""
		st.Initialized = false
		changed = true
		if hadToken {
			return &Decision{PaneID: agent.PaneID, Clear: true}, true
		}
	}

	if !st.Initialized {
		st.Initialized = true
		changed = true
		switch {
		case st.Completed:
			// The final value belongs to the sidebar across server restarts,
			// but the token itself was lost, so report it again.
			st.ReportPending = true
		case st.Running:
			switch {
			case agent.AgentStatus == statusWorking && agent.StateChangeSeq == st.Seq:
				// Same turn, same observed transition sequence: keep the
				// original start so downtime is reflected in the runtime.
			case agent.AgentStatus == statusWorking:
				// The agent changed state while we were disconnected. Start a
				// fresh turn instead of carrying an unknown gap forward.
				st.TurnStartMS = nowMS
				st.Completed = false
				st.ReportPending = false
			case agent.AgentStatus == statusBlocked:
				// A blocked agent can be in the middle of a turn; keep the
				// original start because there is no completed turn to keep.
			default:
				// The turn ended while we were away. Freeze at the last time
				// it was observed running rather than counting downtime.
				st.Running = false
				st.Completed = true
				end := st.LastSeenMS
				if end < st.TurnStartMS {
					end = nowMS
				}
				st.DurationMS = end - st.TurnStartMS
				if st.DurationMS < 0 {
					st.DurationMS = 0
				}
				st.ReportPending = true
			}
		}
	}

	switch agent.AgentStatus {
	case statusWorking:
		if !st.Running {
			st.Running = true
			st.Completed = false
			st.TurnStartMS = nowMS
			st.DurationMS = 0
			st.ReportPending = false
			changed = true
		}
	case statusBlocked:
		// Keep a running turn counting through the wait. A pane that becomes
		// blocked before any working observation has no turn to time.
	default:
		if st.Running {
			st.Running = false
			st.Completed = true
			st.DurationMS = nowMS - st.TurnStartMS
			if st.DurationMS < 0 {
				st.DurationMS = 0
			}
			st.ReportPending = true
			changed = true
		}
	}

	if st.Status != agent.AgentStatus || st.Seq != agent.StateChangeSeq {
		st.Status = agent.AgentStatus
		st.Seq = agent.StateChangeSeq
		changed = true
	}
	st.LastSeenMS = nowMS

	switch {
	case st.Running:
		st.HadToken = true
		return &Decision{
			PaneID: agent.PaneID,
			Value:  formatDuration(nowMS - st.TurnStartMS),
			TTL:    liveTTL,
		}, changed
	case st.Completed && st.ReportPending:
		return &Decision{
			PaneID: agent.PaneID,
			Value:  formatDuration(st.DurationMS),
		}, changed
	case !st.Running && !st.Completed && st.HadToken:
		return &Decision{PaneID: agent.PaneID, Clear: true}, changed
	}
	return nil, changed
}

// MarkReported records a successful metadata write. It returns true when the
// change needs to be persisted.
func (t *Tracker) MarkReported(paneID, value string) bool {
	st, ok := t.Panes[paneID]
	if !ok {
		return false
	}
	st.HadToken = true
	st.Shown = value
	if st.Running {
		return false
	}
	if st.ReportPending {
		st.ReportPending = false
		return true
	}
	return false
}

// MarkCleared records a successful token removal.
func (t *Tracker) MarkCleared(paneID string) bool {
	st, ok := t.Panes[paneID]
	if !ok {
		return false
	}
	if !st.HadToken {
		return false
	}
	st.HadToken = false
	st.Shown = ""
	return true
}

func formatDuration(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	seconds := ms / 1000
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%d:%02d", minutes, secs)
}
