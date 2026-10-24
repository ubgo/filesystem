package tus_test

import (
	"context"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fstus "github.com/ubgo/filesystem/contrib/tus"
)

// abandon starts an upload, sends part of it and returns its URL and ID.
func (e *env) abandon(t *testing.T) (string, string) {
	t.Helper()
	loc, resp := e.create(t, len(pngData), map[string]string{fstus.MetaFilename: "a.png"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	h := map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}
	if r := e.do(t, http.MethodPatch, loc, strings.NewReader(pngData[:4]), h); r.StatusCode != http.StatusNoContent {
		t.Fatalf("partial PATCH = %d", r.StatusCode)
	}
	return loc, path.Base(loc)
}

// age sets the modification time of every file of a session.
func age(t *testing.T, dir, id string, at time.Time) {
	t.Helper()
	for _, name := range []string{id, id + ".info"} {
		if err := os.Chtimes(filepath.Join(dir, name), at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestSweepDeletesOnlyAbandonedSessions(t *testing.T) {
	e := setup(t, fstus.Config{})
	ctx := t.Context()
	now := time.Now()
	oldLoc, oldID := e.abandon(t)
	_, freshID := e.abandon(t)
	age(t, e.tmp, oldID, now.Add(-48*time.Hour))
	// A session whose .info is old but whose data received a PATCH
	// recently is still active.
	_, activeID := e.abandon(t)
	if err := os.Chtimes(filepath.Join(e.tmp, activeID+".info"), now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A data file whose .info was never written, and files tusd never made.
	orphan := strings.Repeat("ab", 16)
	stray := []string{"notes.txt", strings.Repeat("A", 32), "short.info"}
	for _, name := range append([]string{orphan}, stray...) {
		if err := os.WriteFile(filepath.Join(e.tmp, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(filepath.Join(e.tmp, name), now.Add(-48*time.Hour), now.Add(-48*time.Hour))
	}
	if err := os.Mkdir(filepath.Join(e.tmp, strings.Repeat("cd", 16)), 0o700); err != nil {
		t.Fatal(err)
	}

	n, err := fstus.Sweep(ctx, e.tmp, now.Add(-24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want the abandoned session and the orphan", n, err)
	}
	if exists(e.tmp, oldID) || exists(e.tmp, oldID+".info") || exists(e.tmp, orphan) {
		t.Error("an abandoned session survived")
	}
	for _, name := range append([]string{freshID, freshID + ".info", activeID, activeID + ".info", strings.Repeat("cd", 16)}, stray...) {
		if !exists(e.tmp, name) {
			t.Errorf("%s was swept", name)
		}
	}
	// The client of the swept session learns it is gone and must restart.
	if r := e.do(t, http.MethodHead, oldLoc, nil, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("HEAD of a swept session = %d, want 404", r.StatusCode)
	}

	// Run twice: nothing more to do.
	if n, err := fstus.Sweep(ctx, e.tmp, now.Add(-24*time.Hour)); err != nil || n != 0 {
		t.Errorf("second Sweep = %d, %v", n, err)
	}
}

func TestSweepEdges(t *testing.T) {
	ctx := t.Context()
	if n, err := fstus.Sweep(ctx, filepath.Join(t.TempDir(), "never-created"), time.Now()); err != nil || n != 0 {
		t.Errorf("missing dir = %d, %v", n, err)
	}
	file := filepath.Join(t.TempDir(), "a-file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := fstus.Sweep(ctx, file, time.Now()); err == nil {
		t.Error("a dir that is a file was accepted")
	}

	e := setup(t, fstus.Config{})
	_, id := e.abandon(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if n, err := fstus.Sweep(cancelled, e.tmp, time.Now().Add(time.Hour)); err == nil || n != 0 || !exists(e.tmp, id) {
		t.Errorf("cancelled Sweep = %d, %v", n, err)
	}

	if os.Geteuid() == 0 {
		return // root deletes from a read-only directory
	}
	if err := os.Chmod(e.tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(e.tmp, 0o700) }()
	if n, err := fstus.Sweep(ctx, e.tmp, time.Now().Add(time.Hour)); err == nil || n != 0 {
		t.Errorf("Sweep of an undeletable session = %d, %v", n, err)
	}
}
