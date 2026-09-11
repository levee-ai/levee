package budget

import (
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/levee-ai/levee/internal/config"
)

func oneTokenBudgetAgent(name string, limit int64) config.AgentConfig {
	return config.AgentConfig{
		Name: name,
		Mode: "enforce",
		Identifier: config.IdentifierConfig{
			Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: name,
		},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: float64(limit), Window: "1h", WindowType: "rolling"},
		},
	}
}

func passthroughAgent(name string) config.AgentConfig {
	return config.AgentConfig{
		Name: name,
		Mode: "passthrough",
		Identifier: config.IdentifierConfig{
			Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: name,
		},
	}
}

func newTestStore(t *testing.T, agents []config.AgentConfig, now clock) *Store {
	t.Helper()
	store, err := NewStore(agents, 50, now)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestReserveSucceedsWhenAvailable(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	id, ok, err := store.Reserve("a", 300)
	if err != nil || !ok {
		t.Fatalf("Reserve: got (ok=%v, err=%v), want (true, nil)", ok, err)
	}
	if id == 0 {
		t.Fatal("Reserve returned zero ReservationID on success")
	}
}

func TestReserveFailsWhenExhausted(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	if _, ok, _ := store.Reserve("a", 600); !ok {
		t.Fatal("first reserve should succeed")
	}
	_, ok, _ := store.Reserve("a", 500) // 600 held + 500 > 1000
	if ok {
		t.Fatal("second reserve should fail (would exceed budget)")
	}
}

func TestReserveZeroIDWhenRejected(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 100)}, fake.read)
	id, ok, _ := store.Reserve("a", 999)
	if ok || id != 0 {
		t.Fatalf("rejected reserve: got (id=%d, ok=%v), want (0, false)", id, ok)
	}
}

func TestReconcileReturnsDifference(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	id, _, _ := store.Reserve("a", 500) // hold 500
	if err := store.Reconcile("a", id, 200); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Actual was 200, so only 200 committed, hold released. remaining = 800.
	if _, ok, _ := store.Reserve("a", 800); !ok {
		t.Fatal("after reconcile of 200, 800 should be available")
	}
}

func TestReconcileOverageGoesNegative(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	id, _, _ := store.Reserve("a", 500)
	if err := store.Reconcile("a", id, 1500); err != nil { // actual >> estimate
		t.Fatalf("Reconcile: %v", err)
	}
	// 1500 committed against a 1000 budget. Next reserve must fail.
	if _, ok, _ := store.Reserve("a", 1); ok {
		t.Fatal("budget should be negative, reserve must fail")
	}
}

func TestForfeitDeductsFullEstimate(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	id, _, _ := store.Reserve("a", 400)
	if _, err := store.Forfeit("a", id); err != nil {
		t.Fatalf("Forfeit: %v", err)
	}
	// Full 400 stays committed. remaining = 600.
	if _, ok, _ := store.Reserve("a", 601); ok {
		t.Fatal("only 600 should remain after forfeit of 400")
	}
	if _, ok, _ := store.Reserve("a", 600); !ok {
		t.Fatal("exactly 600 should be available")
	}
}

func TestTrackDeductsWithoutReservation(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	if err := store.Track("a", 250); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if _, ok, _ := store.Reserve("a", 751); ok {
		t.Fatal("only 750 should remain after tracking 250")
	}
}

func TestReconcileInvalidIDErrors(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)
	if err := store.Reconcile("a", 9999, 100); err == nil {
		t.Fatal("Reconcile with unknown ID should error")
	}
}

func TestUnknownAgentErrors(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)
	_, _, err := store.Reserve("ghost", 1)
	if err == nil {
		t.Fatal("Reserve for unknown agent should error")
	}
	if !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("Reserve(ghost) error = %v, want ErrUnknownAgent", err)
	}
}

func TestMultiBudgetAnyExhaustionBlocks(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agent := config.AgentConfig{
		Name: "a", Mode: "enforce",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a"},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 1.00, Window: "1h", WindowType: "rolling"}, // 1_000_000 microdollars
		},
	}
	store := newTestStore(t, []config.AgentConfig{agent}, fake.read)

	// Dollar budget is the binding constraint: 1_000_000 microdollars. Caller
	// passes microdollars. 1_500_000 exceeds the $1.00 budget.
	if _, ok, _ := store.ReserveMulti("a", []int64{500, 1_500_000}); ok {
		t.Fatal("should fail: dollar amount 1_500_000 exceeds 1_000_000-microdollar budget")
	}
	// Token-only fits and dollar fits, so this reserve succeeds and proves the
	// prior failed reserve rolled back cleanly.
	if _, ok, _ := store.ReserveMulti("a", []int64{1000, 1_000_000}); !ok {
		t.Fatal("token budget should be fully available after rollback")
	}
}

