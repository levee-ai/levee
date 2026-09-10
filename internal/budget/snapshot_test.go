package budget

import (
	"testing"
	"time"

	"github.com/levee-ai/levee/internal/config"
)

func snapshotTestAgents() []config.AgentConfig {
	return []config.AgentConfig{{
		Name: "agent-a",
		Mode: "enforce",
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
			{Type: "dollars", Limit: 50.00, Window: "24h", WindowType: "fixed", ResetAt: "00:00Z"},
		},
	}}
}

func TestExportRestore_RoundTripBothUnits(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }

	source, err := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	if err != nil {
		t.Fatalf("source store: %v", err)
	}
	if err := source.TrackMulti("agent-a", []int64{700, 4_500_000}); err != nil {
		t.Fatalf("track: %v", err)
	}

	exported := source.Export()

	restored, err := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	if err != nil {
		t.Fatalf("restored store: %v", err)
	}
	report, err := restored.Restore(exported)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Discards) != 0 || report.RestoredBudgets != 2 {
		t.Fatalf("report = %+v, want 2 restored and no discards", report)
	}

	statuses, err := restored.StatusAll("agent-a")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if statuses[0].Used != 700 {
		t.Fatalf("token Used = %d, want 700", statuses[0].Used)
	}
	if statuses[1].Used != 4_500_000 {
		t.Fatalf("dollar Used = %d microdollars, want 4500000", statuses[1].Used)
	}
}

// TestRestore_SecondCallErrorsWithoutMutating guards the re-entry trap: a
// second Restore call on the same store must error instead of mutating state
// again. Without the guard, a second call would sum the rolling token bucket
// a second time (700 becoming 1400) and overwrite the fixed dollar window's
// committedFixed a second time, regardless of what a live agent accumulated
// since the first Restore.
func TestRestore_SecondCallErrorsWithoutMutating(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	source, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	_ = source.TrackMulti("agent-a", []int64{700, 4_500_000})
	exported := source.Export()

	restored, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	if _, err := restored.Restore(exported); err != nil {
		t.Fatalf("first Restore: %v", err)
	}

	report, err := restored.Restore(exported)
	if err == nil {
		t.Fatal("second Restore on the same store must return an error")
	}
	if report.RestoredBudgets != 0 || report.AbsentAgents != 0 || len(report.Discards) != 0 {
		t.Fatalf("second Restore report = %+v, want the zero value", report)
	}

	statuses, _ := restored.StatusAll("agent-a")
	if statuses[0].Used != 700 {
		t.Fatalf("token Used = %d, want 700 (second call must not double-count)", statuses[0].Used)
	}
	if statuses[1].Used != 4_500_000 {
		t.Fatalf("dollar Used = %d, want 4500000 (second call must not overwrite)", statuses[1].Used)
	}
}

func TestExport_InFlightReservationsAreDropped(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store, err := NewStore(snapshotTestAgents(), DefaultStreamLimit, func() time.Time { return now })
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, ok, _ := store.ReserveMulti("agent-a", []int64{500, 1_000_000}); !ok {
		t.Fatal("reserve rejected")
	}
	exported := store.Export()
	for _, budgetSnapshot := range exported["agent-a"].Budgets {
		for _, bucket := range budgetSnapshot.Buckets {
			if bucket.Amount != 0 {
				t.Fatalf("reserved-only state must export zero committed usage, got %d", bucket.Amount)
			}
		}
		if budgetSnapshot.Committed != 0 {
			t.Fatalf("fixed committed = %d, want 0", budgetSnapshot.Committed)
		}
	}
}

func TestRestore_IdentityMismatchDiscardsOneBudget(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	source, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	_ = source.TrackMulti("agent-a", []int64{700, 4_500_000})
	exported := source.Export()

	changed := snapshotTestAgents()
	changed[0].Budgets[1].ResetAt = "06:00Z" // fixed-window anchor change
	restored, _ := NewStore(changed, DefaultStreamLimit, fakeClockFunc)
	report, err := restored.Restore(exported)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if report.RestoredBudgets != 1 {
		t.Fatalf("restored = %d, want 1 (token budget only)", report.RestoredBudgets)
	}
	if len(report.Discards) != 1 || report.Discards[0].Field != "reset_at" {
		t.Fatalf("discards = %+v, want one reset_at discard", report.Discards)
	}
	statuses, _ := restored.StatusAll("agent-a")
	if statuses[0].Used != 700 {
		t.Fatalf("token budget should still restore, Used = %d", statuses[0].Used)
	}
	if statuses[1].Used != 0 {
		t.Fatalf("changed fixed budget must start fresh, Used = %d", statuses[1].Used)
	}
}

