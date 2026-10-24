package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun checks the output the maintenance-jobs guide shows.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"filesystem.audit_prune: 0, err <nil>", // entries are 60 days old, retention is 90
		"filesystem.sweep_abandoned: 1, err <nil>",
		"filesystem.sweep_orphans: 1, err <nil>",
		"filesystem.sweep_trash: 1, err <nil>",
		"filesystem.upload_sweep: 1, err <nil>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
