package acl

import (
	"slices"
	"testing"
)

// TestCmpDepth pins the order a precedence sees matches in: nearest first
// and ContainerDepth after every folder, on either side of the compare.
func TestCmpDepth(t *testing.T) {
	depths := []int{ContainerDepth, 7, 0, ContainerDepth, 2}
	slices.SortFunc(depths, cmpDepth)
	if want := []int{0, 2, 7, ContainerDepth, ContainerDepth}; !slices.Equal(depths, want) {
		t.Fatalf("sorted %v, want %v", depths, want)
	}
	for _, c := range []struct{ a, b, want int }{
		{ContainerDepth, 5, 1}, {5, ContainerDepth, -1}, {ContainerDepth, ContainerDepth, 0}, {1, 2, -1},
	} {
		if got := cmpDepth(c.a, c.b); got != c.want {
			t.Errorf("cmpDepth(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