func TestConcurrentReservesSerialized(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	done := make(chan bool, 100)
	granted := make(chan bool, 100)
	for i := 0; i < 100; i++ {
		go func() {
			_, ok, _ := store.Reserve("a", 100) // 10 should fit (1000/100), rest fail
			if ok {
				granted <- true
			}
			done <- true
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}
	close(granted)
	count := 0
	for range granted {
		count++
	}
	if count != 10 {
		t.Fatalf("exactly 10 reserves of 100 should fit in 1000, got %d", count)
	}
}

func TestStreamLimitBlocksReserve(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	// Large budget so the stream cap, not the budget, is the binding limit.
	store, err := NewStore([]config.AgentConfig{oneTokenBudgetAgent("a", 1_000_000)}, 2, fake.read)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	id1, ok1, _ := store.Reserve("a", 1)
	_, ok2, _ := store.Reserve("a", 1)
	_, ok3, _ := store.Reserve("a", 1) // third should fail: stream cap 2
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("stream cap: got (%v,%v,%v), want (true,true,false)", ok1, ok2, ok3)
	}
	// Reconcile frees a slot.
	if err := store.Reconcile("a", id1, 1); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok, _ := store.Reserve("a", 1); !ok {
		t.Fatal("after reconcile, a stream slot should be free")
	}
}

// TestPropertyReconcileConservesBudget asserts: after a sequence of
// reserve+reconcile pairs (each actual <= estimate, both >= 0), the committed
// usage equals the sum of actuals exactly. No off-by-one, no drift.
func TestPropertyReconcileConservesBudget(t *testing.T) {
	property := func(rawActuals []uint16) bool {
		fake := &fakeClock{now: baseTime()}
		// Huge budget so nothing is ever rejected: we test arithmetic, not gating.
		store, err := NewStore(
			[]config.AgentConfig{oneTokenBudgetAgent("a", math.MaxInt32)},
			math.MaxInt32, fake.read)
		if err != nil {
			return false
		}
		var expected int64
		for _, raw := range rawActuals {
			estimate := int64(raw) + 10
			actual := int64(raw)
			id, ok, reserveErr := store.Reserve("a", estimate)
			if reserveErr != nil || !ok {
				return false
			}
			if reconcileErr := store.Reconcile("a", id, actual); reconcileErr != nil {
				return false
			}
			expected += actual
		}
		state, _ := store.lookup("a")
		state.mutex.Lock()
		used := state.budgets[0].used()
		state.mutex.Unlock()
		return used == expected
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatalf("budget conservation property failed: %v", err)
	}
}

// TestPropertyForfeitNeverUnderCounts asserts: forfeiting always commits the
// full estimate, so committed usage is always >= the actual that would have
// been committed. Over-counting is the safe direction.
func TestPropertyForfeitNeverUnderCounts(t *testing.T) {
	property := func(rawEstimates []uint16) bool {
		fake := &fakeClock{now: baseTime()}
		store, err := NewStore(
			[]config.AgentConfig{oneTokenBudgetAgent("a", math.MaxInt32)},
			math.MaxInt32, fake.read)
		if err != nil {
			return false
		}
		var totalEstimate int64
		for _, raw := range rawEstimates {
			estimate := int64(raw) + 1
			id, ok, reserveErr := store.Reserve("a", estimate)
			if reserveErr != nil || !ok {
				return false
			}
			if _, forfeitErr := store.Forfeit("a", id); forfeitErr != nil {
				return false
			}
			totalEstimate += estimate
		}
		state, _ := store.lookup("a")
		state.mutex.Lock()
		used := state.budgets[0].used()
		state.mutex.Unlock()
		// Forfeit commits the full estimate every time.
		return used == totalEstimate
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 200}); err != nil {
		t.Fatalf("forfeit property failed: %v", err)
	}
}

func TestAdmit_BudgetReject_SetsBindingAndReason(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	outcome, err := store.Admit("a", []int64{1500}) // exceeds 1000
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome.Admitted {
		t.Fatal("expected rejection")
	}
	if outcome.Reason != RejectBudget {
		t.Fatalf("Reason: got %v, want RejectBudget", outcome.Reason)
	}
	if outcome.Binding == nil {
		t.Fatal("expected non-nil Binding on budget reject")
	}
	if outcome.Binding.Type != "tokens" || outcome.Binding.Limit != 1000 {
		t.Fatalf("Binding: got %+v, want tokens/1000", outcome.Binding)
	}
	if outcome.Binding.Remaining != 1000 {
		t.Fatalf("Binding.Remaining: got %d, want 1000", outcome.Binding.Remaining)
	}
}

func TestAdmit_Success_ReturnsID(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	outcome, err := store.Admit("a", []int64{300})
	if err != nil || !outcome.Admitted {
		t.Fatalf("Admit: got (admitted=%v, err=%v), want (true, nil)", outcome.Admitted, err)
	}
	if outcome.ID == 0 {
		t.Fatal("expected non-zero ReservationID")
	}
	if outcome.Reason != RejectNone {
		t.Fatalf("Reason: got %v, want RejectNone", outcome.Reason)
	}
}

func TestAdmit_ConcurrencyReject_NoBinding(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agent := oneTokenBudgetAgent("a", 1000000)
	store, err := NewStore([]config.AgentConfig{agent}, 1, fake.read) // stream limit 1
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if outcome, _ := store.Admit("a", []int64{10}); !outcome.Admitted {
		t.Fatal("first admit should succeed")
	}
	outcome, err := store.Admit("a", []int64{10}) // slot taken
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome.Admitted {
		t.Fatal("expected concurrency rejection")
	}
	if outcome.Reason != RejectConcurrency {
		t.Fatalf("Reason: got %v, want RejectConcurrency", outcome.Reason)
	}
	if outcome.Binding != nil {
		t.Fatal("expected nil Binding on concurrency reject")
	}
}

