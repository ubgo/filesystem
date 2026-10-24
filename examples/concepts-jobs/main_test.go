package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun runs every job and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"before promote: read from staging; filesystem.promote moved 1; after: read from store",
		"dry run: would delete 2: incoming/a.png incoming/b.png",
		"sweep: deleted 2, files/kept.png still there: true",
		"trim-media-cache: 1 trimmed; a second sweep named sweep-exports",
		"sweep with minAge 0: invalid_argument",
		"two schedules with one name: invalid_argument",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
