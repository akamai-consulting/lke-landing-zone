package linode

import "testing"

// floorDiv / floorMod implement Euclidean division so pre-epoch (negative)
// timestamps format correctly — the negative-operand branch is the one the
// civil-date round-trip tests don't otherwise reach.
func TestFloorDivMod(t *testing.T) {
	cases := []struct {
		a, b, wantDiv, wantMod int64
	}{
		{7, 3, 2, 1},   // positive
		{-7, 3, -3, 2}, // negative dividend → round toward -inf, non-negative mod
		{-6, 3, -2, 0}, // exact negative → no adjustment
		{6, 3, 2, 0},   // exact positive
	}
	for _, c := range cases {
		if got := floorDiv(c.a, c.b); got != c.wantDiv {
			t.Errorf("floorDiv(%d,%d) = %d, want %d", c.a, c.b, got, c.wantDiv)
		}
		if got := floorMod(c.a, c.b); got != c.wantMod {
			t.Errorf("floorMod(%d,%d) = %d, want %d", c.a, c.b, got, c.wantMod)
		}
	}
}