// TestAdmit_NegativeAmount_Rejected guards the store against a negative requested
// amount. A negative amount must never be treated as "fits" (it is always less
// than any non-negative remaining), because admitting it would bypass enforcement
// and a negative reserved delta would inflate available budget. The store rejects
// it as RejectBudget with a non-nil Binding, consumes no concurrency slot, and
// leaves reserved state untouched (verified by then admitting the full budget).
func TestAdmit_NegativeAmount_Rejected(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	outcome, err := store.Admit("a", []int64{-5})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome.Admitted {
		t.Fatal("negative amount must not be admitted")
	}
	if outcome.Reason != RejectBudget {
		t.Fatalf("Reason: got %v, want RejectBudget", outcome.Reason)
	}
	if outcome.Binding == nil {
		t.Fatal("expected non-nil Binding on negative-amount reject")
	}
	// No reserved state should have been mutated and no stream slot consumed, so
	// the full budget is still available to a normal request.
	if _, ok, _ := store.Reserve("a", 1000); !ok {
		t.Fatal("full 1000 budget should remain after a rejected negative amount")
	}
}

// TestAdmit_MultiBudget_BindingIsLongestRecovery exercises the binding-selection
// logic directly: when more than one budget fails, Admit must report the budget
// with the LATER recovery instant, not the first one it scans. The agent has a
// tokens budget over a 1h window and a dollars budget over a 24h window. A request
// that overruns both makes both fail. Because each amount exceeds its limit,
// recoveryTime returns now plus the window size for each, so the dollars budget
// recovers at now plus 24h while the tokens budget recovers at now plus 1h. The
// dollars budget is therefore the binding one: reporting the later recovery means
// a derived Retry-After never under-states the true wait. A regression that
// returned on the first miss, or compared recovery times the wrong way, would pick
// the tokens budget and fail this test.
func TestAdmit_MultiBudget_BindingIsLongestRecovery(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agent := config.AgentConfig{
		Name: "a", Mode: "enforce",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a"},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 1.00, Window: "24h", WindowType: "rolling"}, // 1_000_000 microdollars
		},
	}
	store := newTestStore(t, []config.AgentConfig{agent}, fake.read)

	// Both amounts exceed their budget's remaining, so both budgets fail.
	outcome, err := store.Admit("a", []int64{1500, 1_500_000})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if outcome.Admitted {
		t.Fatal("expected rejection: both budgets overrun")
	}
	if outcome.Reason != RejectBudget {
		t.Fatalf("Reason: got %v, want RejectBudget", outcome.Reason)
	}
	if outcome.Binding == nil {
		t.Fatal("expected non-nil Binding when budgets fail")
	}
	// The 24h dollars budget recovers later than the 1h tokens budget, so it is
	// the binding constraint.
	if outcome.Binding.Type != "dollars" {
		t.Fatalf("Binding.Type: got %q, want \"dollars\" (the longer-window budget)", outcome.Binding.Type)
	}
	tokensRecovery := baseTime().Add(time.Hour)
	dollarsRecovery := baseTime().Add(24 * time.Hour)
	if !outcome.Binding.ResetAt.Equal(dollarsRecovery) {
		t.Fatalf("Binding.ResetAt: got %s, want %s (now plus 24h)",
			outcome.Binding.ResetAt.UTC(), dollarsRecovery.UTC())
	}
	if !outcome.Binding.ResetAt.After(tokensRecovery) {
		t.Fatalf("Binding.ResetAt %s must be after the tokens budget recovery %s",
			outcome.Binding.ResetAt.UTC(), tokensRecovery.UTC())
	}
}

func multiBudgetAgent(name string) config.AgentConfig {
	return config.AgentConfig{
		Name: name, Mode: "enforce",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: name},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 1.00, Window: "1h", WindowType: "rolling"}, // 1_000_000 microdollars
		},
	}
}

func TestReconcileMulti_CommitsPerBudgetActuals(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{multiBudgetAgent("a")}, fake.read)

	// Reserve over-estimates both: 800 tokens, 900_000 microdollars.
	id, ok, err := store.ReserveMulti("a", []int64{800, 900_000})
	if err != nil || !ok {
		t.Fatalf("ReserveMulti: ok=%v err=%v", ok, err)
	}
	// Actuals are smaller: 40 tokens, 50_000 microdollars ($0.05).
	if _, err := store.ReconcileMulti("a", id, []int64{40, 50_000}); err != nil {
		t.Fatalf("ReconcileMulti: %v", err)
	}
	statuses, err := store.StatusAll("a")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if statuses[0].Used != 40 {
		t.Errorf("tokens used = %d, want 40", statuses[0].Used)
	}
	if statuses[1].Used != 50_000 {
		t.Errorf("dollars used = %d microdollars, want 50_000 (the dollar budget must NOT be stuck at the 900_000 estimate)", statuses[1].Used)
	}
}

