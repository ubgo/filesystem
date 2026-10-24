package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against a fresh in-process WebDAV server
// and checks the lines the guide quotes.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		// The in-process server writes to the host's temp dir, so the
		// driver's probe adds case-insensitive on macOS and not on Linux.
		"capabilities: [copy,move,delete-prefix,sweep-abandoned,find-abandoned,dirs",
		"stored: reports/2026-10.csv 18 bytes text/csv; charset=utf-8",
		"listed: archive/2026/10.csv\nlisted: shared/latest.csv\n",
		"made inbox/2026: exists=true, listed entries=0",
		"after deleting its last file, shared exists=true",
		"metadata: unsupported",
		"if-not-exists: unsupported",
		"reserved key: invalid_key",
		"abandoned: staging aaaaaaaaaaaaaaaaaaaaaaaaaa\nswept: 1\n",
		"refused DSN (password in the URL): invalid_argument",
		"refused DSN (plain http off loopback): invalid_argument",
		"refused DSN (password_env without a user): invalid_argument",
		`webdav.password=[redacted]`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), davPassword) {
		t.Errorf("the password reached the output:\n%s", out.String())
	}
}
