package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pickers"
)

func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "-> 200") || !strings.Contains(got, `"type":"application/pdf"`) || !strings.Contains(got, "invoice-2026-10.pdf") {
		t.Fatalf("output: %s", got)
	}
}

// TestOwnSource pins the custom source: it imports with the right token,
// maps a refused token to permission, and refuses an ID outside the
// vendor's alphabet before any request.
func TestOwnSource(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"box import: contract.pdf application/pdf 29 bytes", `box import with token "exp…": permission`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	_, err := boxSource{API: "https://box.example"}.Stat(context.Background(), nil, pickers.Pick{ID: "../admin"})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Stat of a crafted ID = %v, want invalid_argument", err)
	}
}