func TestTrackMulti_CommitsPerBudgetActuals(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{multiBudgetAgent("a")}, fake.read)

	if err := store.TrackMulti("a", []int64{30, 70_000}); err != nil {
		t.Fatalf("TrackMulti: %v", err)
	}
	statuses, _ := store.StatusAll("a")
	if statuses[0].Used != 30 || statuses[1].Used != 70_000 {
		t.Errorf("used = (%d,%d), want (30,70000)", statuses[0].Used, statuses[1].Used)
	}
}

func TestReconcileMulti_LengthMismatchErrors(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{multiBudgetAgent("a")}, fake.read)
	id, ok, err := store.ReserveMulti("a", []int64{10, 10})
	if err != nil || !ok {
		t.Fatalf("ReserveMulti setup: ok=%v err=%v", ok, err)
	}
	if _, err := store.ReconcileMulti("a", id, []int64{10}); err == nil {
		t.Fatal("expected an error for a one-element actuals against a two-budget agent")
	}
	// The mismatch must mutate nothing: the reservation must still be live, so
	// Forfeit on the same id succeeds. If the length check were ever moved after
	// takeReservation, the reservation would be consumed and this Forfeit would
	// fail with unknown reservation.
	if _, err := store.Forfeit("a", id); err != nil {
		t.Fatalf("reservation must survive a mismatch error, but Forfeit failed: %v", err)
	}
}

func TestStatusAll_ReturnsEveryBudget(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{multiBudgetAgent("a")}, fake.read)
	statuses, err := store.StatusAll("a")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len = %d, want 2", len(statuses))
	}
	if statuses[0].Type != "tokens" || statuses[1].Type != "dollars" {
		t.Errorf("types = (%q,%q), want (tokens,dollars)", statuses[0].Type, statuses[1].Type)
	}
	if statuses[1].Limit != 1_000_000 {
		t.Errorf("dollars limit = %d microdollars, want 1_000_000", statuses[1].Limit)
	}
}

func TestReconcileMulti_ReleasesStreamSlot(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	// Stream limit 1 so the slot, not the budget, is the binding constraint.
	store, err := NewStore([]config.AgentConfig{multiBudgetAgent("a")}, 1, fake.read)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	id, ok, err := store.ReserveMulti("a", []int64{10, 10})
	if err != nil || !ok {
		t.Fatalf("first ReserveMulti: ok=%v err=%v", ok, err)
	}
	// Slot is now taken. Reconcile must release it.
	if _, err := store.ReconcileMulti("a", id, []int64{10, 10}); err != nil {
		t.Fatalf("ReconcileMulti: %v", err)
	}
	if _, ok2, err := store.ReserveMulti("a", []int64{10, 10}); err != nil || !ok2 {
		t.Fatalf("second ReserveMulti after ReconcileMulti must succeed (slot released): ok=%v err=%v", ok2, err)
	}
}

// TestCommit_SaturatesInsteadOfWrapping is a regression guard for the phantom-credit
// bug: two MaxInt64 commits to a dollars budget once wrapped used() negative via plain
// +=, producing remaining > limit (a budget credit that never existed). With saturating
// commit, used() clamps to MaxInt64 and remaining stays <= 0.
func TestCommit_SaturatesInsteadOfWrapping(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agent := config.AgentConfig{
		Name: "a", Mode: "observe",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a"},
		Budgets: []config.BudgetConfig{
			{Type: "dollars", Limit: 1.00, Window: "1h", WindowType: "rolling"},
		},
	}
	store, err := NewStore([]config.AgentConfig{agent}, 50, fake.read)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Two MaxInt64 commits. Without saturation the second wraps used() negative,
	// producing a phantom credit (remaining > limit). With saturation used() stays
	// at MaxInt64 and remaining stays <= 0.
	if err := store.TrackMulti("a", []int64{math.MaxInt64}); err != nil {
		t.Fatalf("TrackMulti 1: %v", err)
	}
	if err := store.TrackMulti("a", []int64{math.MaxInt64}); err != nil {
		t.Fatalf("TrackMulti 2: %v", err)
	}
	statuses, err := store.StatusAll("a")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if statuses[0].Used < 0 {
		t.Errorf("used wrapped negative: %d (phantom credit, the runaway-bill bug)", statuses[0].Used)
	}
	if statuses[0].Used != math.MaxInt64 {
		t.Errorf("used = %d, want MaxInt64 (saturated)", statuses[0].Used)
	}
	if statuses[0].Remaining > 0 {
		t.Errorf("remaining = %d, want <= 0 (an exhausted budget must not show a credit)", statuses[0].Remaining)
	}
}

// BenchmarkReserveReconcileSingleAgent measures contention when every goroutine
// hits one hot agent (worst case for the per-agent lock).
func BenchmarkReserveReconcileSingleAgent(b *testing.B) {
	fake := &fakeClock{now: baseTime()}
	store, _ := NewStore(
		[]config.AgentConfig{oneTokenBudgetAgent("hot", 1<<62)},
		1<<62, fake.read)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id, ok, _ := store.Reserve("hot", 100)
			if ok {
				_ = store.Reconcile("hot", id, 100)
			}
		}
	})
}

