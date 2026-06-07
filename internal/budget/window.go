package budget

import (
	"time"

	"github.com/levee-ai/levee/pkg/types"
)

// ringBucket holds the committed amount for one sub-interval of a rolling
// window. EpochStart is the unix-second start of the interval this slot
// currently represents. A slot whose EpochStart no longer matches the current
// sub-interval is stale and treated as empty.
type ringBucket struct {
	EpochStart int64
	Amount     int64
}

// budgetWindow tracks committed usage and active reservations for one budget
// (tokens or dollars) over one window. Rolling windows use the bucket ring.
// Fixed windows use windowStart plus a wall-clock boundary. Amounts are int64
// in that budget's unit (tokens or cents). The store holds the agent mutex
// while calling these methods, so budgetWindow itself is not synchronized.
type budgetWindow struct {
	WindowType types.WindowType
	WindowSize time.Duration
	Limit      int64
	reserved   int64 // Sum of active holds, adjusted by the store.

	now clock

	// Rolling fields.
	buckets        []ringBucket
	bucketWidthSec int64

	// Fixed fields.
	resetHour      int
	resetMinute    int
	windowStart    time.Time
	committedFixed int64
}

// newRollingWindow builds a rolling window with bucketCount slots over
// windowSize. bucketCount must be >= 1. The bucket width is rounded UP so the
// ring always spans at least windowSize seconds. Rounding down would make the
// ring cover less than the window and silently drop in-window usage, an
// under-count in the forbidden direction. Rounding up widens the trailing-edge
// over-count slightly (the safe never-under-count direction) for windows whose
// second count is not a multiple of bucketCount (60 over 1h divides evenly and
// is unaffected).
func newRollingWindow(limit int64, windowSize time.Duration, bucketCount int, now clock) *budgetWindow {
	windowSeconds := int64(windowSize.Seconds())
	width := (windowSeconds + int64(bucketCount) - 1) / int64(bucketCount)
	if width < 1 {
		width = 1
	}
	return &budgetWindow{
		WindowType:     types.WindowRolling,
		WindowSize:     windowSize,
		Limit:          limit,
		now:            now,
		buckets:        make([]ringBucket, bucketCount),
		bucketWidthSec: width,
	}
}

// newFixedWindow builds a fixed window resetting at resetAt ("HH:MMZ", UTC).
// resetAt is pre-validated by config, so parsing here cannot fail in practice.
func newFixedWindow(limit int64, windowSize time.Duration, resetAt string, now clock) *budgetWindow {
	hour, minute := parseResetAt(resetAt)
	window := &budgetWindow{
		WindowType:  types.WindowFixed,
		WindowSize:  windowSize,
		Limit:       limit,
		now:         now,
		resetHour:   hour,
		resetMinute: minute,
	}
	window.windowStart = window.currentBoundary(now().UTC())
	return window
}

// parseResetAt extracts hour and minute from "HH:MMZ". Config validation
// guarantees the format, so a malformed string defaults to midnight.
func parseResetAt(resetAt string) (hour, minute int) {
	// Format is exactly HH:MMZ, e.g. "00:00Z".
	if len(resetAt) != 6 || resetAt[2] != ':' || resetAt[5] != 'Z' {
		return 0, 0
	}
	hour = int(resetAt[0]-'0')*10 + int(resetAt[1]-'0')
	minute = int(resetAt[3]-'0')*10 + int(resetAt[4]-'0')
	return hour, minute
}

// currentBoundary returns the most recent reset instant at or before now.
func (window *budgetWindow) currentBoundary(now time.Time) time.Time {
	year, month, day := now.Date()
	candidate := time.Date(year, month, day, window.resetHour, window.resetMinute, 0, 0, time.UTC)
	if candidate.After(now) {
		candidate = candidate.AddDate(0, 0, -1)
	}
	return candidate
}

// bucketStart truncates an instant to the start of its sub-interval.
func (window *budgetWindow) bucketStart(now time.Time) int64 {
	return (now.Unix() / window.bucketWidthSec) * window.bucketWidthSec
}

// used returns committed usage currently inside the window.
func (window *budgetWindow) used() int64 {
	if window.WindowType == types.WindowFixed {
		window.maybeReset()
		return window.committedFixed
	}
	now := window.now()
	cutoff := now.Unix() - int64(window.WindowSize.Seconds())
	var total int64
	for i := range window.buckets {
		bucket := window.buckets[i]
		// Live if the bucket's interval END is still after the cutoff.
		if bucket.EpochStart+window.bucketWidthSec > cutoff {
			total += bucket.Amount
		}
	}
	return total
}

// remaining returns limit minus committed usage minus active reservations.
// It can be negative.
func (window *budgetWindow) remaining() int64 {
	return window.Limit - window.used() - window.reserved
}

// commit records settled usage at the current time.
func (window *budgetWindow) commit(amount int64) {
	if window.WindowType == types.WindowFixed {
		window.maybeReset()
		window.committedFixed += amount
		return
	}
	now := window.now()
	start := window.bucketStart(now)
	slot := (start / window.bucketWidthSec) % int64(len(window.buckets))
	if window.buckets[slot].EpochStart != start {
		// Slot rotated to a new interval (or first use). Reset before adding.
		window.buckets[slot] = ringBucket{EpochStart: start, Amount: 0}
	}
	window.buckets[slot].Amount += amount
}

// maybeReset advances a fixed window across any boundaries that have passed,
// zeroing committed usage. Skips intermediate windows after a long downtime.
func (window *budgetWindow) maybeReset() {
	now := window.now().UTC()
	for !now.Before(window.windowStart.Add(window.WindowSize)) {
		window.windowStart = window.windowStart.Add(window.WindowSize)
		window.committedFixed = 0
	}
}
