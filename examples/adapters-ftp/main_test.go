package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against a fresh in-process FTP server
// and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		// The in-process server writes to the host's temp dir, so the
		// driver's probe adds case-insensitive on macOS and not on Linux.
		"capabilities: [move,delete-prefix,sweep-abandoned,find-abandoned,dirs",
		"uploaded: outbound/orders.csv 11 bytes text/csv; charset=utf-8",
		"six concurrent uploads over a pool of three connections: ok",
		"listed: archive/2026-10/orders.csv",
		"copied by streaming through the client: outbound/orders-resend.csv 11 bytes",
		"objects: 8",
		"made returns/2026: exists=true; after deleting its only file, returns exists=false",
		"conditional write: unsupported",
		"metadata: unsupported",
		"reserved key: invalid_key",
		"abandoned: staging bbbbbbbbbbbbbbbbbbbbbbbbbb\nswept: 1\n",
		"refused DSN (password in the URL): invalid_argument",
		"refused DSN (max_conns=0): invalid_argument",
		"refused DSN (no root directory): invalid_argument",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