// BenchmarkReserveReconcilePerGoroutineAgent measures the independent-agent
// path: each goroutine uses its own agent, so it should scale near-linearly.
func BenchmarkReserveReconcilePerGoroutineAgent(b *testing.B) {
	fake := &fakeClock{now: baseTime()}
	agents := make([]config.AgentConfig, 0, 64)
	for i := 0; i < 64; i++ {
		agents = append(agents, oneTokenBudgetAgent("agent-"+strconv.Itoa(i), 1<<62))
	}
	store, _ := NewStore(agents, 1<<62, fake.read)
	var counter int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		name := "agent-" + strconv.FormatInt(atomic.AddInt64(&counter, 1)%64, 10)
		for pb.Next() {
			id, ok, _ := store.Reserve(name, 100)
			if ok {
				_ = store.Reconcile(name, id, 100)
			}
		}
	})
}

// newTokenStore builds a store for one agent with a single rolling 1h token
// budget, reusing oneTokenBudgetAgent (the same fixture the rest of this file
// builds on) rather than duplicating the agent scaffolding. Accepts
// testing.TB so both tests and benchmarks can call it.
func newTokenStore(testBench testing.TB, agentName string, limit float64, now clock) *Store {
	testBench.Helper()
	store, err := NewStore([]config.AgentConfig{oneTokenBudgetAgent(agentName, int64(limit))}, 50, now)
	if err != nil {
		testBench.Fatalf("NewStore: %v", err)
	}
	return store
}

