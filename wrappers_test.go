package filesystem_test

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

func TestScopedConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		base := mustDisk(t, "base", memory.New())
		// Noise outside the scope must never appear inside it.
		_, _ = base.WriteBytes(t.Context(), "other/noise.txt", []byte("x"), filesystem.WriteOptions{})
		_, _ = base.WriteBytes(t.Context(), "tenantx/noise.txt", []byte("x"), filesystem.WriteOptions{})
		scoped, err := base.Scope("tenant", "tenant/a")
		if err != nil {
			t.Fatal(err)
		}
		return scoped
	})
}

func TestScopedOnLocalConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		drv, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		base := mustDisk(t, "base", drv)
		t.Cleanup(func() { _ = base.Close() })
		scoped, err := base.Scope("scoped", "s")
		if err != nil {
			t.Fatal(err)
		}
		return scoped
	})
}

func TestScope(t *testing.T) {
	ctx := t.Context()
	inner := memory.New()
	base := mustDisk(t, "base", fullDriver{inner}, filesystem.WithPublicURL("https://cdn.test"))
	s, err := base.Scope("t1", "/tenants/1/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteBytes(ctx, "logo.png", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := base.Exists(ctx, "tenants/1/logo.png"); !ok {
		t.Error("scoped write did not land under the prefix")
	}
	if u, _ := s.PublicURL(ctx, "logo.png"); u != "https://cdn.test/tenants/1/logo.png" {
		t.Errorf("scoped public URL = %q", u)
	}
	noBase := mustDisk(t, "nb", fullDriver{memory.New()})
	s2, _ := noBase.Scope("s2", "p")
	if u, _ := s2.PublicURL(ctx, "k"); u != "https://bucket.test/p/k" {
		t.Errorf("driver public URL through scope = %q", u)
	}
	if r, _ := s2.SignedURL(ctx, "k", getOpts); r.URL != "https://backend.test/p/k?sig=1" {
		t.Errorf("presign through scope = %q", r.URL)
	}
	if r, _ := s2.PresignPart(ctx, "k", "1", 1, time.Minute); !strings.Contains(r.URL, "/p/k") {
		t.Errorf("presign part through scope = %q", r.URL)
	}
	if _, err := base.Scope("bad", "../x"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("traversal prefix: %v", err)
	}
	if _, err := base.Scope("bad", "/"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("empty prefix: %v", err)
	}
	if _, err := base.Scope("bad name", "p"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("bad name: %v", err)
	}
}

func TestMirrorConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		primary := mustDisk(t, "primary", memory.New())
		drv, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		replica := mustDisk(t, "replica", drv)
		t.Cleanup(func() { _ = replica.Close() })
		m, err := filesystem.Mirror("mirror", primary, replica)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Every case's end state must be identical on the replica,
			// unless the case writes keys differing only in case and the
			// replica folds case where the primary does not (local on
			// macOS): a documented divergence of such mirrors.
			if t.Name() == "TestMirrorConformance/LetterCase" && replica.Capabilities().CaseInsensitive != primary.Capabilities().CaseInsensitive {
				return
			}
			assertSameTree(t, primary, replica)
		})
		return m
	})
}

// assertSameTree compares every key and its bytes on two disks, ignoring
// the multipart staging that only the primary sees.
func assertSameTree(t *testing.T, a, b *filesystem.Disk) {
	t.Helper()
	ctx := context.Background()
	list := func(d *filesystem.Disk) map[string]string {
		out := map[string]string{}
		for e, err := range d.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
			if err != nil {
				t.Fatalf("list %s: %v", d.Name(), err)
			}
			data, err := d.ReadAll(ctx, e.Key)
			if err != nil {
				t.Fatalf("read %s:%s: %v", d.Name(), e.Key, err)
			}
			out[e.Key] = string(data)
		}
		return out
	}
	pa, pb := list(a), list(b)
	if len(pa) != len(pb) {
		t.Errorf("primary has %d keys, replica %d", len(pa), len(pb))
	}
	for k, v := range pa {
		if pb[k] != v {
			t.Errorf("replica %q differs from primary", k)
		}
	}
}

