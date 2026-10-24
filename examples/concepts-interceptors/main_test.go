package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the behaviour of every interceptor the page describes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  enter outer (stat a.txt)\n  enter inner (stat a.txt)\n  leave inner\n  leave outer",
		`observed write a.txt: attempts=3 code=""`,
		`observed stat a.txt: attempts=3 code=""`,
		`observed write b.txt: attempts=1 code="unavailable"`,
		"  write: unavailable",
		`observed stat a.txt: attempts=3 code="unavailable"`,
		"deadline exceeded: true",
		"limit: second caller unavailable",
		"read-only write: read_only",
		"read-only exists: <nil>",
		"guard inside: <nil>",
		"guard outside: permission",
		`level=DEBUG msg="filesystem operation" disk=media op=write key=a.txt attempts=1 bytes=5`,
		`level=DEBUG msg="filesystem operation" disk=media op=stat key=missing.txt attempts=1 code=not_found`,
		`level=WARN msg="filesystem operation" disk=media op=set_visibility key=missing.txt attempts=1 code=not_found`,
		"bandwidth: 6000 bytes at 20000 B/s took at least 150ms: true",
		"fault without retry: unavailable",
		"fault inside retry: 20 writes stored in 43 attempts",
		`hedge: read "answered" after 2 driver reads`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
