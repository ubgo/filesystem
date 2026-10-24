package filesystem_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// archive is a memory driver that also keeps holds, lists versions,
// restores, and answers conditional reads itself, the way an object store
// with object lock and versioning does, recording what it was asked.
type archive struct {
	*memory.Driver
	holds    map[string]filesystem.Hold
	restored map[string]int
	condRead int
}

func newArchive() *archive {
	return &archive{Driver: memory.New(), holds: map[string]filesystem.Hold{}, restored: map[string]int{}}
}

func (a *archive) Capabilities() filesystem.Capabilities {
	c := a.Driver.Capabilities()
	c.Hold, c.Versions, c.Restore, c.ConditionalRead = true, true, true, true
	return c
}

func (a *archive) Hold(_ context.Context, key string) (filesystem.Hold, error) {
	return a.holds[key], nil
}

func (a *archive) SetHold(_ context.Context, key string, h filesystem.Hold) error {
	a.holds[key] = h
	return nil
}

func (a *archive) Versions(ctx context.Context, key string) ([]filesystem.Version, error) {
	e, err := a.Driver.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return []filesystem.Version{{ID: "v2", Size: e.Size, ETag: e.ETag, Latest: true}, {ID: "v1", DeleteMarker: true}}, nil
}

func (a *archive) Restore(_ context.Context, key string, o filesystem.RestoreOptions) error {
	a.restored[key] = o.Days
	return nil
}

func (a *archive) ReadIf(ctx context.Context, key string, o filesystem.ReadOptions) (io.ReadCloser, filesystem.Entry, error) {
	a.condRead++
	e, err := a.Driver.Stat(ctx, key)
	if err != nil {
		return nil, filesystem.Entry{}, err
	}
	if filesystem.NotModified(e, o) {
		return nil, filesystem.Entry{}, filesystem.ErrNotModified
	}
	rc, err := a.Driver.Read(ctx, key, filesystem.ByteRange{})
	return rc, e, err
}

