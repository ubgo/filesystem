package records_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
)

const pngData2 = pngData + "-second-version"

var pngPolicy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}

// keeper is a Versioner and ReplaceHooks plugin that records what it saw.
type keeper struct {
	keep     bool
	kept     []records.File
	failKeep error
	failHook error
	hooked   [][2]records.File
}

func (k *keeper) Name() string { return "keeper" }

func (k *keeper) KeepVersion(_ context.Context, _ *records.Store, prev records.File) (bool, error) {
	if k.failKeep != nil {
		return false, k.failKeep
	}
	k.kept = append(k.kept, prev)
	return k.keep, nil
}

func (k *keeper) BeforeReplace(_ context.Context, _ *records.Store, cur records.File, next *records.File) error {
	if k.failHook != nil {
		return k.failHook
	}
	k.hooked = append(k.hooked, [2]records.File{cur, *next})
	next.Metadata = map[string]string{"replaced": "yes"}
	next.Key = "hijacked" // ignored: a hook cannot move the content
	return nil
}

func TestReplace(t *testing.T) {
	ctx := t.Context()
	k := &keeper{}
	e := newEnv(t, nil, records.WithPlugins(k))
	f, err := e.store.Upload(ctx, "t", "media", strings.NewReader(pngData), "photo.png", upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Visibility: filesystem.VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	post := records.Owner{Type: "post", ID: "p"}
	att, _ := e.store.Attach(ctx, "t", f.ID, post, "cover")

	public := pngPolicy
	public.Visibility = filesystem.VisibilityPublic // the file's own visibility wins
	g, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData2), "", public)
	if err != nil {
		t.Fatal(err)
	}
	if g.ID != f.ID || g.Key == f.Key || g.Filename != "photo.png" || g.Size != int64(len(pngData2)) || g.Visibility != filesystem.VisibilityPrivate || g.Metadata["replaced"] != "yes" {
		t.Fatalf("replaced = %+v", g)
	}
	if en, _ := e.disk.Stat(ctx, g.Key); en.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("new object visibility = %q", en.Visibility)
	}
	stored, _ := e.store.File(ctx, "t", f.ID)
	if stored.Key != g.Key || stored.SHA256 != g.SHA256 || stored.Metadata["replaced"] != "yes" {
		t.Fatalf("row = %+v", stored)
	}
	if ok, _ := e.disk.Exists(ctx, f.Key); ok {
		t.Fatal("the previous object was kept without a Versioner keeping it")
	}
	if got, _ := e.store.Attachments(ctx, "t", post, "cover"); len(got) != 1 || got[0].Attachment.ID != att.ID || got[0].File.Key != g.Key {
		t.Fatalf("attachments after replace = %+v", got)
	}
	if len(k.kept) != 1 || k.kept[0].Key != f.Key || len(k.hooked) != 1 || k.hooked[0][0].Key != f.Key || k.hooked[0][1].Key != g.Key {
		t.Fatalf("plugins saw kept %+v, hooked %+v", k.kept, k.hooked)
	}

	h, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData), "renamed.png", pngPolicy)
	if err != nil || h.Filename != "renamed.png" {
		t.Fatalf("replace with a name = %+v, %v", h, err)
	}
}

func TestReplaceKeepsVersions(t *testing.T) {
	ctx := t.Context()
	k := &keeper{keep: true}
	e := newEnv(t, nil, records.WithPlugins(k))
	f := e.put(t, "t", "a.png")
	if _, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData2), "", pngPolicy); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.disk.Exists(ctx, f.Key); !ok {
		t.Fatal("a kept version's object was deleted")
	}

	// ReplaceObject alone takes what it is not told from the disk.
	_, _ = e.disk.WriteBytes(ctx, "direct.png", []byte(pngData), filesystem.WriteOptions{})
	g, err := e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "direct.png"})
	if err != nil || g.Size != int64(len(pngData)) || g.ContentType != "image/png" || g.Filename != f.Filename {
		t.Fatalf("ReplaceObject = %+v, %v", g, err)
	}
}

