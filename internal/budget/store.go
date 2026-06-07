package budget

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/levee-ai/levee/internal/config"
	"github.com/levee-ai/levee/pkg/types"
)

// defaultBucketCount is the number of sub-buckets per rolling window. Sixty
// buckets bound the trailing-edge over-count to 1/60th of the window.
const defaultBucketCount = 60

// reservedAmount is one budget's portion of a grouped reservation.
type reservedAmount struct {
	budgetIndex int
	amount      int64
}

// agentBudgetState is all per-agent budget state, guarded by its own mutex.
type agentBudgetState struct {
	mutex             sync.Mutex
	budgets           []*budgetWindow
	reservations      map[uint64][]reservedAmount
	nextReservationID uint64
}

// Store is the in-memory budget store. The map is guarded by mutex for lookup,
// each agent by its own mutex for the multi-field critical section.
type Store struct {
	mutex   sync.RWMutex
	agents  map[string]*agentBudgetState
	limiter *ConcurrencyLimiter
	now     clock
}

// NewStore builds a store from agent config. defaultStreamLimit is the per-agent
// concurrent-stream cap (project default 50). now is the clock (time.Now in
// production). Only enforce and observe agents get budget state. Passthrough
// agents are skipped (they have no budgets to track).
func NewStore(agents []config.AgentConfig, defaultStreamLimit int64, now clock) (*Store, error) {
	if now == nil {
		now = systemClock
	}
	agentMap := make(map[string]*agentBudgetState, len(agents))
	for _, agent := range agents {
		if agent.Mode == "passthrough" {
			continue
		}
		windows := make([]*budgetWindow, 0, len(agent.Budgets))
		for _, budget := range agent.Budgets {
			window, err := buildWindow(budget, now)
			if err != nil {
				return nil, fmt.Errorf("agent %q: %w", agent.Name, err)
			}
			windows = append(windows, window)
		}
		agentMap[agent.Name] = &agentBudgetState{
			budgets:      windows,
			reservations: make(map[uint64][]reservedAmount),
		}
	}
	return &Store{
		agents:  agentMap,
		limiter: NewConcurrencyLimiter(nil, defaultStreamLimit),
		now:     now,
	}, nil
}

// buildWindow converts one BudgetConfig into a budgetWindow. Token limits are
// integers, dollar limits are converted to integer cents.
func buildWindow(budget config.BudgetConfig, now clock) (*budgetWindow, error) {
	windowSize, err := time.ParseDuration(budget.Window)
	if err != nil {
		return nil, fmt.Errorf("budget window %q: %w", budget.Window, err)
	}
	limit := int64(budget.Limit)
	if budget.Type == "dollars" {
		limit = int64(math.Round(budget.Limit * 100))
	}
	if budget.WindowType == "fixed" {
		return newFixedWindow(limit, windowSize, budget.ResetAt, now), nil
	}
	return newRollingWindow(limit, windowSize, defaultBucketCount, now), nil
}

// lookup returns the agent state, releasing the map lock before the caller
// takes the agent lock (deadlock-safe ordering).
func (store *Store) lookup(agentName string) (*agentBudgetState, error) {
	store.mutex.RLock()
	state, ok := store.agents[agentName]
	store.mutex.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", agentName)
	}
	return state, nil
}

// Reserve checks the single-budget agent case. It is sugar over ReserveMulti
// with one amount applied to budget 0. Most callers (token-only) use this.
func (store *Store) Reserve(agentName string, estimatedTokens int64) (types.ReservationID, bool, error) {
	return store.ReserveMulti(agentName, []int64{estimatedTokens})
}

