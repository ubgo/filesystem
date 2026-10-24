package main

import (
	"strings"
	"testing"
)

// TestRun runs the demo and checks every step's outcome.
func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"attach 4 photos to a gallery of 3: quota_exceeded",
		"the post's PDFs: 1 (docs/report.pdf)",
		"gallery pages: [[photos/1.png photos/2.png] [photos/3.png]]",
		"content_type: image/png 4 (84 B), application/pdf 2 (29 B)",
		"visibility: private 5 (92 B), public 1 (21 B)",
		"folder: (root) 4 (71 B), Trip 2 (42 B)",
		"trash 3 selected: ok, not_found, ok",
		"orphans: [tmp/left-behind.bin]; rows without an object: [docs/other.pdf]",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
