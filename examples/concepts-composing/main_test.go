package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun runs every composition and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`route: avatars disk holds "u1.png", routed listing "avatars/u1.png exports/2026-10.zip notes.txt", avatar url https://cdn.example.com/u1.png`,
		"route: copy across mounts: <nil>",
		"failover: read logo.png moved from primary to replica",
		`failover: read "logo" while the primary is down`,
		"failover: write during the outage: unavailable",
		`overlay: preview reads "edited in preview", lists "a.txt"; production still lists "a.txt b.txt"`,
		"cache: 3 reads, 1 reached the bucket",
		`cache: after a write, read "final cut" (bucket reads: 2)`,
		"cache: trimmed 1 copy to stay under 8 bytes",
		"batch: missing-1.txt failed: not_found",
		"batch: missing-2.txt failed: not_found",
		`batch: copied "a.txt b.txt c.txt"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
