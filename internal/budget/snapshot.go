package budget

import (
	"fmt"
	"time"

	"github.com/levee-ai/levee/pkg/types"
)

// BucketSnapshot is one rolling-window ring slot with committed usage.
type BucketSnapshot struct {
	EpochStart int64 `json:"epoch_start"`
	Amount     int64 `json:"amount"`
}

// BudgetSnapshot is the persisted committed usage of one budget window plus
// the identity fields Restore validates. Amounts are int64 in the window's
// own unit (tokens or microdollars). Reservations are never persisted. Which
// fields are populated depends on WindowType: a rolling window uses
// BucketCount and Buckets, a fixed window uses ResetAt, WindowStart, and
// Committed.
type BudgetSnapshot struct {
	Unit          string           `json:"unit"`
	WindowType    string           `json:"window_type"`
	WindowSeconds int64            `json:"window_seconds"`
	BucketCount   int              `json:"bucket_count,omitempty"`
	Buckets       []BucketSnapshot `json:"buckets,omitempty"`
	ResetAt       string           `json:"reset_at,omitempty"`
	WindowStart   time.Time        `json:"window_start,omitzero"`
	Committed     int64            `json:"committed,omitempty"`
}

// AgentSnapshot is the persisted state of one agent, budgets index-aligned
// with config.
type AgentSnapshot struct {
	Budgets []BudgetSnapshot `json:"budgets"`
}

// RestoreDiscard names one budget whose saved state was rejected and why.
type RestoreDiscard struct {
	Agent       string
	BudgetIndex int
	Field       string
}

// RestoreReport is what Restore did. The store takes no logger, the caller
// logs this (WARN per discard, one INFO for absent agents).
type RestoreReport struct {
	RestoredBudgets int
	Discards        []RestoreDiscard
	AbsentAgents    int
}

// Export copies committed usage for every agent. Lock protocol: agent
// pointers are collected under the map RLock, which is released BEFORE any
// agent lock is taken (the lookup ordering). The result may be torn across
// agents, which is fine, no cross-agent invariant exists. Reservations are
// deliberately absent (the persistence decision record is in the Session 8
// design document).
func (store *Store) Export() map[string]AgentSnapshot {
	type namedState struct {
		name  string
		state *agentBudgetState
	}
	store.mutex.RLock()
	states := make([]namedState, 0, len(store.agents))
	for name, state := range store.agents {
		states = append(states, namedState{name: name, state: state})
	}
	store.mutex.RUnlock()

	exported := make(map[string]AgentSnapshot, len(states))
	for _, entry := range states {
		entry.state.mutex.Lock()
		budgets := make([]BudgetSnapshot, len(entry.state.budgets))
		for i, window := range entry.state.budgets {
			budgets[i] = exportWindow(window)
		}
		entry.state.mutex.Unlock()
		exported[entry.name] = AgentSnapshot{Budgets: budgets}
	}
	return exported
}

// exportWindow copies one window's committed usage and identity. The caller
// holds the agent lock.
func exportWindow(window *budgetWindow) BudgetSnapshot {
	snapshot := BudgetSnapshot{
		Unit:          window.Unit,
		WindowType:    string(window.WindowType),
		WindowSeconds: int64(window.WindowSize.Seconds()),
	}
	if window.WindowType == types.WindowFixed {
		snapshot.ResetAt = fmt.Sprintf("%02d:%02dZ", window.resetHour, window.resetMinute)
		snapshot.WindowStart = window.windowStart
		snapshot.Committed = window.committedFixed
		return snapshot
	}
	snapshot.BucketCount = len(window.buckets)
	for _, bucket := range window.buckets {
		if bucket.Amount != 0 {
			snapshot.Buckets = append(snapshot.Buckets, BucketSnapshot(bucket))
		}
	}
	return snapshot
}