func TestMirror(t *testing.T) {
	ctx := t.Context()
	primary := mustDisk(t, "p", fullDriver{memory.New()})
	if _, err := filesystem.Mirror("m", primary); err == nil {
		t.Error("mirror without mirrors accepted")
	}
	if _, err := filesystem.Mirror("m", primary, primary); err == nil {
		t.Error("primary as its own mirror accepted")
	}
	if _, err := filesystem.Mirror("m", nil, primary); err == nil {
		t.Error("nil primary accepted")
	}
	failing := mustDisk(t, "broken", undeletable{newBasic()})
	m, err := filesystem.Mirror("m", primary, failing)
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities().PresignPart {
		t.Error("mirror kept PresignPart")
	}
	if _, err := m.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate, Metadata: map[string]string{"a": "b"}}); err != nil {
		t.Fatalf("write fans out to a mirror without metadata: %v", err)
	}
	err = m.Delete(ctx, "k")
	var fe *filesystem.Error
	if !errors.As(err, &fe) || fe.Code != filesystem.CodeUnavailable || !strings.Contains(err.Error(), "mirror broken") {
		t.Errorf("mirror failure = %v", err)
	}
	if ok, _ := primary.Exists(ctx, "k"); ok {
		t.Error("primary delete did not happen before the mirror failed")
	}
	_, err = m.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeUnsupported)
	if r, err := m.SignedURL(ctx, "k", getOpts); err != nil || r.URL == "" {
		t.Errorf("signed GET through mirror: %v", err)
	}
	if u, err := m.PublicURL(ctx, "k"); err != nil || u != "https://bucket.test/k" {
		t.Errorf("public URL through mirror = %q, %v", u, err)
	}
	if v, ok := filesystem.As[*memory.Driver](m); !ok || v == nil {
		t.Error("As did not unwrap the mirror")
	}
	// Primary failures stop before the mirrors.
	bad := mustDisk(t, "bad", &erroringWrites{basicDriver: newBasic()})
	m2, _ := filesystem.Mirror("m2", bad, mustDisk(t, "r", memory.New()))
	_, err = m2.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodePermission)
	// Visibility changes skip mirrors that cannot store it.
	m3, _ := filesystem.Mirror("m3", mustDisk(t, "p3", memory.New()), mustDisk(t, "r3", newBasic()))
	_, _ = m3.WriteBytes(ctx, "v", []byte("x"), filesystem.WriteOptions{})
	if err := m3.SetVisibility(ctx, "v", filesystem.VisibilityPublic); err != nil {
		t.Errorf("set visibility with a basic mirror: %v", err)
	}
	if _, err := filesystem.Mirror("bad name!", primary, m3); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("invalid mirror name = %v", err)
	}
}

// TestMirrorSignsUploadsThroughItsURLSigner pins the documented split: a
// primary that signs its own URLs has signed PUTs refused (TestMirror),
// while a primary without signing gets them from the disk's URLSigner,
// naming the mirror disk, so the application serves the upload through the
// mirror.
func TestMirrorSignsUploadsThroughItsURLSigner(t *testing.T) {
	ctx := t.Context()
	primary := mustDisk(t, "p", memory.New(), filesystem.WithURLSigner(stubSigner{}))
	if primary.Capabilities().Presign {
		t.Fatal("the primary must sign through its URLSigner for this test")
	}
	m, err := filesystem.Mirror("m", primary, mustDisk(t, "r", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil || r.URL != "https://app.test/files/m/k" {
		t.Errorf("signed PUT through a mirror with a URLSigner = %+v, %v", r, err)
	}
}

// erroringWrites fails every write.
type erroringWrites struct{ *basicDriver }

func (*erroringWrites) Write(context.Context, string, io.Reader, filesystem.WriteOptions) (filesystem.Entry, error) {
	return filesystem.Entry{}, filesystem.ErrPermission
}

func TestMirrorPrimaryFailuresStopEarly(t *testing.T) {
	ctx := t.Context()
	replica := mustDisk(t, "r", memory.New())
	m, _ := filesystem.Mirror("m", mustDisk(t, "p", memory.New()), replica)
	_, err := m.Copy(ctx, "missing", "b")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = m.Move(ctx, "missing", "b")
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, m.SetVisibility(ctx, "missing", filesystem.VisibilityPublic), filesystem.CodeNotFound)
	_, err = m.CompleteMultipart(ctx, "k", "nope", []filesystem.Part{{Number: 1}})
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, mustMirror(t, &undeletablePrefix{Driver: memory.New()}).DeletePrefix(ctx, "d"), filesystem.CodePermission)
	wantCode(t, mustMirror(t, undeletable{newBasic()}).Delete(ctx, "d"), filesystem.CodePermission)
	// A replica read failure surfaces.
	m4, _ := filesystem.Mirror("m4", mustDisk(t, "p4", unreadable{newBasic()}), mustDisk(t, "r4", memory.New()))
	_, err = m4.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeUnavailable)
}

type undeletablePrefix struct{ *memory.Driver }

func (*undeletablePrefix) DeletePrefix(context.Context, string) error {
	return filesystem.ErrPermission
}

