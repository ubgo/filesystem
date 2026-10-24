package webp

import (
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
)

// TestMethodFor pins the effort scale's ends and order.
func TestMethodFor(t *testing.T) {
	if methodFor(1) != 0 || methodFor(imaging.MaxEffort) != maxMethod {
		t.Fatalf("ends: %d %d", methodFor(1), methodFor(imaging.MaxEffort))
	}
	for e := 2; e <= imaging.MaxEffort; e++ {
		if methodFor(e) < methodFor(e-1) {
			t.Fatalf("effort %d gives method %d, below effort %d's", e, methodFor(e), e-1)
		}
	}
}
