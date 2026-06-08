package budget

import (
	"testing"
	"time"

	"github.com/levee-ai/levee/pkg/types"
)

// fakeClock returns a controllable time.
type fakeClock struct{ now time.Time }

func (f *fakeClock) advance(d time.Duration) { f.now = f.now.Add(d) }
func (f *fakeClock) read() time.Time         { return f.now }

func baseTime() time.Time {
	return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
}

func TestRollingWindowUsedAndRemaining(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(1000, time.Hour, 60, fake.read)

	window.commit(300)
	if used := window.used(); used != 300 {
		t.Fatalf("used: got %d, want 300", used)
	}
	if remaining := window.remaining(); remaining != 700 {
		t.Fatalf("remaining: got %d, want 700", remaining)
	}
}

func TestRollingWindowExpiresOldUsage(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(1000, time.Hour, 60, fake.read)

	window.commit(400)      // At t=0, lands in the minute-0 bucket [12:00, 12:01).
	fake.advance(time.Hour) // Exactly one window later.
	// Advance one full bucket width so the minute-0 bucket's END (12:01) reaches
	// the trailing cutoff and ages out. A smaller nudge leaves the bucket counted
	// in full, which is the settled safe over-count direction.
	fake.advance(time.Minute)
	if used := window.used(); used != 0 {
		t.Fatalf("used after expiry: got %d, want 0", used)
	}
}

func TestRollingWindowBoundaryBucketOverCounts(t *testing.T) {
	// A bucket only partially inside the trailing window is still counted in
	// full. That over-counts, which is the safe never-under-count direction.
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(1000, time.Hour, 60, fake.read) // 60 buckets of 1 min

	window.commit(100) // lands in the minute-0 bucket
	fake.advance(59*time.Minute + 30*time.Second)
	// minute-0 bucket is ~30s from fully aging out, still counted in full.
	if used := window.used(); used != 100 {
		t.Fatalf("boundary bucket: got %d, want 100 (over-count is safe)", used)
	}
}

func TestRollingWindowStaleWrappedBucketIsZero(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(600, time.Hour, 60, fake.read)

	window.commit(999)      // minute-0 slot
	fake.advance(time.Hour) // same slot index reused one window later
	window.commit(5)        // must zero the stale 999 first, then add 5
	if used := window.used(); used != 5 {
		t.Fatalf("stale wrapped bucket: got %d, want 5", used)
	}
}

func TestRollingWindowIdleGapLongerThanWindow(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(1000, time.Hour, 60, fake.read)

	window.commit(700)
	fake.advance(5 * time.Hour) // idle far longer than the whole window
	if used := window.used(); used != 0 {
		t.Fatalf("all buckets should be stale: got %d, want 0", used)
	}
	// And a fresh commit works cleanly afterward.
	window.commit(50)
	if used := window.used(); used != 50 {
		t.Fatalf("post-idle commit: got %d, want 50", used)
	}
}

func TestRollingWindowNonDivisorDurationNeverUnderCounts(t *testing.T) {
	// A 90s window over 60 buckets does not divide evenly (90/60 = 1.5). The
	// bucket width must round UP to 2s so the ring spans at least 90s. Rounding
	// down to 1s would make the ring cover only 60s and silently drop usage
	// committed between 60s and 90s ago, the forbidden under-count direction.
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(100000, 90*time.Second, 60, fake.read)

	window.commit(500) // At t=0.
	// Commit one token per second for 65 seconds. The t=0 commit is still inside
	// the 90s window the whole time, so used() must never lose it.
	for i := 0; i < 65; i++ {
		fake.advance(time.Second)
		window.commit(1)
	}
	// Truth: 500 + 65 = 565, all inside the trailing 90s. Over-count is allowed,
	// under-count is forbidden.
	if used := window.used(); used < 565 {
		t.Fatalf("non-divisor window under-counted: got %d, want >= 565", used)
	}
}

func TestFixedWindowResetsAtBoundary(t *testing.T) {
	// reset_at 00:00Z, 24h window. Start at 12:00Z (mid-window).
	fake := &fakeClock{now: baseTime()}
	window := newFixedWindow(1000, 24*time.Hour, "00:00Z", fake.read)

	window.commit(800)
	if used := window.used(); used != 800 {
		t.Fatalf("pre-reset used: got %d, want 800", used)
	}
	fake.advance(12 * time.Hour) // now 2026-06-01 00:00:00Z exactly
	fake.advance(time.Second)    // strictly past the boundary
	if used := window.used(); used != 0 {
		t.Fatalf("post-reset used: got %d, want 0", used)
	}
}

