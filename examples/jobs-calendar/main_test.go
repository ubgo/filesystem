package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestRun checks the output the maintenance guide's calendar section
// points at.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"filesystem.upload_sweep: next run 2026-10-10T03:00:00Z", // the next 03:00 after day 2, 14:30
		"filesystem.sweep_abandoned: next run 2026-10-10T03:00:00Z",
		"app.trim_thumb_cache: 1, err <nil>", // 15 bytes over a 10-byte budget: one thumbnail goes
		"filesystem.sweep_abandoned: 1, err <nil>, dry run: multipart:",
		"filesystem.upload_sweep: 1, err <nil>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestNoRecords pins the point of the example: maintenance without a
// database imports diskjobs, never jobs or records.
func TestNoRecords(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var sawDiskjobs bool
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		switch {
		case p == "github.com/ubgo/filesystem/jobs/diskjobs":
			sawDiskjobs = true
		case p == "github.com/ubgo/filesystem/jobs", strings.HasPrefix(p, "github.com/ubgo/filesystem/records"):
			t.Errorf("main.go imports %s; this example runs without records", p)
		}
	}
	if !sawDiskjobs {
		t.Error("main.go does not import diskjobs; the import scan would pass vacuously")
	}
}
