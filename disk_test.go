package filesystem_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestNewDiskValidation(t *testing.T) {
	mem := memory.New()
	bad := []struct {
		name string
		drv  filesystem.Driver
		opts []filesystem.Option
	}{
		{"", mem, nil},
		{"has/slash", mem, nil},
		{"-leading", mem, nil},
		{strings.Repeat("a", 65), mem, nil},
		{"ok", nil, nil},
		{"ok", mem, []filesystem.Option{filesystem.WithInterceptors(nil)}},
		{"ok", mem, []filesystem.Option{filesystem.WithPublicURL("not a url")}},
		{"ok", mem, []filesystem.Option{filesystem.WithPublicURL("ftp://cdn.test")}},
		{"ok", mem, []filesystem.Option{filesystem.WithPublicURL("https://")}},
		{"ok", mem, []filesystem.Option{filesystem.WithDefaultVisibility("secret")}},
		{"ok", mem, []filesystem.Option{filesystem.WithURLSigner(nil)}},
	}
	for _, c := range bad {
		if _, err := filesystem.NewDisk(c.name, c.drv, c.opts...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("NewDisk(%q, %T, %d opts) err = %v, want invalid_argument", c.name, c.drv, len(c.opts), err)
		}
	}
	d := mustDisk(t, "Uploads_1.v2", mem)
	if d.Name() != "Uploads_1.v2" || d.Capabilities() != mem.Capabilities() || d.DefaultVisibility() != filesystem.VisibilityPrivate {
		t.Errorf("Name/Capabilities = %q %v", d.Name(), d.Capabilities())
	}
}

// liar declares Copy without implementing Copier.
type liar struct{ *basicDriver }

func (liar) Capabilities() filesystem.Capabilities { return filesystem.Capabilities{Copy: true} }

// halfMultipart declares PresignPart without Multipart.
type halfMultipart struct{ fullDriver }

func (h halfMultipart) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{PresignPart: true}
}

