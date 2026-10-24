package video_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/sqlb"
)

// edgeEnv is a Store on a database whose steps can be made to fail, with
// the plugin and, after it, extra plugins, over the disks "media", "cdn"
// and the read-only "ro".
type edgeEnv struct {
	store      *records.Store
	plugin     *video.Plugin
	media, cdn *filesystem.Disk
	faults     *recordstest.Faults
}

func newEdgeEnv(t *testing.T, more ...records.Plugin) *edgeEnv {
	t.Helper()
	media, _ := filesystem.NewDisk("media", memory.New())
	cdn, _ := filesystem.NewDisk("cdn", memory.New())
	ro, _ := filesystem.NewDisk("ro", memory.New(), filesystem.WithInterceptors(filesystem.ReadOnly()))
	mgr, err := filesystem.NewManager(media, cdn, ro)
	if err != nil {
		t.Fatal(err)
	}
	db, faults := recordstest.OpenFaulty(t)
	e := &edgeEnv{plugin: video.NewPlugin(video.Tool{}), media: media, cdn: cdn, faults: faults}
	if e.store, err = records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(append([]records.Plugin{e.plugin}, more...)...)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

// file records a text file (a type the plugin leaves alone) and gives it
// meta, naming a derived object as the plugin writes it, straight in the
// database (the plugin drops such keys from a caller's metadata); derived
// bytes are stored on media under that key unless nil.
func (e *edgeEnv) file(t *testing.T, key string, meta map[string]string, derived []byte) records.File {
	t.Helper()
	ctx := t.Context()
	if _, err := e.media.WriteBytes(ctx, key, []byte("text"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if derived != nil {
		if _, err := e.media.WriteBytes(ctx, meta[video.MetaPosterKey], derived, filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: key, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	e.setRawMetadata(t, f, string(raw))
	f.Metadata = meta
	return f
}

// TestCallerCannotNameObjects: the keys naming the plugin's objects are
// dropped from a caller's metadata on a file of any type, since the purge
// deletes what they name, while the caller's other keys stay.
func TestCallerCannotNameObjects(t *testing.T) {
	e := newEdgeEnv(t)
	if _, err := e.media.WriteBytes(t.Context(), "a.txt", []byte("text"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "a.txt", ContentType: "text/plain",
		Metadata: map[string]string{video.MetaPosterKey: "victim.bin", video.MetaPosterDisk: "cdn", "app": "kept"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Metadata[video.MetaPosterKey]; ok || f.Metadata[video.MetaPosterDisk] != "" || f.Metadata["app"] != "kept" {
		t.Fatalf("metadata = %v; want only the caller's own key", f.Metadata)
	}
	if _, err := e.media.WriteBytes(t.Context(), "b.txt", []byte("text"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	only, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "b.txt", ContentType: "text/plain",
		Metadata: map[string]string{video.MetaPosterKey: "victim.bin"}})
	if err != nil || only.Metadata != nil {
		t.Fatalf("metadata = %v, %v; want none", only.Metadata, err)
	}
}

// setRawMetadata stores raw as f's metadata column, bypassing the Store.
func (e *edgeEnv) setRawMetadata(t *testing.T, f records.File, raw string) {
	t.Helper()
	upd := sqlb.NewUpdate(e.store.Dialect()).Table(records.TableFiles).Set(sqlb.Set(records.FileMetadata, raw)).
		Where(sqlb.Eq(records.FileScope, f.Scope), sqlb.Eq(records.FileID, f.ID))
	if _, err := e.store.Exec(t.Context(), upd); err != nil {
		t.Fatal(err)
	}
}

// TestOwnedEdges_Metadata: a row whose metadata names a derived object
// but is not a map of strings, or matches the LIKE only through a
// case-insensitive comparison, claims nothing in OwnedObjects; the first
// is left alone by RenameDisk, which still renames the well-formed rows.
func TestOwnedEdges_Metadata(t *testing.T) {
	ctx := t.Context()
	e := newEdgeEnv(t)
	odd := e.file(t, "odd.txt", map[string]string{video.MetaPosterKey: "odd.bin"}, nil)
	e.setRawMetadata(t, odd, `{"`+video.MetaPosterKey+`":"odd.bin","`+video.MetaPosterDisk+`":"cdn","n":1}`)
	upper := e.file(t, "upper.txt", map[string]string{video.MetaPosterKey: "upper.bin"}, nil)
	e.setRawMetadata(t, upper, `{"`+strings.ToUpper(video.MetaPosterKey)+`":"upper.bin"}`)
	good := e.file(t, "good.txt", map[string]string{video.MetaPosterKey: "good.bin", video.MetaPosterDisk: "cdn"}, nil)
	owned, err := e.plugin.OwnedObjects(ctx, e.store, "cdn", []string{"odd.bin", "good.bin"})
	if err != nil || owned["odd.bin"] || !owned["good.bin"] {
		t.Fatalf("owned = %v, %v; want only good.bin", owned, err)
	}
	if owned, err := e.plugin.OwnedObjects(ctx, e.store, "media", []string{"upper.bin"}); err != nil || len(owned) != 0 {
		t.Fatalf("owned = %v, %v; want nothing", owned, err)
	}
	if err := e.plugin.RenameDisk(ctx, e.store, "cdn", "cdn2"); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.File(ctx, scope, good.ID)
	if err != nil || got.Metadata[video.MetaPosterDisk] != "cdn2" {
		t.Fatalf("well-formed row after the rename: %v, %v", got.Metadata, err)
	}
}

// TestOwnedEdges_DatabaseFailures: a failing query or update is returned
// by OwnedObjects and RenameDisk, never taken for "nothing owned".
func TestOwnedEdges_DatabaseFailures(t *testing.T) {
	ctx := t.Context()
	e := newEdgeEnv(t)
	e.file(t, "a.txt", map[string]string{video.MetaPosterKey: "a.bin", video.MetaPosterDisk: "cdn"}, nil)
	e.faults.Arm(1)
	if _, err := e.plugin.OwnedObjects(ctx, e.store, "cdn", []string{"a.bin"}); !errors.Is(err, recordstest.ErrInjected) {
		t.Fatalf("OwnedObjects on a failing query = %v", err)
	}
	e.faults.Arm(0)
	if err := e.plugin.RenameDisk(ctx, e.store, "nothing", "other"); err != nil {
		t.Fatal(err)
	}
	query := e.faults.Steps()
	for n := 1; n <= query+1; n++ { // the query's steps, then the update
		e.faults.Arm(n)
		if err := e.plugin.RenameDisk(ctx, e.store, "cdn", "cdn2"); !errors.Is(err, recordstest.ErrInjected) {
			t.Fatalf("RenameDisk failing at step %d = %v", n, err)
		}
	}
	e.faults.Arm(0)
}

// failingMover fails every MoveFile after the plugin moved its objects.
type failingMover struct{}

func (failingMover) Name() string { return "failing_mover" }
func (failingMover) MoveFileDisk(context.Context, *records.Store, records.File, string) error {
	return errors.New("refused")
}

// TestOwnedEdges_Move: MoveFileDisk refuses an unknown disk on either
// side, a derived object missing from the disk and a copy the
// destination refuses; and a move rolled back after the copy deletes the
// copy again.
func TestOwnedEdges_Move(t *testing.T) {
	ctx := t.Context()
	t.Run("disks", func(t *testing.T) {
		e := newEdgeEnv(t)
		f := e.file(t, "a.txt", map[string]string{video.MetaPosterKey: "a.bin"}, []byte("derived"))
		g := f
		g.Disk = "nowhere"
		if err := e.plugin.MoveFileDisk(ctx, e.store, g, "cdn"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Fatalf("unknown source disk = %v", err)
		}
		if err := e.plugin.MoveFileDisk(ctx, e.store, f, "nowhere"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Fatalf("unknown target disk = %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		e := newEdgeEnv(t)
		f := e.file(t, "a.txt", map[string]string{video.MetaPosterKey: "a.bin"}, nil)
		if err := e.plugin.MoveFileDisk(ctx, e.store, f, "cdn"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
			t.Fatalf("missing derived object = %v", err)
		}
	})
	t.Run("refused copy", func(t *testing.T) {
		e := newEdgeEnv(t)
		f := e.file(t, "a.txt", map[string]string{video.MetaPosterKey: "a.bin"}, []byte("derived"))
		if err := e.plugin.MoveFileDisk(ctx, e.store, f, "ro"); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
			t.Fatalf("copy onto a read-only disk = %v", err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		e := newEdgeEnv(t, failingMover{})
		f := e.file(t, "a.txt", map[string]string{video.MetaPosterKey: "a.bin"}, []byte("derived"))
		if _, err := e.store.MoveFile(ctx, scope, f.ID, "cdn"); err == nil {
			t.Fatal("the move succeeded past a failing plugin")
		}
		if ok, _ := e.cdn.Exists(ctx, "a.bin"); ok {
			t.Fatal("the copy outlived the rolled-back move")
		}
		if ok, _ := e.media.Exists(ctx, "a.bin"); !ok {
			t.Fatal("the rolled-back move deleted the original")
		}
	})
}