func mustMirror(t *testing.T, primary filesystem.Driver) *filesystem.Disk {
	t.Helper()
	m, err := filesystem.Mirror("m", mustDisk(t, "p", primary), mustDisk(t, "r", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReadOnly(t *testing.T) {
	ctx := t.Context()
	inner := memory.New()
	_, _ = inner.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{})
	d := mustDisk(t, "ro", inner, filesystem.WithInterceptors(filesystem.ReadOnly()), filesystem.WithURLSigner(stubSigner{}))
	if _, err := d.ReadAll(ctx, "k"); err != nil {
		t.Errorf("read on read-only disk: %v", err)
	}
	if _, err := d.SignedURL(ctx, "k", getOpts); err != nil {
		t.Errorf("signed GET on read-only disk: %v", err)
	}
	for _, err := range []error{
		func() error { _, err := d.WriteBytes(ctx, "k", nil, filesystem.WriteOptions{}); return err }(),
		d.Delete(ctx, "k"),
		func() error {
			_, err := d.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
			return err
		}(),
		d.Check(ctx),
	} {
		if !errors.Is(err, filesystem.ErrReadOnly) {
			t.Errorf("mutation on read-only disk: %v", err)
		}
	}
}

func TestManager(t *testing.T) {
	ctx := t.Context()
	a, b := mustDisk(t, "a", memory.New()), mustDisk(t, "b", memory.New())
	m, err := filesystem.NewManager(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := m.Default(); d != a {
		t.Error("first disk is not the default")
	}
	if err := m.SetDefault("b"); err != nil {
		t.Fatal(err)
	}
	if d, _ := m.Default(); d != b {
		t.Error("SetDefault ignored")
	}
	wantCode(t, m.SetDefault("zzz"), filesystem.CodeNotFound)
	_, err = m.Disk("zzz")
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, m.Add(mustDisk(t, "a", memory.New())), filesystem.CodeExists)
	wantCode(t, m.Add(nil), filesystem.CodeInvalidArgument)
	if _, err := filesystem.NewManager(a, a); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Error("NewManager accepted duplicate names")
	}
	if got := strings.Join(m.Names(), ","); got != "a,b" {
		t.Errorf("Names = %s", got)
	}
	if err := m.Check(ctx); err != nil {
		t.Errorf("Check: %v", err)
	}
	empty, _ := filesystem.NewManager()
	_, err = empty.Default()
	wantCode(t, err, filesystem.CodeNotFound)

	closer := &closingDriver{basicDriver: newBasic(), err: errors.New("close failed")}
	m2, _ := filesystem.NewManager(mustDisk(t, "c", closer), mustDisk(t, "ro", memory.New(), filesystem.WithInterceptors(filesystem.ReadOnly())))
	if err := m2.Close(); err == nil || !closer.closed {
		t.Errorf("Close = %v", err)
	}
	if err := m2.Check(ctx); !errors.Is(err, filesystem.ErrReadOnly) {
		t.Errorf("Check of a read-only disk = %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close of plain disks = %v", err)
	}
}

// corrupting returns different bytes than were written, to cover Check's
// comparison.
type corrupting struct{ *basicDriver }

func (corrupting) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("not what you wrote")), nil
}

func TestCheckFailures(t *testing.T) {
	ctx := t.Context()
	wantCode(t, mustDisk(t, "d", corrupting{newBasic()}).Check(ctx), filesystem.CodeUnknown)
	wantCode(t, mustDisk(t, "d", unreadable{newBasic()}).Check(ctx), filesystem.CodePermission)
	wantCode(t, mustDisk(t, "d", undeletable{newBasic()}).Check(ctx), filesystem.CodePermission)
}

