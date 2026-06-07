package budget

import (
	"math"
	"strconv"
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
	if err := store.Forfeit("a", id); err != nil {
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
	if _, _, err := store.Reserve("ghost", 1); err == nil {
		t.Fatal("Reserve for unknown agent should error")
	}
}

func TestMultiBudgetAnyExhaustionBlocks(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	agent := config.AgentConfig{
		Name: "a", Mode: "enforce",
		Identifier: config.IdentifierConfig{Type: "header", HeaderName: "X-Levee-Agent", HeaderValue: "a"},
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 1.00, Window: "1h", WindowType: "rolling"}, // 100 cents
		},
	}
	store := newTestStore(t, []config.AgentConfig{agent}, fake.read)

	// Dollar budget is the binding constraint: 100 cents. Caller passes cents.
	if _, ok, _ := store.ReserveMulti("a", []int64{500, 150}); ok {
		t.Fatal("should fail: dollar amount 150 exceeds 100-cent budget")
	}
	// Token-only fits but dollar overflows, so the whole reserve must roll back
	// and leave the token budget untouched.
	if _, ok, _ := store.ReserveMulti("a", []int64{1000, 100}); !ok {
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
			if forfeitErr := store.Forfeit("a", id); forfeitErr != nil {
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
			{Type: "dollars", Limit: 1.00, Window: "24h", WindowType: "rolling"}, // 100 cents
		},
	}
	store := newTestStore(t, []config.AgentConfig{agent}, fake.read)

	// Both amounts exceed their budget's remaining, so both budgets fail.
	outcome, err := store.Admit("a", []int64{1500, 150})
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
