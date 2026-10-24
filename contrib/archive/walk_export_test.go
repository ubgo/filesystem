package archive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/archive"
)

// TestWalk pins the exported walk: a custom visitor sees every entry with
// its bytes, nested entries included; its error stops the walk and is
// returned; and the limits run whatever the visitor does, so a visitor
// that reads nothing cannot let a bomb through.
func TestWalk(t *testing.T) {
	b := zipOf(t,
		fixture{name: "a.txt", body: []byte("alpha")},
		fixture{name: "inner.zip", body: zipOf(t, fixture{name: "b.txt", body: []byte("beta")})},
	)
	got := map[string]string{}
	err := archive.Walk(t.Context(), bytes.NewReader(b), int64(len(b)), archive.Limits{}, func(_ context.Context, e archive.Entry, content io.Reader) error {
		body, err := io.ReadAll(content)
		got[e.Path()] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["a.txt"] != "alpha" || got["inner.zip!/b.txt"] != "beta" || len(got) != 3 {
		t.Fatalf("visited %v", got)
	}

	stop := errors.New("stop")
	calls := 0
	err = archive.Walk(t.Context(), bytes.NewReader(b), int64(len(b)), archive.Limits{}, func(context.Context, archive.Entry, io.Reader) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("a visitor error: got %v after %d calls", err, calls)
	}

	bomb := zipOf(t, fixture{name: "zeros.bin", body: zeros})
	err = archive.Walk(t.Context(), bytes.NewReader(bomb), int64(len(bomb)), archive.Limits{}, func(context.Context, archive.Entry, io.Reader) error { return nil })
	wantRejected(t, err, archive.ErrLimit)
}

// TestExtractSkip pins ExtractOptions.Skip: skipped entries are neither
// stored nor returned, the rest are; and a skipped entry still passes
// through the limits, so Skip cannot let a bomb through.
func TestExtractSkip(t *testing.T) {
	debris := func(e archive.Entry) bool {
		return strings.HasPrefix(e.Name, "__MACOSX/") || strings.HasSuffix(e.Name, ".DS_Store")
	}
	b := zipOf(t,
		fixture{name: "a.txt", body: []byte("alpha")},
		fixture{name: "__MACOSX/._a.txt", body: []byte("fork")},
		fixture{name: "dir/.DS_Store", body: []byte("ds")},
	)
	d := newDisk(t)
	got, err := archive.Extract(t.Context(), d, "in", bytes.NewReader(b), int64(len(b)), archive.ExtractOptions{Skip: debris})
	if err != nil {
		t.Fatal(err)
	}
	if keys := keysOn(t, d); !slices.Equal(keys, []string{"in/a.txt"}) || len(got) != 1 {
		t.Fatalf("stored %v, returned %+v", keys, got)
	}
	// The default stores every file.
	all := newDisk(t)
	if _, err := archive.Extract(t.Context(), all, "in", bytes.NewReader(b), int64(len(b)), archive.ExtractOptions{}); err != nil || len(keysOn(t, all)) != 3 {
		t.Fatalf("default extract: %v %v", keysOn(t, all), err)
	}

	bomb := zipOf(t, fixture{name: "__MACOSX/zeros.bin", body: zeros})
	_, err = archive.Extract(t.Context(), newDisk(t), "in", bytes.NewReader(bomb), int64(len(bomb)), archive.ExtractOptions{Skip: debris})
	wantRejected(t, err, archive.ErrLimit)
}
