package budget

import (
	"math"
	"testing"
	"testing/quick"

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