func TestRestore_EveryIdentityFieldDiscards(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	source, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	_ = source.TrackMulti("agent-a", []int64{700, 4_500_000})
	exported := source.Export()

	mutations := []struct {
		name  string
		field string
		apply func(*config.AgentConfig)
	}{
		{"unit change", "unit", func(agentConfig *config.AgentConfig) {
			agentConfig.Budgets[0].Type = "dollars"
			agentConfig.Budgets[0].Limit = 10.00
		}},
		{"window type change", "window_type", func(agentConfig *config.AgentConfig) {
			agentConfig.Budgets[0].WindowType = "fixed"
			agentConfig.Budgets[0].ResetAt = "00:00Z"
		}},
		{"window size change", "window_seconds", func(agentConfig *config.AgentConfig) {
			agentConfig.Budgets[0].Window = "2h"
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := snapshotTestAgents()
			mutation.apply(&changed[0])
			restored, err := NewStore(changed, DefaultStreamLimit, fakeClockFunc)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			report, err := restored.Restore(exported)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			wantDiscard := RestoreDiscard{Agent: "agent-a", BudgetIndex: 0, Field: mutation.field}
			if len(report.Discards) != 1 || report.Discards[0] != wantDiscard {
				t.Fatalf("discards = %+v, want exactly [%+v]", report.Discards, wantDiscard)
			}
			if report.RestoredBudgets != 1 {
				t.Fatalf("restored = %d, want 1 (the sibling dollar budget)", report.RestoredBudgets)
			}
		})
	}
}

// TestRestore_BucketCountMismatchDiscards covers the bucket_count identity
// field, which TestRestore_EveryIdentityFieldDiscards cannot: bucket count is
// the compile-time defaultBucketCount (60), not a config.BudgetConfig field,
// so no agent config can ever produce this mismatch on the config side. The
// saved snapshot is hand-tampered instead, mirroring how
// TestRestore_BucketCollisionSumsSaturating hand-builds a saved snapshot.
func TestRestore_BucketCountMismatchDiscards(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	restored, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)

	saved := map[string]AgentSnapshot{"agent-a": {Budgets: []BudgetSnapshot{
		{
			Unit: "tokens", WindowType: "rolling", WindowSeconds: 3600, BucketCount: 30,
			Buckets: []BucketSnapshot{{EpochStart: now.Unix(), Amount: 100}},
		},
		exportedFixedZero(),
	}}}
	report, err := restored.Restore(saved)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if report.RestoredBudgets != 1 {
		t.Fatalf("restored = %d, want 1 (sibling fixed budget only)", report.RestoredBudgets)
	}
	if len(report.Discards) != 1 {
		t.Fatalf("discards = %+v, want exactly one", report.Discards)
	}
	want := RestoreDiscard{Agent: "agent-a", BudgetIndex: 0, Field: "bucket_count"}
	if report.Discards[0] != want {
		t.Fatalf("discard = %+v, want %+v", report.Discards[0], want)
	}
}

