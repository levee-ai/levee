// Package budget implements the Reserve/Reconcile/Forfeit budget store.
package budget

import "time"

// clock returns the current time. Production uses time.Now. Tests inject a
// fake so window boundaries and rolling expiry are deterministic. Rolling
// windows key their buckets by UTC epoch (not a stored monotonic reading),
// so a wall-clock fake is sufficient and snapshot reloads stay correct.
type clock func() time.Time

// systemClock is the production clock.
func systemClock() time.Time { return time.Now() }