// newTokenAndDollarStore builds a store for one agent with a rolling 1h token
// budget and a fixed daily dollar budget (reset at 00:00Z), for tests that
// need a settle to cross more than one budget at once. It extends
// oneTokenBudgetAgent's config with the second budget instead of duplicating
// the agent scaffolding a second time.
func newTokenAndDollarStore(testBench testing.TB, agentName string, tokenLimit, dollarLimit float64, now clock) *Store {
	testBench.Helper()
	agent := oneTokenBudgetAgent(agentName, int64(tokenLimit))
	agent.Budgets = append(agent.Budgets, config.BudgetConfig{
		Type: "dollars", Limit: dollarLimit, Window: "24h", WindowType: "fixed", ResetAt: "00:00Z",
	})
	store, err := NewStore([]config.AgentConfig{agent}, 50, now)
	if err != nil {
		testBench.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestReconcileMulti_ReportsNegativeCrossing(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClock := func() time.Time { return now }
	store := newTokenStore(t, "agent-a", 100, fakeClock)

	id, ok, err := store.Reserve("agent-a", 50)
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	// Acquire the second hold now, while the budget still fits. Admit rejects
	// even a zero-amount reserve once committed usage alone already exceeds
	// the limit (remaining goes negative, and 0 is greater than a negative
	// remaining), so id2 must be reserved before the first crossing below, not
	// after. Settling id2 later exercises the identical crossing transition
	// regardless of when the reservation itself was taken out.
	id2, ok, err := store.Reserve("agent-a", 0)
	if err != nil || !ok {
		t.Fatalf("second reserve: ok=%v err=%v", ok, err)
	}

	// Actual usage far above the estimate pushes committed past the limit.
	crossed, err := store.ReconcileMulti("agent-a", id, []int64{150})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !crossed {
		t.Fatal("expected crossing when committed usage exceeds the limit")
	}

	// Already negative: a further settle must NOT report a new crossing.
	crossed, err = store.ReconcileMulti("agent-a", id2, []int64{10})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if crossed {
		t.Fatal("crossing must be a transition, not a level")
	}
}

func TestForfeit_ReportsNegativeCrossing(t *testing.T) {
	// Admit requires amount <= limit - used - reserved, so the shape is:
	// limit 90, track 75 (remaining 15), reserve 14 (fits), track another 5
	// while the hold is live (committed 80, still under 90), then forfeit:
	// committed 80 + 14 = 94 > 90 crosses the limit.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClock := func() time.Time { return now }
	store := newTokenStore(t, "agent-a", 90, fakeClock)

	if err := store.TrackMulti("agent-a", []int64{75}); err != nil {
		t.Fatalf("track: %v", err)
	}
	id, ok, err := store.Reserve("agent-a", 14)
	if err != nil || !ok {
		t.Fatalf("reserve 14 against remaining 15: ok=%v err=%v", ok, err)
	}
	if err := store.TrackMulti("agent-a", []int64{5}); err != nil {
		t.Fatalf("second track: %v", err)
	}
	crossed, err := store.Forfeit("agent-a", id)
	if err != nil {
		t.Fatalf("forfeit: %v", err)
	}
	if !crossed {
		t.Fatal("expected forfeit to cross the limit (80 committed + 14 forfeited > 90)")
	}
}

func TestReconcileMulti_TwoBudgetsCrossingReportsOnce(t *testing.T) {
	// Agent with a rolling token budget and a fixed dollar budget, both small.
	// One settle pushes BOTH past their limits: crossed is a single bool.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClock := func() time.Time { return now }
	store := newTokenAndDollarStore(t, "agent-a", 100, 0.01, fakeClock)

	id, ok, err := store.ReserveMulti("agent-a", []int64{50, 5000})
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	crossed, err := store.ReconcileMulti("agent-a", id, []int64{200, 20000})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !crossed {
		t.Fatal("expected crossing")
	}
}

// TestReconcileMulti_OnlyFirstBudgetCrosses guards the OR aggregation in
// ReconcileMulti's settlement loop. The token budget (checked first) crosses,
// the dollar budget (checked second, and checked last) stays well under its
// limit. An overwrite bug (crossed = ... instead of crossed = crossed || ...)
// would let the dollar budget's false wipe out the token budget's true, and
// this test would catch it. TestReconcileMulti_TwoBudgetsCrossingReportsOnce
// alone cannot: it crosses both budgets, so an overwrite still lands on true.
func TestReconcileMulti_OnlyFirstBudgetCrosses(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClock := func() time.Time { return now }
	store := newTokenAndDollarStore(t, "agent-a", 100, 1_000_000, fakeClock)

	id, ok, err := store.ReserveMulti("agent-a", []int64{50, 5000})
	if err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	// Token settle (200) exceeds the 100-token limit. Dollar settle (6000
	// microdollars) stays far under the 1_000_000-dollar (1e12-microdollar) limit.
	crossed, err := store.ReconcileMulti("agent-a", id, []int64{200, 6000})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !crossed {
		t.Fatal("expected crossing: the token budget alone crosses even though the dollar budget does not")
	}
}

func TestCrossing_CanRefireAfterAging(t *testing.T) {
	fake := &fakeClock{now: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
	store := newTokenStore(t, "agent-a", 100, fake.read)

	id, ok, _ := store.Reserve("agent-a", 10)
	if !ok {
		t.Fatal("reserve rejected")
	}
	crossed, _ := store.ReconcileMulti("agent-a", id, []int64{150})
	if !crossed {
		t.Fatal("first crossing expected")
	}

	// Advance past the window so committed usage ages out entirely.
	fake.advance(2 * time.Hour)
	id, ok, _ = store.Reserve("agent-a", 10)
	if !ok {
		t.Fatal("post-aging reserve rejected")
	}
	crossed, _ = store.ReconcileMulti("agent-a", id, []int64{150})
	if !crossed {
		t.Fatal("crossing must re-fire after usage ages back under the limit")
	}
}

// TestOutstandingReservations exercises the cross-agent summation in
// OutstandingReservations across two agents, so a bug that only summed the
// first agent looked up (or overwrote rather than accumulated the total)
// would be caught, not just a single-agent count that a broken sum could
// still get right by accident.
func TestOutstandingReservations(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(
		[]config.AgentConfig{oneTokenBudgetAgent("agent-a", 1000), oneTokenBudgetAgent("agent-b", 1000)},
		50, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if store.OutstandingReservations() != 0 {
		t.Fatal("fresh store must have zero outstanding reservations")
	}

	idForAgentA, ok, _ := store.Reserve("agent-a", 10)
	if !ok {
		t.Fatal("reserve rejected for agent-a")
	}
	idForAgentB, ok, _ := store.Reserve("agent-b", 10)
	if !ok {
		t.Fatal("reserve rejected for agent-b")
	}
	if store.OutstandingReservations() != 2 {
		t.Fatal("expected two outstanding reservations across both agents")
	}

	if _, err := store.Forfeit("agent-a", idForAgentA); err != nil {
		t.Fatalf("forfeit agent-a: %v", err)
	}
	if store.OutstandingReservations() != 1 {
		t.Fatal("expected one outstanding reservation after forfeiting agent-a's hold")
	}
	if _, err := store.Forfeit("agent-b", idForAgentB); err != nil {
		t.Fatalf("forfeit agent-b: %v", err)
	}
	if store.OutstandingReservations() != 0 {
		t.Fatal("expected zero after forfeiting both")
	}
}

func BenchmarkReconcileMultiTwoBudgets(b *testing.B) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	// Limits far above any plausible b.N: the fake clock never advances, so
	// nothing ever ages out of the rolling token window and committed usage
	// only grows. A limit sized for a normal test hard-fails once b.N crosses
	// limit divided by the per-iteration commit amount (verified: the previous
	// 1_000_000_000-token limit failed at -benchtime=5s). 1e15 tokens and 1e9
	// dollars (the config validation ceiling, 1e15 microdollars) leave headroom
	// far beyond any iteration count a benchmark run reaches.
	store := newTokenAndDollarStore(b, "agent-a", 1e15, 1e9, func() time.Time { return now })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, ok, err := store.ReserveMulti("agent-a", []int64{100, 500})
		if err != nil || !ok {
			b.Fatalf("reserve: ok=%v err=%v", ok, err)
		}
		if _, err := store.ReconcileMulti("agent-a", id, []int64{90, 450}); err != nil {
			b.Fatalf("reconcile: %v", err)
		}
	}
}

func BenchmarkForfeitTwoBudgets(b *testing.B) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := newTokenAndDollarStore(b, "agent-a", 1e15, 1e9, func() time.Time { return now })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id, ok, err := store.ReserveMulti("agent-a", []int64{100, 500})
		if err != nil || !ok {
			b.Fatalf("reserve: ok=%v err=%v", ok, err)
		}
		if _, err := store.Forfeit("agent-a", id); err != nil {
			b.Fatalf("forfeit: %v", err)
		}
	}
}