// Restore applies saved committed usage into identity-matching windows.
// PRECONDITION: the store is fresh (no traffic yet) and no listener is
// running. Restore is called once, in runServe, before the Snapshotter and
// the servers start. This precondition is enforced, not just documented: a
// second call on the same store returns a zero report and an error instead
// of mutating state, because re-applying a rolling window's saved buckets
// would sum them a second time on top of the first application's amounts
// (double-counting usage), and re-applying a fixed window would overwrite
// committedFixed with the stale saved value regardless of what a live agent
// accumulated since the first Restore.
func (store *Store) Restore(saved map[string]AgentSnapshot) (RestoreReport, error) {
	store.mutex.Lock()
	if store.restored {
		store.mutex.Unlock()
		return RestoreReport{}, fmt.Errorf("budget state already restored, Restore is once per store")
	}
	store.restored = true
	store.mutex.Unlock()

	report := RestoreReport{}
	for agentName, agentSnapshot := range saved {
		state, err := store.lookup(agentName)
		if err != nil {
			report.AbsentAgents++
			continue
		}
		state.mutex.Lock()
		for index, budgetSnapshot := range agentSnapshot.Budgets {
			if index >= len(state.budgets) {
				report.Discards = append(report.Discards, RestoreDiscard{
					Agent: agentName, BudgetIndex: index, Field: "budget_index"})
				continue
			}
			window := state.budgets[index]
			if field := mismatchedField(window, budgetSnapshot); field != "" {
				report.Discards = append(report.Discards, RestoreDiscard{
					Agent: agentName, BudgetIndex: index, Field: field})
				continue
			}
			restoreWindow(window, budgetSnapshot)
			report.RestoredBudgets++
		}
		state.mutex.Unlock()
	}
	return report, nil
}

// mismatchedField validates a saved budget against the configured window.
// Returns the name of the first identity field that does not match, or the
// empty string when every identity field matches (every mismatch has a field
// name, so a bool alongside it would be redundant).
func mismatchedField(window *budgetWindow, snapshot BudgetSnapshot) string {
	if window.Unit != snapshot.Unit {
		return "unit"
	}
	if string(window.WindowType) != snapshot.WindowType {
		return "window_type"
	}
	if int64(window.WindowSize.Seconds()) != snapshot.WindowSeconds {
		return "window_seconds"
	}
	if window.WindowType == types.WindowRolling && len(window.buckets) != snapshot.BucketCount {
		return "bucket_count"
	}
	if window.WindowType == types.WindowFixed {
		savedHour, savedMinute := parseResetAt(snapshot.ResetAt)
		if savedHour != window.resetHour || savedMinute != window.resetMinute {
			return "reset_at"
		}
	}
	return ""
}

// restoreWindow applies saved usage. The caller holds the agent lock and has
// validated identity. Rolling slot placement: slot is (epoch / width) modulo
// count. A slot collision SUMS saturating and keeps the newer epoch, never
// overwrites: dropping committed usage silently is the forbidden direction,
// while counting an old amount under a newer epoch only over-counts duration
// (the safe direction). Fixed windows run the boundary catch-up HERE,
// pre-traffic, so a long downtime's many-step catch-up loop never executes
// under the agent lock on the first live request.
func restoreWindow(window *budgetWindow, snapshot BudgetSnapshot) {
	if window.WindowType == types.WindowFixed {
		window.windowStart = snapshot.WindowStart.UTC()
		window.committedFixed = snapshot.Committed
		window.maybeReset()
		return
	}
	for _, bucket := range snapshot.Buckets {
		slot := (bucket.EpochStart / window.bucketWidthSec) % int64(len(window.buckets))
		existing := window.buckets[slot]
		merged := ringBucket(bucket)
		if existing.Amount != 0 {
			merged.Amount = saturatingAdd(existing.Amount, bucket.Amount)
			if existing.EpochStart > merged.EpochStart {
				merged.EpochStart = existing.EpochStart
			}
		}
		window.buckets[slot] = merged
	}
}
