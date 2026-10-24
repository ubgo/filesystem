package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the output the maintenance guide shows.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	want := `dry run: would purge 1 file(s)
sweep: purged 1 file(s)
scrub: report.txt changed
scrub: checked 2 file(s)
migrate: moved 2 file(s) to cold
sync dry run: 2 to copy, 1 to delete
sync: copied 2, deleted 1, unchanged 0
export: 2 file(s), 43 bytes, link valid until 9:15AM
export sweep: deleted 1 expired export(s)
`
	if got := out.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, strings.TrimSpace(want))
	}
}
