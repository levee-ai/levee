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
	// in full, which is the settled safe over-count direction (spec Section 4).
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
