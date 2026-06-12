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

// RejectReason explains why Admit declined a request.
type RejectReason int

const (
	RejectNone        RejectReason = iota // admitted
	RejectBudget                          // a budget did not fit
	RejectConcurrency                     // no stream slot available
)

// BudgetStatus is a point-in-time snapshot of one budget, built under the agent
// lock for the 429 response body.
type BudgetStatus struct {
	Type      string
	Limit     int64
	Used      int64
	Remaining int64
	ResetAt   time.Time
}

// Outcome is the full result of an Admit call.
type Outcome struct {
	ID       types.ReservationID
	Admitted bool
	Reason   RejectReason
	Binding  *BudgetStatus // set only when Reason is RejectBudget
}

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
// integers, dollar limits are converted to integer microdollars.
func buildWindow(budget config.BudgetConfig, now clock) (*budgetWindow, error) {
	windowSize, err := time.ParseDuration(budget.Window)
	if err != nil {
		return nil, fmt.Errorf("budget window %q: %w", budget.Window, err)
	}
	limit := int64(budget.Limit)
	if budget.Type == "dollars" {
		// Microdollars (1e-6 USD). Integer cents under-counts sub-cent requests to
		// zero (probe-verified), so the finer unit is required for correctness.
		// math.Round(limit * 1e6) is exact for any 2-decimal limit (probe-verified).
		limit = int64(math.Round(budget.Limit * 1e6))
	}
	if budget.WindowType == "fixed" {
		window := newFixedWindow(limit, windowSize, budget.ResetAt, now)
		window.Unit = budget.Type
		return window, nil
	}
	window := newRollingWindow(limit, windowSize, defaultBucketCount, now)
	window.Unit = budget.Type
	return window, nil
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

// Admit checks every budget and a stream slot atomically, returning a structured
// Outcome. On success it creates a reservation (same effect as ReserveMulti). On
// a budget miss it reports RejectBudget with the binding budget (the failing one
// with the longest recovery time, so a derived Retry-After never under-states the
// wait). On a full stream slot it reports RejectConcurrency with no Binding.
// amounts is index-aligned with the agent's configured budgets.
func (store *Store) Admit(agentName string, amounts []int64) (Outcome, error) {
	state, err := store.lookup(agentName)
	if err != nil {
		return Outcome{}, err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()

	if len(amounts) != len(state.budgets) {
		return Outcome{}, fmt.Errorf(
			"agent %q: got %d amounts, want %d budgets", agentName, len(amounts), len(state.budgets))
	}

	// Check every budget. Collect the binding (longest-recovery) failure rather
	// than returning on the first miss, so the reported reset is honest when more
	// than one budget is exhausted. A negative amount is always a miss: it would
	// otherwise pass the remaining check (no negative is greater than a
	// non-negative remaining) and then a negative reserved delta would inflate the
	// available budget. A money-protection store must never treat a negative
	// request as fitting, independent of how the amount was produced. Zero is a
	// valid amount (a dollars budget reserves zero tokens) and still fits.
	var binding *BudgetStatus
	for i, window := range state.budgets {
		remaining := window.remaining()
		if amounts[i] < 0 || amounts[i] > remaining {
			candidate := BudgetStatus{
				Type:      window.Unit,
				Limit:     window.Limit,
				Used:      window.used(),
				Remaining: remaining,
				ResetAt:   window.recoveryTime(amounts[i]),
			}
			if binding == nil || candidate.ResetAt.After(binding.ResetAt) {
				snapshot := candidate
				binding = &snapshot
			}
		}
	}
	if binding != nil {
		return Outcome{Admitted: false, Reason: RejectBudget, Binding: binding}, nil
	}

	// Budgets all fit. Try the stream slot.
	if !store.limiter.Acquire(agentName) {
		return Outcome{Admitted: false, Reason: RejectConcurrency}, nil
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
	return Outcome{ID: types.ReservationID(id), Admitted: true, Reason: RejectNone}, nil
}

// Reserve checks the single-budget agent case. It is sugar over ReserveMulti
// with one amount applied to budget 0. Most callers (token-only) use this.
func (store *Store) Reserve(agentName string, estimatedTokens int64) (types.ReservationID, bool, error) {
	return store.ReserveMulti(agentName, []int64{estimatedTokens})
}

// ReserveMulti checks every budget against its estimate. All must fit, and a
// stream slot must be available, or nothing is reserved. amounts is indexed to
// match the agent's budgets slice (caller supplies tokens or microdollars per budget).
func (store *Store) ReserveMulti(agentName string, amounts []int64) (types.ReservationID, bool, error) {
	outcome, err := store.Admit(agentName, amounts)
	if err != nil {
		return 0, false, err
	}
	return outcome.ID, outcome.Admitted, nil
}

// Reconcile releases a reservation and commits the actual amount to budget 0's
// unit. It is the single-budget settlement path, retained for the error-handling
// API contract and its own tests. The proxy no longer calls it: multi-budget
// agents settle through ReconcileMulti, which commits per-budget actuals.
//
// PRECONDITION: budget index 0 is the token budget. actualTokens is committed
// to budgets[0] in token units. Any other budget (for example a dollars budget)
// keeps its reserved estimate, which over-counts in the safe direction. The
// per-budget actuals path is ReconcileMulti, which the proxy uses for any agent
// with a dollar budget, so this positional assumption is no longer on the live
// path. A configuration that lists a non-token budget first would commit a token
// count into the wrong unit, which is why the live path is ReconcileMulti.
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

// StatusOf returns a snapshot of the agent's first budget for inspection and
// tests. It reports the committed usage and remaining for budget index 0 (the
// token budget in the single-budget path). StatusAll returns every budget for a
// multi-budget agent. Returns an error for an unknown agent.
func (store *Store) StatusOf(agentName string) (BudgetStatus, error) {
	state, err := store.lookup(agentName)
	if err != nil {
		return BudgetStatus{}, err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if len(state.budgets) == 0 {
		return BudgetStatus{}, fmt.Errorf("agent %q has no budgets", agentName)
	}
	window := state.budgets[0]
	return BudgetStatus{
		Type:      window.Unit,
		Limit:     window.Limit,
		Used:      window.used(),
		Remaining: window.remaining(),
		ResetAt:   window.recoveryTime(0),
	}, nil
}

// Track commits an actual amount with no reservation. It is the single-budget
// observe-mode path, retained for the API contract and its own tests. The proxy
// now tracks observe-mode breaches through TrackMulti, which commits per-budget
// actuals. Applies to budget 0 (the token budget) for the single-budget path.
//
// PRECONDITION: budget index 0 is the token budget (same positional assumption
// as Reconcile). The per-budget path is TrackMulti, which the proxy uses for any
// agent with a dollar budget. The len check guards against a zero-budget state,
// which cannot occur for enforce or observe agents (config requires at least one
// budget) and which never reaches here for passthrough agents (they are absent
// from the map, so lookup returns an error first). It is defensive only.
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

// takeReservation subtracts the held reservations' reserved amounts, deletes the
// reservation, and returns the held reservations so the caller can commit
// actuals. The caller holds the agent lock. Returns (nil, false) when the
// reservation id is unknown.
func (state *agentBudgetState) takeReservation(reservationID types.ReservationID) ([]reservedAmount, bool) {
	held, ok := state.reservations[uint64(reservationID)]
	if !ok {
		return nil, false
	}
	for _, reservation := range held {
		state.budgets[reservation.budgetIndex].reserved -= reservation.amount
	}
	delete(state.reservations, uint64(reservationID))
	return held, true
}

// ReconcileMulti releases the reservation and commits actuals[budgetIndex] to each
// held budget window, instead of the positional single-budget Reconcile. actuals
// is index-aligned with the agent's budgets (tokens slot in tokens, dollars slot
// in microdollars). This is the multi-budget settlement path the proxy uses, the
// single-budget Reconcile is retained unchanged for the 001 API contract.
func (store *Store) ReconcileMulti(agentName string, reservationID types.ReservationID, actuals []int64) error {
	state, err := store.lookup(agentName)
	if err != nil {
		return err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()

	if len(actuals) != len(state.budgets) {
		return fmt.Errorf("agent %q: got %d actuals, want %d budgets", agentName, len(actuals), len(state.budgets))
	}
	held, ok := state.takeReservation(reservationID)
	if !ok {
		return fmt.Errorf("agent %q: unknown reservation %d", agentName, reservationID)
	}
	for _, reservation := range held {
		state.budgets[reservation.budgetIndex].commit(actuals[reservation.budgetIndex])
	}
	store.limiter.Release(agentName)
	return nil
}

// TrackMulti commits actuals[i] to budget i with no reservation, for observe-mode
// breaches on a multi-budget agent. actuals is index-aligned with the budgets.
func (store *Store) TrackMulti(agentName string, actuals []int64) error {
	state, err := store.lookup(agentName)
	if err != nil {
		return err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if len(actuals) != len(state.budgets) {
		return fmt.Errorf("agent %q: got %d actuals, want %d budgets", agentName, len(actuals), len(state.budgets))
	}
	for i := range state.budgets {
		state.budgets[i].commit(actuals[i])
	}
	return nil
}

// StatusAll returns a snapshot of every budget for the agent, index-aligned with
// the configured budgets. It is the multi-budget counterpart to StatusOf and is
// used by tests asserting per-budget settlement (and by the Session 9 admin API).
func (store *Store) StatusAll(agentName string) ([]BudgetStatus, error) {
	state, err := store.lookup(agentName)
	if err != nil {
		return nil, err
	}
	state.mutex.Lock()
	defer state.mutex.Unlock()
	statuses := make([]BudgetStatus, len(state.budgets))
	for i, window := range state.budgets {
		statuses[i] = BudgetStatus{
			Type:      window.Unit,
			Limit:     window.Limit,
			Used:      window.used(),
			Remaining: window.remaining(),
			ResetAt:   window.recoveryTime(0),
		}
	}
	return statuses, nil
}