func TestSetPausedAndIsPaused(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, nil)

	if store.IsPaused("a") {
		t.Fatal("agent paused before any SetPaused call")
	}
	if err := store.SetPaused("a", true); err != nil {
		t.Fatalf("SetPaused(a, true): %v", err)
	}
	if !store.IsPaused("a") {
		t.Fatal("IsPaused false after SetPaused true")
	}
	if err := store.SetPaused("a", true); err != nil {
		t.Fatalf("second SetPaused(a, true): %v", err)
	}
	if err := store.SetPaused("a", false); err != nil {
		t.Fatalf("SetPaused(a, false): %v", err)
	}
	if store.IsPaused("a") {
		t.Fatal("IsPaused true after SetPaused false")
	}
}

func TestSetPausedUnknownAgentErrors(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, nil)
	err := store.SetPaused("typo", true)
	if !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("SetPaused(typo) error = %v, want ErrUnknownAgent", err)
	}
}

func TestIsPausedUnknownAgentIsFalse(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, nil)
	if store.IsPaused("nobody") {
		t.Fatal("IsPaused(nobody) = true, want false")
	}
}

func TestPassthroughAgentCanBePaused(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{passthroughAgent("scraper")}, nil)
	if err := store.SetPaused("scraper", true); err != nil {
		t.Fatalf("SetPaused on passthrough agent: %v", err)
	}
	if !store.IsPaused("scraper") {
		t.Fatal("passthrough agent not paused after SetPaused")
	}
}

func TestPausedAgentsSorted(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{
		oneTokenBudgetAgent("zeta", 1000),
		passthroughAgent("alpha"),
		oneTokenBudgetAgent("mid", 1000),
		oneTokenBudgetAgent("delta", 1000),
		passthroughAgent("omega"),
	}, nil)
	if got := store.PausedAgents(); len(got) != 0 {
		t.Fatalf("PausedAgents on fresh store = %v, want empty", got)
	}
	for _, name := range []string{"zeta", "alpha", "omega", "delta"} {
		if err := store.SetPaused(name, true); err != nil {
			t.Fatalf("SetPaused(%s): %v", name, err)
		}
	}
	// Repeated calls each draw a fresh random map-iteration start, so an
	// unsorted implementation must produce the sorted order on every one of
	// these independent draws to pass, which catches a missing sort with
	// near certainty.
	want := []string{"alpha", "delta", "omega", "zeta"}
	for call := 0; call < 8; call++ {
		got := store.PausedAgents()
		if len(got) != len(want) {
			t.Fatalf("PausedAgents call %d = %v, want %v", call, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("PausedAgents call %d = %v, want %v (exact sorted order)", call, got, want)
			}
		}
	}
}

func TestPauseControlsAreRaceSafe(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{
		oneTokenBudgetAgent("a", 1000),
		passthroughAgent("b"),
	}, nil)
	var waitGroup sync.WaitGroup
	for worker := 0; worker < 50; worker++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			name := "a"
			if index%2 == 0 {
				name = "b"
			}
			for i := 0; i < 100; i++ {
				_ = store.SetPaused(name, i%2 == 0)
				_ = store.IsPaused(name)
				_ = store.PausedAgents()
			}
		}(worker)
	}
	waitGroup.Wait()
}

func TestResetUsageZeroesRollingWindow(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	if err := store.Track("a", 700); err != nil {
		t.Fatalf("Track: %v", err)
	}
	cleared, err := store.ResetUsage("a")
	if err != nil {
		t.Fatalf("ResetUsage: %v", err)
	}
	if len(cleared) != 1 || cleared[0] != 700 {
		t.Fatalf("cleared = %v, want [700]", cleared)
	}
	status, err := store.StatusOf("a")
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if status.Used != 0 || status.Remaining != 1000 {
		t.Fatalf("after reset: used=%d remaining=%d, want 0 and 1000", status.Used, status.Remaining)
	}
}

func TestResetUsageZeroesFixedWindowKeepsAnchor(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agentConfig := config.AgentConfig{
		Name: "a",
		Mode: "enforce",
		Identifier: config.IdentifierConfig{
			Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a",
		},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "24h", WindowType: "fixed", ResetAt: "00:00Z"},
		},
	}
	store := newTestStore(t, []config.AgentConfig{agentConfig}, fake.read)

	if err := store.Track("a", 400); err != nil {
		t.Fatalf("Track: %v", err)
	}
	before, err := store.StatusOf("a")
	if err != nil {
		t.Fatalf("StatusOf before: %v", err)
	}
	cleared, err := store.ResetUsage("a")
	if err != nil {
		t.Fatalf("ResetUsage: %v", err)
	}
	if len(cleared) != 1 || cleared[0] != 400 {
		t.Fatalf("cleared = %v, want [400]", cleared)
	}
	after, err := store.StatusOf("a")
	if err != nil {
		t.Fatalf("StatusOf after: %v", err)
	}
	if after.Used != 0 {
		t.Fatalf("used = %d after reset, want 0", after.Used)
	}
	if !after.ResetAt.Equal(before.ResetAt) {
		t.Fatalf("fixed-window boundary moved on reset: %v -> %v", before.ResetAt, after.ResetAt)
	}
}

