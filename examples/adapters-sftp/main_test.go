package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against a fresh in-process SSH server
// and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		// The in-process server writes to the host's temp dir, so the
		// driver's probe adds case-insensitive on macOS and not on Linux.
		"capabilities: [move,set-visibility,delete-prefix,sweep-abandoned,find-abandoned,dirs,conditional-write",
		"second conditional write: exists",
		"stat: outbound/orders.csv 14 bytes private (type from extension: text/csv; charset=utf-8)",
		"listed: archive/2026-10/orders.csv",
		"metadata: unsupported",
		"headers: unsupported",
		"via Config: archived file exists=true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "listed: outbound") {
		t.Errorf("the moved file is still listed:\n%s", out.String())
	}
}
