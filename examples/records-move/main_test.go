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
	want := strings.Join([]string{
		`uploaded 3 notes to "incoming"`,
		"promoted 3",
		`first note: disk=kept, still on incoming=false, reads "first note"`,
		"promote again: 0, <nil>",
	}, "\n") + "\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}
