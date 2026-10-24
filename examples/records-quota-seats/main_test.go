package main

import (
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"two uploads:             alice 30/40, bob 25/40, team 55/100 bytes",
		`alice past her seat:     filesystem: quota_exceeded: quota: uploader "user:alice" in scope "team-acme" would exceed its limit of 40 bytes`,
		"bob grew alice's file:   alice 38/40, bob 25/40, team 63/100 bytes",
		"recount:                 team 63 bytes, 2 files",
		"after the recount:       alice 38/40, bob 25/40, team 63/100 bytes",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