func TestResetUsageKeepsReservationsAndSettlement(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	reservationID, ok, err := store.Reserve("a", 300)
	if err != nil || !ok {
		t.Fatalf("Reserve: ok=%v err=%v", ok, err)
	}
	if _, err := store.ResetUsage("a"); err != nil {
		t.Fatalf("ResetUsage: %v", err)
	}
	status, err := store.StatusOf("a")
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if status.Remaining != 700 {
		t.Fatalf("remaining = %d with live reservation after reset, want 700", status.Remaining)
	}
	if _, err := store.ReconcileMulti("a", reservationID, []int64{250}); err != nil {
		t.Fatalf("ReconcileMulti after reset: %v", err)
	}
	status, err = store.StatusOf("a")
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if status.Used != 250 || status.Remaining != 750 {
		t.Fatalf("after settle: used=%d remaining=%d, want 250 and 750", status.Used, status.Remaining)
	}
}

func TestResetUsageErrors(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{
		oneTokenBudgetAgent("a", 1000),
		passthroughAgent("scraper"),
	}, nil)

	if _, err := store.ResetUsage("typo"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("ResetUsage(typo) error = %v, want ErrUnknownAgent", err)
	}
	if _, err := store.ResetUsage("scraper"); !errors.Is(err, ErrNoBudgets) {
		t.Fatalf("ResetUsage(passthrough) error = %v, want ErrNoBudgets", err)
	}
}

func TestResetUsageDoesNotClearPause(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, nil)
	if err := store.SetPaused("a", true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	if _, err := store.ResetUsage("a"); err != nil {
		t.Fatalf("ResetUsage: %v", err)
	}
	if !store.IsPaused("a") {
		t.Fatal("reset cleared the pause, it must not")
	}
}

func TestResetUsageZeroesEveryBudgetOfMultiBudgetAgent(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agent := config.AgentConfig{
		Name: "a", Mode: "enforce",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a"},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 1.00, Window: "1h", WindowType: "rolling"}, // 1_000_000 microdollars
		},
	}
	store := newTestStore(t, []config.AgentConfig{agent}, fake.read)

	if err := store.TrackMulti("a", []int64{700, 300_000}); err != nil {
		t.Fatalf("TrackMulti: %v", err)
	}
	cleared, err := store.ResetUsage("a")
	if err != nil {
		t.Fatalf("ResetUsage: %v", err)
	}
	if len(cleared) != 2 || cleared[0] != 700 || cleared[1] != 300_000 {
		t.Fatalf("cleared = %v, want [700 300000] (tokens then microdollars)", cleared)
	}
	statuses, err := store.StatusAll("a")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("StatusAll returned %d budgets, want 2", len(statuses))
	}
	for i, status := range statuses {
		if status.Used != 0 {
			t.Fatalf("budget %d (%s): used = %d after reset, want 0", i, status.Type, status.Used)
		}
	}
}

func TestStatusAllReportsReserved(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	store := newTestStore(t, []config.AgentConfig{oneTokenBudgetAgent("a", 1000)}, fake.read)

	if _, ok, err := store.Reserve("a", 300); err != nil || !ok {
		t.Fatalf("Reserve: ok=%v err=%v", ok, err)
	}
	statuses, err := store.StatusAll("a")
	if err != nil {
		t.Fatalf("StatusAll: %v", err)
	}
	if statuses[0].Reserved != 300 {
		t.Fatalf("Reserved = %d, want 300", statuses[0].Reserved)
	}
	total := statuses[0].Used + statuses[0].Reserved + statuses[0].Remaining
	if total != statuses[0].Limit {
		t.Fatalf("used+reserved+remaining = %d, want limit %d", total, statuses[0].Limit)
	}
}

func TestInFlightReservations(t *testing.T) {
	store := newTestStore(t, []config.AgentConfig{
		oneTokenBudgetAgent("a", 1000),
		passthroughAgent("scraper"),
	}, nil)

	count, err := store.InFlightReservations("a")
	if err != nil || count != 0 {
		t.Fatalf("fresh agent: count=%d err=%v, want 0 and nil", count, err)
	}
	if _, ok, err := store.Reserve("a", 100); err != nil || !ok {
		t.Fatalf("Reserve 1: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Reserve("a", 100); err != nil || !ok {
		t.Fatalf("Reserve 2: ok=%v err=%v", ok, err)
	}
	count, err = store.InFlightReservations("a")
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v, want 2 and nil", count, err)
	}
	count, err = store.InFlightReservations("scraper")
	if err != nil || count != 0 {
		t.Fatalf("passthrough: count=%d err=%v, want 0 and nil", count, err)
	}
	if _, err := store.InFlightReservations("typo"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("unknown: err=%v, want ErrUnknownAgent", err)
	}
}