// ReserveMulti checks every budget against its estimate. All must fit, and a
// stream slot must be available, or nothing is reserved. amounts is indexed to
// match the agent's budgets slice (caller supplies tokens or cents per budget).
func (store *Store) ReserveMulti(agentName string, amounts []int64) (types.ReservationID, bool, error) {
	state, err := store.lookup(agentName)
	if err != nil {
		return 0, false, err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()

	if len(amounts) != len(state.budgets) {
		return 0, false, fmt.Errorf(
			"agent %q: got %d amounts, want %d budgets", agentName, len(amounts), len(state.budgets))
	}

	// Check every budget first. No mutation until all pass.
	for i, window := range state.budgets {
		if amounts[i] > window.remaining() {
			return 0, false, nil
		}
	}
	// Reserve a stream slot. If at the cap, reject without touching budgets.
	if !store.limiter.Acquire(agentName) {
		return 0, false, nil
	}
	// Commit the holds.
	held := make([]reservedAmount, 0, len(amounts))
	for i := range state.budgets {
		state.budgets[i].reserved += amounts[i]
		held = append(held, reservedAmount{budgetIndex: i, amount: amounts[i]})
	}
	state.nextReservationID++
	id := state.nextReservationID
	state.reservations[id] = held
	return types.ReservationID(id), true, nil
}

// Reconcile releases a reservation and commits the actual amount to budget 0's
// unit. For multi-budget agents, actuals beyond budget 0 are handled by the
// caller converting before Track in later sessions. For MVP the proxy uses
// single-budget Reconcile per the error-handling contract.
//
// PRECONDITION: budget index 0 is the token budget. actualTokens is committed
// to budgets[0] in token units. Any other budget (for example a dollars budget)
// keeps its reserved estimate, which over-counts in the safe direction until
// per-budget actuals arrive with dollar pricing (spec Section 7, Session 7).
// A configuration that lists a non-token budget first would commit a token
// count into the wrong unit. Multi-budget pricing MUST add a per-budget actuals
// path (a ReconcileMulti) rather than relying on this positional assumption.
func (store *Store) Reconcile(agentName string, reservationID types.ReservationID, actualTokens int64) error {
	state, err := store.lookup(agentName)
	if err != nil {
		return err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()

	held, ok := state.reservations[uint64(reservationID)]
	if !ok {
		return fmt.Errorf("agent %q: unknown reservation %d", agentName, reservationID)
	}
	for _, reservation := range held {
		window := state.budgets[reservation.budgetIndex]
		window.reserved -= reservation.amount
		// Commit actual. Budget 0 gets actualTokens, others get their estimate
		// (single-budget is the MVP path, multi-budget pricing arrives later).
		commitAmount := reservation.amount
		if reservation.budgetIndex == 0 {
			commitAmount = actualTokens
		}
		window.commit(commitAmount)
	}
	delete(state.reservations, uint64(reservationID))
	store.limiter.Release(agentName)
	return nil
}

// Forfeit releases a reservation and commits the full reserved estimate.
func (store *Store) Forfeit(agentName string, reservationID types.ReservationID) error {
	state, err := store.lookup(agentName)
	if err != nil {
		return err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()

	held, ok := state.reservations[uint64(reservationID)]
	if !ok {
		return fmt.Errorf("agent %q: unknown reservation %d", agentName, reservationID)
	}
	for _, reservation := range held {
		window := state.budgets[reservation.budgetIndex]
		window.reserved -= reservation.amount
		window.commit(reservation.amount)
	}
	delete(state.reservations, uint64(reservationID))
	store.limiter.Release(agentName)
	return nil
}

// Track commits an actual amount with no reservation. Used by observe-mode
// requests that exceeded budget (Reserve returned false) but were forwarded.
// Applies to budget 0 (the token budget) for the MVP single-budget path.
//
// PRECONDITION: budget index 0 is the token budget (same positional assumption
// as Reconcile). Multi-budget tracking arrives with dollar pricing (Session 7).
// The len check guards against a zero-budget state, which cannot occur for
// enforce or observe agents (config requires at least one budget) and which
// never reaches here for passthrough agents (they are absent from the map, so
// lookup returns an error first). It is defensive only.
func (store *Store) Track(agentName string, actualTokens int64) error {
	state, err := store.lookup(agentName)
	if err != nil {
		return err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if len(state.budgets) > 0 {
		state.budgets[0].commit(actualTokens)
	}
	return nil
}