func TestNewDiskRefusesMisdeclaredCapabilities(t *testing.T) {
	_, err := filesystem.NewDisk("d", liar{newBasic()})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "Copy") {
		t.Errorf("liar accepted: %v", err)
	}
	_, err = filesystem.NewDisk("d", halfMultipart{fullDriver{memory.New()}})
	if !strings.Contains(fmtErr(err), "PresignPart without Multipart") {
		t.Errorf("PresignPart without Multipart accepted: %v", err)
	}
	// Implementing an interface while declaring it false is allowed.
	type quiet struct{ *memory.Driver }
	if _, err := filesystem.NewDisk("d", quiet{memory.New()}); err != nil {
		t.Errorf("under-declaring driver refused: %v", err)
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestCapabilitiesString(t *testing.T) {
	if got := (filesystem.Capabilities{}).String(); got != "[]" {
		t.Errorf("empty = %q", got)
	}
	c := filesystem.Capabilities{Copy: true, Multipart: true, PresignPart: true, ConditionalWrite: true}
	if got := c.String(); got != "[copy,multipart,presign-part,conditional-write]" {
		t.Errorf("String = %q", got)
	}
}

// closingDriver records Close.
type closingDriver struct {
	*basicDriver
	closed bool
	err    error
}

func (c *closingDriver) Close() error { c.closed = true; return c.err }

func TestClose(t *testing.T) {
	if err := mustDisk(t, "d", newBasic()).Close(); err != nil {
		t.Errorf("Close on a driver without Close = %v", err)
	}
	cd := &closingDriver{basicDriver: newBasic(), err: errors.New("disk busy")}
	err := mustDisk(t, "d", cd).Close()
	if !cd.closed || err == nil || !strings.Contains(err.Error(), "disk busy") {
		t.Errorf("Close = %v, closed %v", err, cd.closed)
	}
}

func TestAsReachesNativeClientThroughWrappers(t *testing.T) {
	inner := memory.New()
	d := mustDisk(t, "d", fullDriver{inner})
	got, ok := filesystem.As[*memory.Driver](d)
	if !ok || got != inner {
		t.Fatal("As did not reach the native client")
	}
	if _, ok := filesystem.As[*strings.Builder](d); ok {
		t.Fatal("As returned a type the driver does not offer")
	}
	scoped, err := d.Scope("scoped", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := filesystem.As[*memory.Driver](scoped); !ok || got != inner {
		t.Fatal("As did not unwrap the scoped driver")
	}
}

func TestInvalidKeysAreAnnotated(t *testing.T) {
	d := mustDisk(t, "d", memory.New(), filesystem.WithURLSigner(stubSigner{}))
	ctx := t.Context()
	calls := map[filesystem.OpKind]func() error{
		filesystem.OpStat:          func() error { _, err := d.Stat(ctx, "../x"); return err },
		filesystem.OpRead:          func() error { _, err := d.Read(ctx, "../x"); return err },
		filesystem.OpWrite:         func() error { _, err := d.WriteBytes(ctx, "../x", nil, filesystem.WriteOptions{}); return err },
		filesystem.OpDelete:        func() error { return d.Delete(ctx, "../x") },
		filesystem.OpSetVisibility: func() error { return d.SetVisibility(ctx, "../x", filesystem.VisibilityPublic) },
		filesystem.OpList:          func() error { _, err := d.ListPage(ctx, "../x", filesystem.ListOptions{}); return err },
		filesystem.OpDeletePrefix:  func() error { return d.DeletePrefix(ctx, "../x") },
		filesystem.OpPublicURL:     func() error { _, err := d.PublicURL(ctx, "../x"); return err },
		filesystem.OpPresign:       func() error { _, err := d.SignedURL(ctx, "../x", getOpts); return err },
		filesystem.OpMultipartCreate: func() error {
			_, err := d.CreateMultipart(ctx, "../x", filesystem.WriteOptions{})
			return err
		},
		filesystem.OpMultipartPresign: func() error { _, err := d.PresignPart(ctx, "../x", "id", 1, time.Minute); return err },
	}
	for kind, call := range calls {
		var fe *filesystem.Error
		if err := call(); !errors.As(err, &fe) || fe.Code != filesystem.CodeInvalidKey || fe.Op != kind || fe.Disk != "d" || fe.Key != "../x" {
			t.Errorf("%s with a traversal key: %#v", kind, fe)
		}
	}
	for _, call := range []func() error{
		func() error { _, err := d.Copy(ctx, "../a", "b"); return err },
		func() error { _, err := d.Copy(ctx, "a", "../b"); return err },
		func() error { _, err := d.Move(ctx, "a", "../b"); return err },
	} {
		wantCode(t, call(), filesystem.CodeInvalidKey)
	}
}

var getOpts = filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}

func TestReadValidation(t *testing.T) {
	d := mustDisk(t, "d", memory.New())
	_, err := d.ReadRange(t.Context(), "k", filesystem.ByteRange{Offset: -1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.ReadRange(t.Context(), "k", filesystem.ByteRange{Length: -1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.ReadAll(t.Context(), "missing")
	wantCode(t, err, filesystem.CodeNotFound)
}

// brokenStream fails mid-read, to cover ReadAll's error path.
type brokenStream struct{ *basicDriver }

func (brokenStream) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(&errAfter{}), nil
}

type errAfter struct{}

func (*errAfter) Read([]byte) (int, error) { return 0, filesystem.ErrUnavailable }

func TestReadAllStreamError(t *testing.T) {
	_, err := mustDisk(t, "d", brokenStream{newBasic()}).ReadAll(t.Context(), "k")
	wantCode(t, err, filesystem.CodeUnavailable)
}

func TestExistsPropagatesOtherErrors(t *testing.T) {
	d := mustDisk(t, "d", &erroringDriver{basicDriver: newBasic(), err: filesystem.ErrPermission})
	ok, err := d.Exists(t.Context(), "k")
	if ok {
		t.Fatal("Exists true on error")
	}
	wantCode(t, err, filesystem.CodePermission)
}

func TestWriteValidation(t *testing.T) {
	ctx := t.Context()
	basic := mustDisk(t, "d", newBasic())
	_, err := basic.Write(ctx, "k", nil, filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	cases := []struct {
		opts filesystem.WriteOptions
		code filesystem.Code
	}{
		{filesystem.WriteOptions{MaxSize: -1}, filesystem.CodeInvalidArgument},
		{filesystem.WriteOptions{SizeHint: -1}, filesystem.CodeInvalidArgument},
		{filesystem.WriteOptions{SizeHint: 11, MaxSize: 10}, filesystem.CodeTooLarge},
		{filesystem.WriteOptions{Visibility: "hidden"}, filesystem.CodeInvalidArgument},
		{filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}, filesystem.CodeUnsupported},
		{filesystem.WriteOptions{Metadata: map[string]string{"a": "b"}}, filesystem.CodeUnsupported},
		{filesystem.WriteOptions{IfNotExists: true}, filesystem.CodeUnsupported},
	}
	for _, c := range cases {
		_, err := basic.Write(ctx, "k", strings.NewReader("x"), c.opts)
		if filesystem.CodeOf(err) != c.code {
			t.Errorf("Write with %+v: %v, want %s", c.opts, err, c.code)
		}
	}
	// The disk default visibility is fine even without the capability.
	if _, err := basic.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate}); err != nil {
		t.Errorf("default visibility refused: %v", err)
	}
}

func TestWriteDefaultsAndDetection(t *testing.T) {
	ctx := t.Context()
	pub := mustDisk(t, "d", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	e, err := pub.WriteBytes(ctx, "page.html", []byte("<!DOCTYPE html><p>hi"), filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if e.Visibility != filesystem.VisibilityPublic || e.ContentType != "text/html; charset=utf-8" {
		t.Errorf("entry = %+v, want public text/html", e)
	}
	raw := mustDisk(t, "d", memory.New(), filesystem.WithoutContentTypeDetection())
	if e, err = raw.WriteBytes(ctx, "x.png", []byte("\x89PNG\r\n\x1a\n"), filesystem.WriteOptions{}); err != nil || e.ContentType != "" {
		t.Errorf("detection not disabled: %+v %v", e, err)
	}
	// Detection reads the head of a stream without losing it.
	body := io.MultiReader(strings.NewReader(strings.Repeat("z", 600)), strings.NewReader("END"))
	if _, err := raw.Write(ctx, "stream", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := raw.ReadAll(ctx, "stream")
	if len(got) != 603 || !bytes.HasSuffix(got, []byte("END")) {
		t.Errorf("stream round trip lost bytes: %d", len(got))
	}
}

// seekFails is a seekable body whose Seek fails, to cover those paths.
type seekFails struct {
	io.Reader
	failAt int
	calls  int
}

func (s *seekFails) Seek(int64, int) (int64, error) {
	s.calls++
	if s.calls >= s.failAt {
		return 0, errors.New("seek failed")
	}
	return 0, nil
}

// headFails fails while the Disk reads the head for detection.
type headFails struct{}

func (headFails) Read([]byte) (int, error) { return 0, filesystem.ErrPermission }

func TestWriteBodyErrors(t *testing.T) {
	d := mustDisk(t, "d", memory.New())
	ctx := t.Context()
	// A rewind that fails after the head was read is reported.
	_, err := d.Write(ctx, "k", &seekFails{Reader: strings.NewReader("x"), failAt: 2}, filesystem.WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "seek failed") {
		t.Errorf("failed rewind not reported: %v", err)
	}
	_, err = d.Write(ctx, "k", headFails{}, filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodePermission)
	if ok, _ := d.Exists(ctx, "k"); ok {
		t.Error("a failed write stored an object")
	}
}

// retryTwice is an interceptor that runs every call twice when the first
// attempt fails with CodeUnavailable and the op is replayable.
func retryTwice(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
	err := call(ctx)
	if filesystem.CodeOf(err) == filesystem.CodeUnavailable && op.Replayable {
		return call(ctx)
	}
	return err
}

// flakyWriter fails the first Write after consuming the body.
type flakyWriter struct {
	*basicDriver
	failed bool
}

func (f *flakyWriter) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	if !f.failed {
		f.failed = true
		_, _ = io.Copy(io.Discard, body)
		return filesystem.Entry{}, filesystem.ErrUnavailable
	}
	return f.basicDriver.Write(ctx, key, body, o)
}

func TestWriteRetryRewindsSeekableBodies(t *testing.T) {
	d := mustDisk(t, "d", &flakyWriter{basicDriver: newBasic()}, filesystem.WithInterceptors(retryTwice))
	e, err := d.WriteBytes(t.Context(), "k", []byte("payload"), filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.ReadAll(t.Context(), "k")
	if string(got) != "payload" || e.Size != 7 {
		t.Errorf("after retry: %q size %d", got, e.Size)
	}
}

// replayAnyway re-runs every call once more, ignoring Replayable, to prove
// the Disk itself refuses to replay a consumed stream.
func replayAnyway(ctx context.Context, _ *filesystem.Op, call filesystem.Call) error {
	if err := call(ctx); err != nil {
		return call(ctx)
	}
	return nil
}

func TestWriteStreamIsNotReplayable(t *testing.T) {
	var seen *filesystem.Op
	capture := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		seen = op
		return call(ctx)
	}
	d := mustDisk(t, "d", &flakyWriter{basicDriver: newBasic()}, filesystem.WithInterceptors(capture, replayAnyway))
	_, err := d.Write(t.Context(), "k", io.MultiReader(strings.NewReader("stream")), filesystem.WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot be replayed") {
		t.Fatalf("replaying a stream: %v", err)
	}
	if seen.Replayable || seen.Attempt != 2 {
		t.Errorf("op = %+v, want non-replayable with 2 attempts", seen)
	}
}

// swallower ignores read errors from the body and stores what it got: a
// driver bug the Disk must contain.
type swallower struct{ *basicDriver }

func (s swallower) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	data, _ := io.ReadAll(body)
	return s.basicDriver.Write(ctx, key, bytes.NewReader(data), o)
}

func TestTooLargeIsNeverStoredEvenByABuggyDriver(t *testing.T) {
	d := mustDisk(t, "d", swallower{newBasic()})
	_, err := d.Write(t.Context(), "k", strings.NewReader("0123456789"), filesystem.WriteOptions{MaxSize: 4})
	wantCode(t, err, filesystem.CodeTooLarge)
	if ok, _ := d.Exists(t.Context(), "k"); ok {
		t.Error("truncated object left behind")
	}
}

func TestOpFieldsSeenByInterceptors(t *testing.T) {
	var ops []filesystem.Op
	record := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		ops = append(ops, *op)
		return err
	}
	d := mustDisk(t, "rec", fullDriver{memory.New()}, filesystem.WithInterceptors(record))
	ctx := t.Context()
	_, _ = d.WriteBytes(ctx, "a", []byte("abc"), filesystem.WriteOptions{})
	rc, _ := d.Read(ctx, "a")
	_ = rc.Close()
	_, _ = d.Copy(ctx, "a", "b")
	_, _ = d.SignedURL(ctx, "a", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	_, _ = d.SignedURL(ctx, "a", getOpts)
	want := []struct {
		kind      filesystem.OpKind
		mutating  bool
		streaming bool
		target    string
		bytes     int64
	}{
		{filesystem.OpWrite, true, false, "", 3},
		{filesystem.OpRead, false, true, "", 0},
		{filesystem.OpCopy, true, false, "b", 0},
		{filesystem.OpPresign, true, false, "", 0},
		{filesystem.OpPresign, false, false, "", 0},
	}
	if len(ops) != len(want) {
		t.Fatalf("recorded %d ops: %+v", len(ops), ops)
	}
	for i, w := range want {
		o := ops[i]
		if o.Kind != w.kind || o.Mutating != w.mutating || o.Streaming != w.streaming || o.Target != w.target || o.Bytes != w.bytes || o.Disk != "rec" || o.Attempt != 1 {
			t.Errorf("op %d = %+v, want %+v", i, o, w)
		}
	}
	if !filesystem.OpWrite.Mutates() || filesystem.OpRead.Mutates() || filesystem.OpPresign.Mutates() {
		t.Error("Mutates wrong")
	}
	if len(filesystem.OpKindValues) != 34 {
		t.Errorf("OpKindValues has %d kinds", len(filesystem.OpKindValues))
	}
}

func TestInterceptorOrder(t *testing.T) {
	var trace []string
	mk := func(name string) filesystem.Interceptor {
		return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
			trace = append(trace, name+">")
			err := call(ctx)
			trace = append(trace, "<"+name)
			return err
		}
	}
	d := mustDisk(t, "d", memory.New(), filesystem.WithInterceptors(mk("outer")), filesystem.WithInterceptors(mk("inner")))
	_, _ = d.Stat(t.Context(), "x")
	if got := strings.Join(trace, " "); got != "outer> inner> <inner <outer" {
		t.Errorf("order = %s", got)
	}
}

func TestFallbacksOnABasicDriver(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", newBasic())
	if _, err := d.WriteBytes(ctx, "dir/a.txt", []byte("A"), filesystem.WriteOptions{ContentType: "text/x-a"}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Copy(ctx, "dir/a.txt", "dir/b.txt")
	if err != nil || e.Key != "dir/b.txt" {
		t.Fatalf("fallback copy = %+v, %v", e, err)
	}
	if _, err := d.Move(ctx, "dir/b.txt", "moved/c.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Exists(ctx, "dir/b.txt"); ok {
		t.Error("fallback move left the source")
	}
	if err := d.DeletePrefix(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Exists(ctx, "dir/a.txt"); ok {
		t.Error("fallback delete prefix left a key")
	}
	_, err = d.Copy(ctx, "x", "x")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.Move(ctx, "missing", "y")
	wantCode(t, err, filesystem.CodeNotFound)
}

// unreadable fails Read but not Stat, to cover the streamed copy's read error.
type unreadable struct{ *basicDriver }

func (unreadable) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return nil, filesystem.ErrPermission
}

// undeletable fails Delete, to cover Move's and DeletePrefix's error paths.
type undeletable struct{ *basicDriver }

func (undeletable) Delete(context.Context, string) error { return filesystem.ErrPermission }

// unlistable fails List.
type unlistable struct{ *basicDriver }

func (unlistable) List(context.Context, string, filesystem.ListOptions) (filesystem.Page, error) {
	return filesystem.Page{}, filesystem.ErrUnavailable
}

func TestFallbackErrors(t *testing.T) {
	ctx := t.Context()
	seed := func(b *basicDriver) *basicDriver {
		_, _ = b.inner.Write(ctx, "dir/k", strings.NewReader("x"), filesystem.WriteOptions{})
		return b
	}
	_, err := mustDisk(t, "d", unreadable{seed(newBasic())}).Copy(ctx, "dir/k", "other")
	wantCode(t, err, filesystem.CodePermission)
	_, err = mustDisk(t, "d", undeletable{seed(newBasic())}).Move(ctx, "dir/k", "other")
	wantCode(t, err, filesystem.CodePermission)
	wantCode(t, mustDisk(t, "d", undeletable{seed(newBasic())}).DeletePrefix(ctx, "dir"), filesystem.CodePermission)
	wantCode(t, mustDisk(t, "d", unlistable{seed(newBasic())}).DeletePrefix(ctx, "dir"), filesystem.CodeUnavailable)
}

func TestNativeCapabilitiesAreUsed(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", memory.New())
	if _, err := d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetVisibility(ctx, "a", "weird"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("invalid visibility: %v", err)
	}
	wantCode(t, mustDisk(t, "d", newBasic()).SetVisibility(ctx, "a", filesystem.VisibilityPublic), filesystem.CodeUnsupported)
	_, err := d.Copy(ctx, "missing", "b")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.Move(ctx, "missing", "b")
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestListing(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", memory.New())
	for _, k := range []string{"a/1", "a/2", "a/3"} {
		_, _ = d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{})
	}
	_, err := d.ListPage(ctx, "a", filesystem.ListOptions{Limit: -1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	n := 0
	for range d.List(ctx, "a", filesystem.ListOptions{Limit: 1}) {
		n++
		break
	}
	if n != 1 {
		t.Error("breaking out of List kept iterating")
	}
	for _, err := range mustDisk(t, "d", unlistable{newBasic()}).List(ctx, "", filesystem.ListOptions{}) {
		wantCode(t, err, filesystem.CodeUnavailable)
	}
}

// stubSigner signs app URLs deterministically.
type stubSigner struct{}

func (stubSigner) SignURL(_ context.Context, disk, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: o.Method, URL: "https://app.test/files/" + disk + "/" + key, ExpiresAt: fixedNow.Add(o.Expires)}, nil
}

func TestURLs(t *testing.T) {
	ctx := t.Context()
	cdn := mustDisk(t, "d", fullDriver{memory.New()}, filesystem.WithPublicURL("https://cdn.test/assets/"))
	if u, err := cdn.PublicURL(ctx, "/img/a b.png"); err != nil || u != "https://cdn.test/assets/img/a%20b.png" {
		t.Errorf("CDN URL = %q, %v", u, err)
	}
	backend := mustDisk(t, "d", fullDriver{memory.New()})
	if u, err := backend.PublicURL(ctx, "k"); err != nil || u != "https://bucket.test/k" {
		t.Errorf("driver URL = %q, %v", u, err)
	}
	_, err := mustDisk(t, "d", memory.New()).PublicURL(ctx, "k")
	wantCode(t, err, filesystem.CodeUnsupported)

	if r, err := backend.SignedURL(ctx, "k", getOpts); err != nil || r.URL != "https://backend.test/k?sig=1" {
		t.Errorf("driver presign = %+v, %v", r, err)
	}
	signed := mustDisk(t, "local", memory.New(), filesystem.WithURLSigner(stubSigner{}))
	if r, err := signed.SignedURL(ctx, "k", getOpts); err != nil || r.URL != "https://app.test/files/local/k" {
		t.Errorf("signer presign = %+v, %v", r, err)
	}
	_, err = mustDisk(t, "d", memory.New()).SignedURL(ctx, "k", getOpts)
	wantCode(t, err, filesystem.CodeUnsupported)
	for _, o := range []filesystem.PresignOptions{
		{Method: "DELETE", Expires: time.Minute},
		{Method: filesystem.MethodGet},
		{Method: filesystem.MethodGet, Expires: filesystem.MaxPresignExpiry + time.Second},
	} {
		_, err := signed.SignedURL(ctx, "k", o)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	failing := mustDisk(t, "d", memory.New(), filesystem.WithURLSigner(failingSigner{}))
	_, err = failing.SignedURL(ctx, "k", getOpts)
	wantCode(t, err, filesystem.CodePermission)
	_, err = mustDisk(t, "d", failingURLer{fullDriver{memory.New()}}).PublicURL(ctx, "k")
	wantCode(t, err, filesystem.CodePermission)
}

type failingSigner struct{}

func (failingSigner) SignURL(context.Context, string, string, filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{}, filesystem.ErrPermission
}

type failingURLer struct{ fullDriver }

func (failingURLer) PublicURL(context.Context, string) (string, error) {
	return "", filesystem.ErrPermission
}

func TestMultipartThroughDisk(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", fullDriver{memory.New()})
	id, err := d.CreateMultipart(ctx, "big", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := d.PresignPart(ctx, "big", id, 1, time.Minute); err != nil || r.Method != filesystem.MethodPut {
		t.Errorf("PresignPart = %+v, %v", r, err)
	}
	p1, err := d.UploadPart(ctx, "big", id, 1, strings.NewReader("hello "), 6)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := d.UploadPart(ctx, "big", id, 2, &onceReader{r: strings.NewReader("world")}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := d.CompleteMultipart(ctx, "big", id, []filesystem.Part{p1, p2}); err != nil || e.Key != "big" {
		t.Fatalf("complete = %+v, %v", e, err)
	}
	got, _ := d.ReadAll(ctx, "big")
	if string(got) != "hello world" {
		t.Errorf("assembled %q", got)
	}

	// Validation.
	checks := []func() error{
		func() error { _, err := d.UploadPart(ctx, "big", "", 1, strings.NewReader("x"), 1); return err },
		func() error { _, err := d.UploadPart(ctx, "big", id, 0, strings.NewReader("x"), 1); return err },
		func() error {
			_, err := d.UploadPart(ctx, "big", id, filesystem.MaxPartNumber+1, strings.NewReader("x"), 1)
			return err
		},
		func() error { _, err := d.PresignPart(ctx, "big", "", 1, time.Minute); return err },
		func() error { _, err := d.PresignPart(ctx, "big", id, 0, time.Minute); return err },
		func() error { _, err := d.PresignPart(ctx, "big", id, 1, 0); return err },
		func() error { _, err := d.ListParts(ctx, "big", ""); return err },
		func() error { _, err := d.CompleteMultipart(ctx, "big", "", []filesystem.Part{p1}); return err },
		func() error { _, err := d.CompleteMultipart(ctx, "big", id, nil); return err },
		func() error { return d.AbortMultipart(ctx, "big", "") },
	}
	for i, c := range checks {
		if filesystem.CodeOf(c()) != filesystem.CodeInvalidArgument {
			t.Errorf("check %d accepted invalid input", i)
		}
	}
	none := mustDisk(t, "d", newBasic())
	unsupported := []func() error{
		func() error { _, err := none.CreateMultipart(ctx, "k", filesystem.WriteOptions{}); return err },
		func() error { _, err := none.UploadPart(ctx, "k", "id", 1, strings.NewReader("x"), 1); return err },
		func() error { _, err := none.PresignPart(ctx, "k", "id", 1, time.Minute); return err },
		func() error { _, err := none.ListParts(ctx, "k", "id"); return err },
		func() error { _, err := none.CompleteMultipart(ctx, "k", "id", []filesystem.Part{p1}); return err },
		func() error { return none.AbortMultipart(ctx, "k", "id") },
	}
	for i, c := range unsupported {
		if filesystem.CodeOf(c()) != filesystem.CodeUnsupported {
			t.Errorf("unsupported call %d did not say so", i)
		}
	}
	_, err = d.CreateMultipart(ctx, "k", filesystem.WriteOptions{Visibility: "nope"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.UploadPart(ctx, "k", "999", 1, strings.NewReader("x"), 1)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.ListParts(ctx, "k", "999")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.CompleteMultipart(ctx, "k", "999", []filesystem.Part{p1})
	wantCode(t, err, filesystem.CodeNotFound)
	// A body whose Seek fails is uploaded once, as a stream.
	open, _ := d.CreateMultipart(ctx, "k", filesystem.WriteOptions{})
	if p, err := d.UploadPart(ctx, "k", open, 1, &seekFails{Reader: strings.NewReader("x"), failAt: 1}, 1); err != nil || p.Size != 1 {
		t.Errorf("UploadPart of an unseekable seeker: %+v, %v", p, err)
	}
}

// TestUnseekableSeekersAreStreams pins that a body implementing io.Seeker
// whose Seek fails (an *os.File on a pipe, as with "printf x | fs put")
// is written once as a stream instead of failing, and is never retried.
func TestUnseekableSeekersAreStreams(t *testing.T) {
	ctx := t.Context()
	f := &flakyWriter{basicDriver: newBasic()}
	var attempts []int
	count := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		err := call(ctx)
		attempts = append(attempts, op.Attempt)
		return err
	}
	d := mustDisk(t, "d", f, filesystem.WithInterceptors(count, retryTwice))
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.WriteString("<html>piped</html>")
		_ = w.Close()
	}()
	defer func() { _ = r.Close() }()
	// The first attempt fails transiently; a pipe cannot be replayed.
	_, err = d.Write(ctx, "a.html", r, filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeUnavailable)
	if len(attempts) != 1 || attempts[0] != 1 {
		t.Fatalf("pipe write ran %v attempts, want exactly one", attempts)
	}

	// Without a transient failure the pipe is stored, its type sniffed from
	// the head the Disk read before streaming the rest.
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w2.WriteString("<html>piped</html>")
		_ = w2.Close()
	}()
	defer func() { _ = r2.Close() }()
	e, err := d.Write(ctx, "b", r2, filesystem.WriteOptions{})
	if err != nil || e.Size != 18 || !strings.HasPrefix(e.ContentType, "text/html") {
		t.Fatalf("pipe write = %+v, %v", e, err)
	}
	if got, _ := d.ReadAll(ctx, "b"); string(got) != "<html>piped</html>" {
		t.Errorf("stored %q", got)
	}
}

// onceReader is a non-seekable body.
type onceReader struct{ r io.Reader }

func (o *onceReader) Read(p []byte) (int, error) { return o.r.Read(p) }

// flakyParts fails the first UploadPart after consuming the body.
type flakyParts struct {
	*memory.Driver
	failed bool
}

func (f *flakyParts) UploadPart(ctx context.Context, key, id string, n int, body io.Reader, size int64) (filesystem.Part, error) {
	if !f.failed {
		f.failed = true
		_, _ = io.Copy(io.Discard, body)
		return filesystem.Part{}, filesystem.ErrUnavailable
	}
	return f.Driver.UploadPart(ctx, key, id, n, body, size)
}

func TestUploadPartRetryRewinds(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", &flakyParts{Driver: memory.New()}, filesystem.WithInterceptors(retryTwice))
	id, _ := d.CreateMultipart(ctx, "k", filesystem.WriteOptions{})
	p, err := d.UploadPart(ctx, "k", id, 1, strings.NewReader("part"), 4)
	if err != nil || p.Size != 4 {
		t.Fatalf("retried part = %+v, %v", p, err)
	}
	seekBad := mustDisk(t, "d", &flakyParts{Driver: memory.New()}, filesystem.WithInterceptors(retryTwice))
	id, _ = seekBad.CreateMultipart(ctx, "k", filesystem.WriteOptions{})
	_, err = seekBad.UploadPart(ctx, "k", id, 1, &seekFails{Reader: strings.NewReader("x"), failAt: 2}, 1)
	if err == nil {
		t.Error("retry ignored a failing rewind")
	}
}

// failingMultipart fails CreateMultipart and PresignPart at the driver.
type failingMultipart struct{ fullDriver }

func (failingMultipart) CreateMultipart(context.Context, string, filesystem.WriteOptions) (string, error) {
	return "", filesystem.ErrPermission
}
func (failingMultipart) PresignPart(context.Context, string, string, int, time.Duration) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{}, filesystem.ErrPermission
}

func TestMultipartDriverErrors(t *testing.T) {
	d := mustDisk(t, "d", failingMultipart{fullDriver{memory.New()}})
	_, err := d.CreateMultipart(t.Context(), "k", filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodePermission)
	_, err = d.PresignPart(t.Context(), "k", "id", 1, time.Minute)
	wantCode(t, err, filesystem.CodePermission)
}

func TestWriteRetryRewindFailure(t *testing.T) {
	d := mustDisk(t, "d", &flakyWriter{basicDriver: newBasic()}, filesystem.WithInterceptors(retryTwice))
	// Seeks: 1 records the start, 2 rewinds after sniffing, 3 rewinds for
	// the retry and fails.
	_, err := d.Write(t.Context(), "k", &seekFails{Reader: strings.NewReader("x"), failAt: 3}, filesystem.WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "seek failed") {
		t.Errorf("failed rewind on retry: %v", err)
	}
}

// metadataNoCopy stores metadata but has no server-side copy, so the
// streamed copy must carry metadata across.
type metadataNoCopy struct{ *basicDriver }

func (metadataNoCopy) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{Metadata: true, ContentType: true}
}

func TestStreamCopyKeepsMetadata(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", metadataNoCopy{newBasic()})
	_, _ = d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	if _, err := d.Copy(ctx, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(ctx, "b"); e.Metadata["k"] != "v" {
		t.Errorf("metadata lost in streamed copy: %+v", e)
	}
}

// blankVisibility stores visibility but does not report it from Write, so
// the Disk must fill in what it asked for.
type blankVisibility struct{ *memory.Driver }

func (b blankVisibility) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	e, err := b.Driver.Write(ctx, key, body, o)
	e.Visibility = ""
	return e, err
}

func TestWriteFillsVisibility(t *testing.T) {
	d := mustDisk(t, "d", blankVisibility{memory.New()})
	e, err := d.WriteBytes(t.Context(), "k", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	if err != nil || e.Visibility != filesystem.VisibilityPublic {
		t.Errorf("entry visibility = %q, %v", e.Visibility, err)
	}
}

// strayReader returns a reader together with its error, which the Disk must
// close rather than leak.
type strayReader struct {
	*basicDriver
	closed bool
}

func (s *strayReader) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return &closeRecorder{Reader: strings.NewReader(""), closed: &s.closed}, filesystem.ErrNotFound
}

type closeRecorder struct {
	io.Reader
	closed *bool
}

func (c *closeRecorder) Close() error { *c.closed = true; return nil }

func TestReadClosesAReaderReturnedWithAnError(t *testing.T) {
	drv := &strayReader{basicDriver: newBasic()}
	rc, err := mustDisk(t, "d", drv).Read(t.Context(), "k")
	if rc != nil || filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Read = %v, %v", rc, err)
	}
	if !drv.closed {
		t.Error("stray reader leaked")
	}
}

// A positive SizeHint is a declaration: a body that ends early or runs past
// it is refused and never stored, even by a driver that ignores read errors.
func TestSizeHintIsHeldToTheBody(t *testing.T) {
	ctx := t.Context()
	for name, driver := range map[string]filesystem.Driver{"memory": memory.New(), "buggy": swallower{newBasic()}} {
		d := mustDisk(t, "d", driver)
		for _, c := range []struct {
			body string
			hint int64
			ok   bool
		}{
			{"0123456789", 10, true},
			{"01234", 10, false},
			{"0123456789abcdef", 10, false},
			{"", 0, true},
			{"anything", 0, true},
		} {
			// A non-seekable reader, as an HTTP body is.
			e, err := d.Write(ctx, "k", io.MultiReader(strings.NewReader(c.body)), filesystem.WriteOptions{SizeHint: c.hint})
			if c.ok != (err == nil) || (!c.ok && filesystem.CodeOf(err) != filesystem.CodeInvalidArgument) {
				t.Errorf("%s: %d bytes with hint %d: %v", name, len(c.body), c.hint, err)
			}
			if ok, _ := d.Exists(ctx, "k"); ok != c.ok {
				t.Errorf("%s: %d bytes with hint %d: stored=%v", name, len(c.body), c.hint, ok)
			}
			if c.ok && e.Size != int64(len(c.body)) {
				t.Errorf("%s: size %d", name, e.Size)
			}
			_ = d.Delete(ctx, "k")
		}
	}
}