func TestRestore_AbsentAgentCountsAndExtraIndexDiscards(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	source, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)
	_ = source.TrackMulti("agent-a", []int64{700, 4_500_000})
	exported := source.Export()

	// Current config has the agent renamed and only ONE budget.
	changed := []config.AgentConfig{{
		Name: "agent-b",
		Mode: "enforce",
		Budgets: []config.BudgetConfig{
			{Type: "tokens", Limit: 1000, Window: "1h", WindowType: "rolling"},
		},
	}}

	renamedAgentStore, err := NewStore(changed, DefaultStreamLimit, fakeClockFunc)
	if err != nil {
		t.Fatalf("renamedAgentStore: %v", err)
	}
	report, err := renamedAgentStore.Restore(exported)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if report.AbsentAgents != 1 {
		t.Fatalf("absent agents = %d, want 1", report.AbsentAgents)
	}

	// A saved second budget with no config slot discards by index. This gets
	// its own fresh store: reusing renamedAgentStore would call Restore on it
	// a second time, which the re-entry guard now rejects outright.
	extraBudgetStore, err := NewStore(changed, DefaultStreamLimit, fakeClockFunc)
	if err != nil {
		t.Fatalf("extraBudgetStore: %v", err)
	}
	exportedWithExtraBudget := map[string]AgentSnapshot{"agent-b": exported["agent-a"]}
	extraBudgetReport, err := extraBudgetStore.Restore(exportedWithExtraBudget)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	wantDiscard := RestoreDiscard{Agent: "agent-b", BudgetIndex: 1, Field: "budget_index"}
	if len(extraBudgetReport.Discards) != 1 || extraBudgetReport.Discards[0] != wantDiscard {
		t.Fatalf("discards = %+v, want exactly [%+v]", extraBudgetReport.Discards, wantDiscard)
	}
	if extraBudgetReport.RestoredBudgets != 1 {
		t.Fatalf("restored = %d, want 1 (the token budget at index 0)", extraBudgetReport.RestoredBudgets)
	}
}

func TestRestore_RollingUsageAgesAcrossDowntime(t *testing.T) {
	start := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	sourceClock := func() time.Time { return start }
	source, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, sourceClock)
	_ = source.TrackMulti("agent-a", []int64{700, 0})
	exported := source.Export()

	// Restart 30 minutes later: usage still inside the 1h rolling window.
	after30 := start.Add(30 * time.Minute)
	restored30, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, func() time.Time { return after30 })
	if _, err := restored30.Restore(exported); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	statuses, _ := restored30.StatusAll("agent-a")
	if statuses[0].Used != 700 {
		t.Fatalf("30m later Used = %d, want 700 (still in window)", statuses[0].Used)
	}

	// Restart 2 hours later: usage aged out arithmetically.
	after2h := start.Add(2 * time.Hour)
	restored2h, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, func() time.Time { return after2h })
	if _, err := restored2h.Restore(exported); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	statuses, _ = restored2h.StatusAll("agent-a")
	if statuses[0].Used != 0 {
		t.Fatalf("2h later Used = %d, want 0 (aged out)", statuses[0].Used)
	}
}

func TestRestore_FixedWindowCatchesUpDuringRestore(t *testing.T) {
	start := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	source, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, func() time.Time { return start })
	_ = source.TrackMulti("agent-a", []int64{0, 4_500_000})
	exported := source.Export()

	// Restart two days later: the daily fixed window crossed two boundaries,
	// so committed dollars must be zero immediately after Restore.
	twoDays := start.Add(48 * time.Hour)
	restored, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, func() time.Time { return twoDays })
	if _, err := restored.Restore(exported); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	statuses, _ := restored.StatusAll("agent-a")
	if statuses[1].Used != 0 {
		t.Fatalf("dollar Used after two-day downtime = %d, want 0", statuses[1].Used)
	}
}

func TestRestore_BucketCollisionSumsSaturating(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	restored, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)

	// Two saved buckets a full ring cycle apart map to the SAME slot. The
	// restore must sum, never overwrite, and keep the newer epoch.
	width := int64(60) // 1h window over 60 buckets
	ringSpanSeconds := width * 60
	olderEpoch := (now.Unix()/width)*width - ringSpanSeconds
	newerEpoch := (now.Unix() / width) * width
	saved := map[string]AgentSnapshot{"agent-a": {Budgets: []BudgetSnapshot{
		{
			Unit: "tokens", WindowType: "rolling", WindowSeconds: 3600, BucketCount: 60,
			Buckets: []BucketSnapshot{
				{EpochStart: olderEpoch, Amount: 100},
				{EpochStart: newerEpoch, Amount: 200},
			},
		},
		exportedFixedZero(),
	}}}
	report, err := restored.Restore(saved)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Discards) != 0 {
		t.Fatalf("unexpected discards: %+v", report.Discards)
	}
	statuses, _ := restored.StatusAll("agent-a")
	// The summed slot carries the newer epoch, so the whole 300 counts
	// (over-counting duration is the safe direction, dropping is forbidden).
	if statuses[0].Used != 300 {
		t.Fatalf("Used = %d, want 300 (collision summed under newer epoch)", statuses[0].Used)
	}
}