func TestFixedWindowAdvancesMultipleMissedWindows(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newFixedWindow(1000, 24*time.Hour, "00:00Z", fake.read)

	window.commit(500)
	fake.advance(72 * time.Hour) // process "down" 3 days
	if used := window.used(); used != 0 {
		t.Fatalf("after multi-window gap: got %d, want 0", used)
	}
}

func TestWindowTypeTag(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	rolling := newRollingWindow(1, time.Hour, 60, fake.read)
	fixed := newFixedWindow(1, 24*time.Hour, "00:00Z", fake.read)
	if rolling.WindowType != types.WindowRolling {
		t.Fatalf("rolling tag: got %q", rolling.WindowType)
	}
	if fixed.WindowType != types.WindowFixed {
		t.Fatalf("fixed tag: got %q", fixed.WindowType)
	}
}

func TestRecoveryTime_Fixed_ReturnsNextBoundary(t *testing.T) {
	fake := &fakeClock{now: baseTime()} // 2026-05-31 12:00:00 UTC
	window := newFixedWindow(1000, 24*time.Hour, "00:00Z", fake.read)
	window.commit(1000) // full
	// Next 00:00Z boundary after 12:00 on 05-31 is 2026-06-01 00:00:00 UTC.
	got := window.recoveryTime(1)
	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("recoveryTime: got %s, want %s", got, want)
	}
}

func TestRecoveryTime_Rolling_FrontLoadedBucketExits(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(1000, time.Hour, 60, fake.read) // 60 buckets of 1 min
	window.commit(900)                                         // lands in the current minute bucket
	// Request for 200 needs target = 1000 - 0 - 200 = 800. used is 900, so the
	// 900 bucket must exit. It exits one full window after its interval end.
	// bucketWidth=60s, so exit = bucketStart + 60 + 3600 from the commit instant.
	got := window.recoveryTime(200)
	if !got.After(fake.read()) {
		t.Fatalf("recoveryTime should be in the future, got %s vs now %s", got, fake.read())
	}
	// The single 900 bucket exits within (window + one bucket) of now.
	maxExpected := fake.read().Add(time.Hour + time.Minute)
	if got.After(maxExpected) {
		t.Fatalf("recoveryTime %s later than one window+bucket from now %s", got, maxExpected)
	}
}

func TestRecoveryTime_Rolling_AmountExceedsLimit_ReturnsNowPlusWindow(t *testing.T) {
	fake := &fakeClock{now: baseTime()}
	window := newRollingWindow(1000, time.Hour, 60, fake.read)
	window.commit(100)
	// amount 5000 > limit 1000: aging can never satisfy it. Fall back to now+window.
	got := window.recoveryTime(5000)
	want := fake.read().Add(time.Hour)
	if !got.Equal(want) {
		t.Fatalf("recoveryTime: got %s, want now+window %s", got, want)
	}
}

func TestRecoveryTime_Rolling_MultiBucketWalk(t *testing.T) {
	fake := &fakeClock{now: baseTime()} // 2026-05-31 12:00:00 UTC, on a minute boundary
	window := newRollingWindow(1000, time.Hour, 60, fake.read)
	window.commit(300) // minute-0 bucket, EpochStart = baseTime
	fake.advance(time.Minute)
	window.commit(300) // minute-1 bucket, EpochStart = baseTime + 60
	fake.advance(time.Minute)
	window.commit(300) // minute-2 bucket, EpochStart = baseTime + 120, now = baseTime + 120

	// used is 900 across three live buckets. For amount 600, target = 1000 - 0 - 600
	// = 400. The walk subtracts buckets in ascending exit order: clearing the
	// minute-0 bucket leaves 600 (still above 400), clearing the minute-1 bucket too
	// leaves 300 (at or below 400). The minute-1 bucket is the tipping point, so
	// recoveryTime is that bucket's exit instant: its EpochStart plus one bucket
	// width plus one full window, i.e. baseTime + 60 + 60 + 3600 seconds.
	got := window.recoveryTime(600)
	base := baseTime().Unix()
	tippingBucketStart := base + 60
	want := time.Unix(tippingBucketStart+60+3600, 0).UTC() // 2026-05-31 13:02:00 UTC
	if !got.Equal(want) {
		t.Fatalf("recoveryTime: got %s, want %s", got, want)
	}
}