func TestObjectLockVersionsAndRestore(t *testing.T) {
	ctx := t.Context()
	a := newArchive()
	d := mustDisk(t, "archive", a)
	_, _ = d.WriteBytes(ctx, "contracts/a.pdf", []byte("pdf"), filesystem.WriteOptions{})
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := d.SetHold(ctx, "/contracts//a.pdf", filesystem.Hold{Legal: true, RetainUntil: until}); err != nil {
		t.Fatal(err)
	}
	if h, err := d.Hold(ctx, "contracts/a.pdf"); err != nil || !h.Legal || !h.RetainUntil.Equal(until) {
		t.Errorf("hold %+v, %v", h, err)
	}
	vs, err := d.Versions(ctx, "contracts/a.pdf")
	if err != nil || len(vs) != 2 || !vs[0].Latest || vs[0].Size != 3 || !vs[1].DeleteMarker {
		t.Errorf("versions %+v, %v", vs, err)
	}
	if err := d.Restore(ctx, "contracts/a.pdf", filesystem.RestoreOptions{Days: 7}); err != nil || a.restored["contracts/a.pdf"] != 7 {
		t.Errorf("restore %v, %v", a.restored, err)
	}
	_, err = d.Versions(ctx, "missing.pdf")
	wantCode(t, err, filesystem.CodeNotFound)
	for name, err := range map[string]error{
		"restore without days": d.Restore(ctx, "contracts/a.pdf", filesystem.RestoreOptions{}),
		"bad key":              d.SetHold(ctx, "../x", filesystem.Hold{}),
	} {
		if code := filesystem.CodeOf(err); code != filesystem.CodeInvalidArgument && code != filesystem.CodeInvalidKey {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A disk without the capabilities refuses each.
	plain := mustDisk(t, "plain", newBasic())
	_, err1 := plain.Hold(ctx, "a")
	_, err2 := plain.Versions(ctx, "a")
	_, err3 := plain.Tags(ctx, "a")
	for i, err := range []error{err1, err2, err3, plain.SetHold(ctx, "a", filesystem.Hold{}), plain.Restore(ctx, "a", filesystem.RestoreOptions{Days: 1}), plain.SetTags(ctx, "a", nil), plain.SetModTime(ctx, "a", time.Now())} {
		if !errors.Is(err, filesystem.ErrUnsupported) {
			t.Errorf("call %d without the capability: %v", i, err)
		}
	}
	_, err = plain.Append(ctx, "a", strings.NewReader("x"), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestTagsValidation(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "m", memory.New())
	_, _ = d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{})
	for _, tags := range []map[string]string{{"": "v"}, {"k\n": "v"}, {"k": "v\x00"}} {
		if err := d.SetTags(ctx, "a", tags); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("tags %q: %v", tags, err)
		}
	}
	// The caller's map is copied: changing it later changes nothing stored.
	tags := map[string]string{"team": "a"}
	_ = d.SetTags(ctx, "a", tags)
	tags["team"] = "b"
	if got, _ := d.Tags(ctx, "a"); got["team"] != "a" {
		t.Errorf("stored tags changed with the caller's map: %v", got)
	}
}

func TestReadIfNative(t *testing.T) {
	ctx := t.Context()
	a := newArchive()
	d := mustDisk(t, "archive", a)
	e, _ := d.WriteBytes(ctx, "a.txt", []byte("body"), filesystem.WriteOptions{})
	if _, _, err := d.ReadIf(ctx, "a.txt", filesystem.ReadOptions{IfNoneMatch: `W/"` + e.ETag + `"`}); filesystem.CodeOf(err) != filesystem.CodeNotModified || a.condRead != 1 {
		t.Errorf("native not modified: %v, %d calls", err, a.condRead)
	}
	rc, got, err := d.ReadIf(ctx, "/a.txt", filesystem.ReadOptions{IfNoneMatch: "other"})
	if err != nil || got.Key != "a.txt" {
		t.Fatalf("native read: %+v, %v", got, err)
	}
	_ = rc.Close()
	_, _, err = d.ReadIf(ctx, "../x", filesystem.ReadOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
	// The Stat fallback reports a read failure.
	broken := mustDisk(t, "broken", unreadable{newBasic()})
	_, _ = broken.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	_, _, err = broken.ReadIf(ctx, "a.txt", filesystem.ReadOptions{})
	wantCode(t, err, filesystem.CodePermission)
}

func TestNotModified(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 500, time.UTC)
	e := filesystem.Entry{ETag: "abc", ModTime: at}
	for _, c := range []struct {
		o    filesystem.ReadOptions
		want bool
	}{
		{filesystem.ReadOptions{}, false},
		{filesystem.ReadOptions{IfNoneMatch: "abc"}, true},
		{filesystem.ReadOptions{IfNoneMatch: `"abc"`}, true},
		{filesystem.ReadOptions{IfNoneMatch: `W/"abc"`}, true},
		{filesystem.ReadOptions{IfNoneMatch: "*"}, true},
		{filesystem.ReadOptions{IfNoneMatch: "xyz", IfModifiedSince: at.Add(time.Hour)}, false}, // the ETag decides
		{filesystem.ReadOptions{IfModifiedSince: at.Truncate(time.Second)}, true},
		{filesystem.ReadOptions{IfModifiedSince: at.Add(-time.Second)}, false},
	} {
		if got := filesystem.NotModified(e, c.o); got != c.want {
			t.Errorf("NotModified(%+v) = %v", c.o, got)
		}
	}
	if filesystem.NotModified(filesystem.Entry{}, filesystem.ReadOptions{IfNoneMatch: `""`}) {
		t.Error("an object without an ETag matched")
	}
}

func TestCompareAndSwapOptions(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "m", memory.New())
	e, _ := d.WriteBytes(ctx, "a", []byte("v1"), filesystem.WriteOptions{})
	// An ETag quoted as in an HTTP header still matches.
	if _, err := d.WriteBytes(ctx, "a", []byte("v2"), filesystem.WriteOptions{IfMatch: `"` + e.ETag + `"`}); err != nil {
		t.Errorf("quoted ETag: %v", err)
	}
	_, err := d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{IfMatch: "e", IfNotExists: true})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.CreateMultipart(ctx, "big", filesystem.WriteOptions{IfMatch: "e"})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = d.Append(ctx, "a", nil, filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.Append(ctx, "../a", strings.NewReader("x"), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
	err = d.SetModTime(ctx, "a", time.Time{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// A scoped disk forwards every new operation under its prefix; a mirror
// declares none of them.
func TestWrappersAndTheNewOperations(t *testing.T) {
	ctx := t.Context()
	a := newArchive()
	base := mustDisk(t, "base", a)
	tenant, err := base.Scope("acme", "t/acme")
	if err != nil {
		t.Fatal(err)
	}
	if e, err := tenant.Append(ctx, "log.txt", strings.NewReader("x"), filesystem.AppendOptions{}); err != nil || e.Key != "log.txt" {
		t.Fatalf("scoped append %+v, %v", e, err)
	}
	if ok, _ := base.Exists(ctx, "t/acme/log.txt"); !ok {
		t.Error("scoped append missed the prefix")
	}
	if err := tenant.SetModTime(ctx, "log.txt", time.Unix(1e9, 0)); err != nil {
		t.Error(err)
	}
	if err := tenant.SetTags(ctx, "log.txt", map[string]string{"k": "v"}); err != nil {
		t.Error(err)
	}
	if tags, err := tenant.Tags(ctx, "log.txt"); err != nil || tags["k"] != "v" {
		t.Errorf("scoped tags %v, %v", tags, err)
	}
	if err := tenant.SetHold(ctx, "log.txt", filesystem.Hold{Legal: true}); err != nil || !a.holds["t/acme/log.txt"].Legal {
		t.Errorf("scoped hold %v, %v", a.holds, err)
	}
	if h, err := tenant.Hold(ctx, "log.txt"); err != nil || !h.Legal {
		t.Errorf("scoped hold read %+v, %v", h, err)
	}
	if vs, err := tenant.Versions(ctx, "log.txt"); err != nil || len(vs) != 2 {
		t.Errorf("scoped versions %v, %v", vs, err)
	}
	if err := tenant.Restore(ctx, "log.txt", filesystem.RestoreOptions{Days: 1}); err != nil || a.restored["t/acme/log.txt"] != 1 {
		t.Errorf("scoped restore %v, %v", a.restored, err)
	}
	if rc, e, err := tenant.ReadIf(ctx, "log.txt", filesystem.ReadOptions{}); err != nil || e.Key != "log.txt" {
		t.Errorf("scoped read %+v, %v", e, err)
	} else {
		_ = rc.Close()
	}
	m, err := filesystem.Mirror("m", base, mustDisk(t, "copy", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Capabilities()
	if c.Append || c.SetModTime || c.ConditionalRead || c.CompareAndSwap || c.Tags || c.Hold || c.Versions || c.Restore {
		t.Errorf("mirror caps %v", c)
	}
	if !strings.Contains(base.Capabilities().String(), "hold") {
		t.Errorf("caps string %v", base.Capabilities())
	}
}

func TestAppendOptions(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "m", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	for name, o := range map[string]filesystem.AppendOptions{"negative size": {MaxSize: -1}, "unknown visibility": {Visibility: "secret"}} {
		if _, err := d.Append(ctx, "a", strings.NewReader("x"), o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	e, err := d.Append(ctx, "pub.log", strings.NewReader("x"), filesystem.AppendOptions{})
	if err != nil || e.Visibility != filesystem.VisibilityPublic {
		t.Errorf("default visibility %+v, %v", e, err)
	}
	e, err = d.Append(ctx, "priv.log", strings.NewReader("x"), filesystem.AppendOptions{Visibility: filesystem.VisibilityPrivate})
	if err != nil || e.Visibility != filesystem.VisibilityPrivate {
		t.Errorf("chosen visibility %+v, %v", e, err)
	}
}

// wrappingAppender reports a body read failure as its own unclassified
// error, as an SDK that wraps every error would.
type wrappingAppender struct{ *memory.Driver }

func (w wrappingAppender) Append(ctx context.Context, key string, body io.Reader, o filesystem.AppendOptions) (filesystem.Entry, error) {
	if _, err := io.ReadAll(body); err != nil {
		return filesystem.Entry{}, filesystem.NewError(filesystem.CodeUnknown, errors.New("upload aborted"))
	}
	return w.Driver.Append(ctx, key, strings.NewReader(""), o)
}

// The size cap is reported as too_large whatever the driver calls the
// failed read.
func TestAppendCapIsTooLarge(t *testing.T) {
	d := mustDisk(t, "w", wrappingAppender{memory.New()})
	_, err := d.Append(t.Context(), "log", strings.NewReader(strings.Repeat("x", 20)), filesystem.AppendOptions{MaxSize: 10})
	wantCode(t, err, filesystem.CodeTooLarge)
}