// TestRestore_BucketCollisionNewerFedFirstKeepsNewerEpoch is the mutation-
// closing sibling to TestRestore_BucketCollisionSumsSaturating. That test
// feeds its colliding buckets older-then-newer, so the bucket being merged in
// is always the newer one and restoreWindow's "keep the newer epoch" branch
// (`if existing.EpochStart > merged.EpochStart`) happens to already hold the
// right value without that branch's body ever running (confirmed empirically
// below by deleting the branch and re-running the suite: that test alone
// still passed). Feeding the SAME two buckets newer-then-older instead makes
// the bucket being merged in the older one on the second iteration, so the
// branch must actively fire to overwrite merged.EpochStart back to the newer
// value.
//
// The far-past epoch (two ring cycles back, not one) matters too: at exactly
// one ring cycle old, used()'s trailing-edge over-count tolerance counts a
// bucket as live under EITHER epoch, which is why
// TestRestore_BucketCollisionSumsSaturating's own Used-equals-300 assertion
// does not actually depend on which epoch won. Two ring cycles back is
// unambiguously stale (its live-check fails even with the +bucketWidthSec
// slack), so Used reads 300 only if the newer epoch survived the merge, and
// 0 if the far-past epoch did.
func TestRestore_BucketCollisionNewerFedFirstKeepsNewerEpoch(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	fakeClockFunc := func() time.Time { return now }
	restored, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, fakeClockFunc)

	width := int64(60) // 1h window over 60 buckets
	ringSpanSeconds := width * 60
	newerEpoch := (now.Unix() / width) * width
	farPastEpoch := newerEpoch - 2*ringSpanSeconds // two cycles back: unambiguously stale
	saved := map[string]AgentSnapshot{"agent-a": {Budgets: []BudgetSnapshot{
		{
			Unit: "tokens", WindowType: "rolling", WindowSeconds: 3600, BucketCount: 60,
			Buckets: []BucketSnapshot{
				{EpochStart: newerEpoch, Amount: 200},
				{EpochStart: farPastEpoch, Amount: 100},
			},
		},
		exportedFixedZero(),
	}}}
	report, err := restored.Restore(saved)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Discards) != 0 {
		t.Fatalf("unexpected discards: %+v", report.Discards)
	}
	statuses, _ := restored.StatusAll("agent-a")
	// If the merge had kept the far-past epoch instead, this bucket would
	// already be stale and Used would read 0, not 300.
	if statuses[0].Used != 300 {
		t.Fatalf("Used = %d, want 300 (collision summed under the newer epoch even when fed second)", statuses[0].Used)
	}
}

func TestRestore_FutureEpochsCountConservatively(t *testing.T) {
	// Clock stepped backward across a restart: saved buckets carry epochs in
	// the local future. used() must COUNT them (over-count, the safe
	// direction), never drop them.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	restored, _ := NewStore(snapshotTestAgents(), DefaultStreamLimit, func() time.Time { return now })

	futureEpoch := ((now.Unix() / 60) * 60) + 600 // ten minutes ahead
	saved := map[string]AgentSnapshot{"agent-a": {Budgets: []BudgetSnapshot{
		{
			Unit: "tokens", WindowType: "rolling", WindowSeconds: 3600, BucketCount: 60,
			Buckets: []BucketSnapshot{{EpochStart: futureEpoch, Amount: 400}},
		},
		exportedFixedZero(),
	}}}
	report, err := restored.Restore(saved)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(report.Discards) != 0 {
		t.Fatalf("unexpected discards: %+v", report.Discards)
	}
	statuses, _ := restored.StatusAll("agent-a")
	if statuses[0].Used != 400 {
		t.Fatalf("Used = %d, want 400 (future usage must count)", statuses[0].Used)
	}
}

// exportedFixedZero builds an identity-matching zero-usage fixed dollar
// budget snapshot for tests that hand-build the rolling half.
//
// WindowStart is hardcoded to midnight UTC on 2026-09-09. Every test in this
// file that calls this helper shares the same noon-2026-09-09 (or later)
// fake clock, so that boundary is always the correct currentBoundary for
// them. A future test using a different `now` must not reuse this helper
// without checking the boundary still matches its own clock.
func exportedFixedZero() BudgetSnapshot {
	return BudgetSnapshot{
		Unit: "dollars", WindowType: "fixed", WindowSeconds: 86400, ResetAt: "00:00Z",
		WindowStart: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), Committed: 0,
	}
}
