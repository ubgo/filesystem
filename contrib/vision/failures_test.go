package vision_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/records/tags"
)

// TestDefaultErrorHandlerLogs pins that without WithErrorHandler a failed
// labelling is logged through slog's default logger, with the file it
// concerns, rather than dropped.
func TestDefaultErrorHandlerLogs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	e, _ := newEnv(t, &fakeLabeler{err: filesystem.Errorf(filesystem.CodeUnavailable, "labeler down")})
	f, err := e.store.Upload(t.Context(), optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "labelling failed") || !strings.Contains(out, f.ID) || !strings.Contains(out, "labeler down") {
		t.Fatalf("log = %q", out)
	}
}

// TestBeforePurgeLeavesTheTagsPlugin pins that the plugin itself does
// nothing on purge: the tags plugin owns the file's tags.
func TestBeforePurgeLeavesTheTagsPlugin(t *testing.T) {
	_, vp := newEnv(t, &fakeLabeler{})
	if err := vp.BeforePurge(t.Context(), nil, records.File{}); err != nil {
		t.Fatal(err)
	}
}

// TestLabel_StorageFailures pins that a file whose object is gone, or
// whose disk the store does not have, fails Label before the labeler is
// called.
func TestLabel_StorageFailures(t *testing.T) {
	ctx := t.Context()
	l := &fakeLabeler{}
	e, vp := newEnv(t, l, vision.WithErrorHandler(func(context.Context, records.File, error) {}))
	f, err := e.store.Upload(ctx, optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	calls := l.calls()
	if err := e.disk.Delete(ctx, f.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := vp.Label(ctx, e.store, optedIn, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing object: %v, want CodeNotFound", err)
	}
	// Two stores over one database: the file is recorded through one whose
	// manager has its disk, and labelled through one whose manager does not.
	db, _ := recordstest.Open(t)
	storeOn := func(name string) (*records.Store, *filesystem.Disk) {
		t.Helper()
		d, err := filesystem.NewDisk(name, memory.New())
		if err != nil {
			t.Fatal(err)
		}
		mgr, err := filesystem.NewManager(d)
		if err != nil {
			t.Fatal(err)
		}
		s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(tags.New()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s, d
	}
	withDisk, media := storeOn("media")
	if _, err := media.WriteBytes(ctx, "x.png", pngHead, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	g, err := withDisk.CreateFile(ctx, records.NewFile{Scope: optedIn, Disk: "media", Key: "x.png", Filename: "x.png", ContentType: "image/png", Size: int64(len(pngHead))})
	if err != nil {
		t.Fatal(err)
	}
	without, _ := storeOn("elsewhere")
	if _, err := vp.Label(ctx, without, optedIn, g.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a file on a disk the store does not have: %v, want CodeNotFound", err)
	}
	if l.calls() != calls {
		t.Fatalf("the labeler was called %d more times", l.calls()-calls)
	}
}

// TestPlugin_EqualScoresKeepTheirOrder pins that labels of equal score
// keep the labeler's order when MaxLabels cuts the list.
func TestPlugin_EqualScoresKeepTheirOrder(t *testing.T) {
	l := &fakeLabeler{labels: []vision.Label{{Name: "First", Score: 0.9}, {Name: "Second", Score: 0.9}, {Name: "Third", Score: 0.9}}}
	e, _ := newEnv(t, l, vision.WithMaxLabels(2))
	f, err := e.store.Upload(t.Context(), optedIn, "media", bytes.NewReader(pngHead), "dog.png", policy)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(e.tagsOf(t, optedIn, f.ID), ",")
	if !strings.Contains(strings.ToLower(got), "first") || !strings.Contains(strings.ToLower(got), "second") || strings.Contains(strings.ToLower(got), "third") {
		t.Fatalf("tags %s, want the first two in the labeler's order", got)
	}
}
