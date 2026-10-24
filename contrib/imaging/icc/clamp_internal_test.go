package icc

import (
	"math"
	"testing"
)

// TestClampUnit pins the defence in depth behind Parse's checks: NaN and
// infinities never become a table index.
func TestClampUnit(t *testing.T) {
	for in, want := range map[float64]float64{math.NaN(): 0, math.Inf(1): 1, math.Inf(-1): 0, -0.5: 0, 0.25: 0.25, 2: 1} {
		if got := clampUnit(in); got != want {
			t.Errorf("clampUnit(%v) = %v, want %v", in, got, want)
		}
	}
}
