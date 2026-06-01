package types

import "testing"

func TestReservationIDIsUint64(t *testing.T) {
	var id ReservationID = 42
	if uint64(id) != 42 {
		t.Fatalf("expected ReservationID to convert to uint64 42, got %d", uint64(id))
	}
	// The zero value is the invalid/sentinel reservation per 001-error-handling.md.
	var zero ReservationID
	if zero != 0 {
		t.Fatalf("expected zero value to be 0, got %d", zero)
	}
}