func TestCopyAndMoveAcross(t *testing.T) {
	ctx := t.Context()
	src := mustDisk(t, "src", memory.New())
	dst := mustDisk(t, "dst", newBasic())
	_, _ = src.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{ContentType: "text/x-test", Metadata: map[string]string{"m": "1"}})
	if _, err := filesystem.CopyAcross(ctx, src, "a.txt", dst, "copy.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := dst.ReadAll(ctx, "copy.txt"); string(got) != "hello" {
		t.Errorf("copied %q", got)
	}
	full := mustDisk(t, "full", memory.New())
	if _, err := filesystem.CopyAcross(ctx, src, "a.txt", full, "c.txt"); err != nil {
		t.Fatal(err)
	}
	if e, _ := full.Stat(ctx, "c.txt"); e.Metadata["m"] != "1" || e.ContentType != "text/x-test" {
		t.Errorf("metadata or type lost: %+v", e)
	}
	if _, err := filesystem.CopyAcross(ctx, src, "a.txt", src, "same-disk.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.MoveAcross(ctx, src, "a.txt", dst, "moved.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := src.Exists(ctx, "a.txt"); ok {
		t.Error("MoveAcross left the source")
	}
	if _, err := filesystem.MoveAcross(ctx, src, "same-disk.txt", src, "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	_, err := filesystem.CopyAcross(ctx, src, "missing", dst, "x")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = filesystem.MoveAcross(ctx, src, "missing", dst, "x")
	wantCode(t, err, filesystem.CodeNotFound)
	ur := mustDisk(t, "ur", unreadable{newBasic()})
	_, _ = ur.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
	_, err = filesystem.CopyAcross(ctx, ur, "k", dst, "x")
	wantCode(t, err, filesystem.CodePermission)
	ud := mustDisk(t, "ud", undeletable{newBasic()})
	_, _ = ud.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
	_, err = filesystem.MoveAcross(ctx, ud, "k", dst, "x")
	wantCode(t, err, filesystem.CodePermission)
}

func TestMux(t *testing.T) {
	ctx := t.Context()
	mux := filesystem.NewMux()
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		t.Fatal(err)
	}
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		t.Fatal(err)
	}
	wantCode(t, mux.Register(memory.Scheme, memory.Opener), filesystem.CodeExists)
	wantCode(t, mux.Register("", memory.Opener), filesystem.CodeInvalidArgument)
	wantCode(t, mux.Register("x", nil), filesystem.CodeInvalidArgument)

	d, err := mux.Open(ctx, "cache", "mem://cache?prefix=t1&public_url=https://cdn.test&visibility=public")
	if err != nil {
		t.Fatal(err)
	}
	e, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	if err != nil || e.Visibility != filesystem.VisibilityPublic {
		t.Fatalf("write = %+v, %v", e, err)
	}
	if u, _ := d.PublicURL(ctx, "a.txt"); u != "https://cdn.test/t1/a.txt" {
		t.Errorf("public URL = %q", u)
	}
	ro, err := mux.Open(ctx, "ro", "mem://?readonly=true")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.WriteBytes(ctx, "k", nil, filesystem.WriteOptions{}); !errors.Is(err, filesystem.ErrReadOnly) {
		t.Errorf("readonly DSN writable: %v", err)
	}
	if _, err := mux.Open(ctx, "rw", "mem://?readonly=false"); err != nil {
		t.Error(err)
	}
	dir := t.TempDir()
	ld, err := mux.Open(ctx, "files", "file://"+dir, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ld.Close() }()
	if e, err := ld.WriteBytes(ctx, "x", []byte("1"), filesystem.WriteOptions{}); err != nil || e.Visibility != filesystem.VisibilityPublic {
		t.Errorf("local via mux: %+v %v", e, err)
	}
	bad := []string{
		"%zz",
		"nope://x",
		"mem://?typo=1",
		"mem://?readonly=maybe",
		"mem://?prefix=../x",
		"mem://?visibility=secret",
		"mem://?public_url=ftp://x",
		"file://host/path",
		"file:",
		"file:///dev/null/not-a-dir",
	}
	for _, dsn := range bad {
		if _, err := mux.Open(ctx, "d", dsn); err == nil {
			t.Errorf("Open(%q) succeeded", dsn)
		} else if strings.Contains(err.Error(), "secret") && strings.Contains(dsn, "secret") && !strings.Contains(dsn, "visibility") {
			t.Errorf("error leaks the DSN: %v", err)
		}
	}
	rel, err := mux.Open(ctx, "rel", "file:"+dir+"/rel")
	if err != nil {
		t.Fatal(err)
	}
	_ = rel.Close()
	u, _ := url.Parse("mem://?a=1")
	if err := filesystem.RejectParams(u, "a"); err != nil {
		t.Errorf("allowed param rejected: %v", err)
	}
}

func TestPageOf(t *testing.T) {
	objs := []filesystem.Entry{{Key: "d/c"}, {Key: "d/a"}, {Key: "d/sub/x"}, {Key: "d/sub/y"}, {Key: "d/b"}}
	p := filesystem.PageOf(objs, "d", filesystem.ListOptions{})
	var got []string
	for _, e := range p.Entries {
		got = append(got, e.Key)
	}
	if strings.Join(got, ",") != "d/a,d/b,d/c,d/sub" || !p.Entries[3].IsDir || p.NextCursor != "" {
		t.Errorf("folded page = %v %+v", got, p)
	}
	p = filesystem.PageOf(objs, "d", filesystem.ListOptions{Recursive: true, Limit: 2, Cursor: "d/a"})
	if len(p.Entries) != 2 || p.Entries[0].Key != "d/b" || p.NextCursor != "d/c" {
		t.Errorf("paged = %+v", p)
	}
	if p := filesystem.PageOf(nil, "", filesystem.ListOptions{}); len(p.Entries) != 0 || p.NextCursor != "" {
		t.Errorf("empty = %+v", p)
	}
}