func TestReplaceRefusals(t *testing.T) {
	ctx := t.Context()
	k := &keeper{}
	e := newEnv(t, nil, records.WithPlugins(k))
	f := e.put(t, "t", "a.png")
	trashed := e.put(t, "t", "b.png")
	_ = e.store.Trash(ctx, "t", trashed.ID)
	_, _ = e.disk.WriteBytes(ctx, "other.png", []byte(pngData2), filesystem.WriteOptions{})

	for name, c := range map[string]struct {
		err  error
		code filesystem.Code
	}{
		"trashed":         {only(e.store.ReplaceObject(ctx, "t", trashed.ID, records.NewContent{Key: "other.png"})), filesystem.CodeNotFound},
		"missing file":    {only(e.store.ReplaceObject(ctx, "t", "fil_missing", records.NewContent{Key: "other.png"})), filesystem.CodeNotFound},
		"other scope":     {only(e.store.ReplaceObject(ctx, "u", f.ID, records.NewContent{Key: "other.png"})), filesystem.CodeNotFound},
		"same object":     {only(e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: f.Key})), filesystem.CodeInvalidArgument},
		"missing object":  {only(e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "nothing.png"})), filesystem.CodeNotFound},
		"bad key":         {only(e.store.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "../x"})), filesystem.CodeInvalidKey},
		"refused upload":  {only(e.store.Replace(ctx, "t", f.ID, strings.NewReader("<!DOCTYPE html><p>page</p>"), "", pngPolicy)), filesystem.CodeRejected},
		"replace missing": {only(e.store.Replace(ctx, "t", "fil_missing", strings.NewReader(pngData), "", pngPolicy)), filesystem.CodeNotFound},
		"replace trashed": {only(e.store.Replace(ctx, "t", trashed.ID, strings.NewReader(pngData2), "", pngPolicy)), filesystem.CodeNotFound},
	} {
		if filesystem.CodeOf(c.err) != c.code {
			t.Errorf("%s: %v, want %s", name, c.err, c.code)
		}
	}
	// Only the seeded objects remain: refused replacements stored nothing.
	if n := countObjects(t, e.disk); n != 3 {
		t.Fatalf("%d objects", n)
	}

	boom := errors.New("plugin refused")
	for _, fail := range []func(){func() { k.failHook = boom }, func() { k.failHook, k.failKeep = nil, boom }} {
		fail()
		if _, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData2), "", pngPolicy); !errors.Is(err, boom) {
			t.Fatalf("plugin failure = %v", err)
		}
		if got, _ := e.store.File(ctx, "t", f.ID); got.Key != f.Key {
			t.Fatal("a refused replacement changed the row")
		}
		if n := countObjects(t, e.disk); n != 3 {
			t.Fatalf("%d objects after a refused replacement", n)
		}
	}
}

func only[T any](_ T, err error) error { return err }

// TestReplaceRollback proves the new object goes, and the old stays, when
// an enclosing transaction rolls back.
func TestReplaceRollback(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	boom := errors.New("later step failed")
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData2), "", pngPolicy); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got, _ := e.store.File(ctx, "t", f.ID); got.Key != f.Key {
		t.Fatal("a rolled-back replacement changed the row")
	}
	if ok, _ := e.disk.Exists(ctx, f.Key); !ok || countObjects(t, e.disk) != 1 {
		t.Fatalf("objects after rollback: old kept %v, total %d", ok, countObjects(t, e.disk))
	}
}

func TestReplaceObjectFailures(t *testing.T) {
	ctx := t.Context()
	var handled []string
	e := newEnv(t, undeletable{memory.New()}, records.WithObjectErrorHandler(func(_ context.Context, f records.File, err error) {
		handled = append(handled, f.Key+":"+string(filesystem.CodeOf(err)))
	}))
	f := e.put(t, "t", "a.png")
	if _, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(pngData2), "", pngPolicy); err != nil {
		t.Fatal(err)
	}
	if strings.Join(handled, ",") != "a.png:permission" {
		t.Fatalf("handled %v", handled)
	}

	// A store whose manager lacks the file's disk.
	empty, _ := filesystem.NewManager()
	other, err := records.New(e.db, testDialect(), empty)
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Replace(ctx, "t", f.ID, strings.NewReader(pngData), "", pngPolicy)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = other.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "x.png"})
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestReplaceObjectTakesFileVisibility: the new object gets the file's
// visibility, so a private file never ends up over a public object; a
// rolled-back replacement gives the object its own visibility back, and a
// refused visibility change refuses the replacement.
func TestReplaceObjectTakesFileVisibility(t *testing.T) {
	ctx := t.Context()
	var refuse bool
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if refuse && op.Kind == filesystem.OpSetVisibility {
			return filesystem.Errorf(filesystem.CodePermission, "refused")
		}
		return call(ctx)
	}))
	mgr, _ := filesystem.NewManager(disk)
	s, _ := records.New(openDB(t), testDialect(), mgr)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	write := func(key string, v filesystem.Visibility) {
		if _, err := disk.WriteBytes(ctx, key, []byte(pngData), filesystem.WriteOptions{Visibility: v}); err != nil {
			t.Fatal(err)
		}
	}
	write("a.png", filesystem.VisibilityPrivate)
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.png"})
	if err != nil || f.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("CreateFile = %+v, %v", f, err)
	}

	write("rolled.png", filesystem.VisibilityPublic)
	boom := errors.New("later step failed")
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "rolled.png"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || objectVisibility(t, disk, "rolled.png") != filesystem.VisibilityPublic {
		t.Fatalf("rolled back: %v, object %s", err, objectVisibility(t, disk, "rolled.png"))
	}

	refuse = true
	_, err = s.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "rolled.png"})
	wantCode(t, err, filesystem.CodePermission)
	refuse = false

	write("public.png", filesystem.VisibilityPublic)
	g, err := s.ReplaceObject(ctx, "t", f.ID, records.NewContent{Key: "public.png"})
	if err != nil || g.Visibility != filesystem.VisibilityPrivate || objectVisibility(t, disk, "public.png") != filesystem.VisibilityPrivate {
		t.Fatalf("ReplaceObject = %+v, %v, object %s", g, err, objectVisibility(t, disk, "public.png"))
	}
}
