package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks each ordering rule the consistency page states.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"rolled back upload: true; objects 0, rows 0",
		"record a missing object: not_found",
		"purge rolled back: true; objects 1, rows 1",
		"purge committed: objects 0, rows 0",
		"orphans deleted [uploads/crashed.pdf]; objects 2, rows 1",
		"second IfNotExists write: exists",
		"mirror down: unavailable; primary true, replica false",
		"written again: replica true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
