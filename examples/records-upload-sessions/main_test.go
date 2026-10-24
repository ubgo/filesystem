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
		"issued ups_… for prefix users/alice until 09:15",
		"with the token:                201 users/alice/…",
		"no token:                      403",
		"forged secret:                 403",
		"over the session's 64 bytes:   413",
		"a PNG the endpoint allows:     415",
		"another instance:              201 users/alice/…",
		"after the expiry:              403",
		"sweep deleted 1 expired session(s)",
		"revoked:                       403",
		"issue wider than the endpoint: invalid_argument",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
