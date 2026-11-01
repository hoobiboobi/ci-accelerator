package store

import "testing"

// An id a signed column cannot hold is stored as NULL, never as a negative
// number that breaks every later read of the table (the pipelines list
// scans ids as uint64).
func TestNullUintRange(t *testing.T) {
	for v, valid := range map[uint64]bool{0: false, 1: true, 1<<63 - 1: true, 1 << 63: false, ^uint64(0): false} {
		if got := nullUint(v); got.Valid != valid || (valid && got.Int64 < 0) {
			t.Errorf("nullUint(%d) = %+v, want valid %v", v, got, valid)
		}
	}
}
