package filesystem_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// closeCount counts how many readers it handed out were closed.
type closeCount struct{ n atomic.Int64 }

type countedReader struct {
	io.Reader
	c *closeCount
}

func (r countedReader) Close() error {
	r.c.n.Add(1)
	return nil
}

// countingDriver is a versioned memory driver whose readers count their
// Close calls, so a test can prove the Disk never leaks a reader it
// discards.
type countingDriver struct {
	*memory.Driver
	closes closeCount
}

func (d *countingDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := d.Driver.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	return countedReader{Reader: rc, c: &d.closes}, nil
}

// hedgeTwice stands in for a hedging interceptor: on a hedgeable
// operation it marks the calls concurrent and runs two that both succeed,
// recording what the second returned.
func hedgeTwice(second *error) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if !op.Hedgeable {
			return call(ctx)
		}
		op.Concurrent = true
		if err := call(ctx); err != nil {
			return err
		}
		*second = call(ctx)
		return nil
	}
}

// TestConcurrentAttemptsKeepTheFirstResult proves that when two
// concurrent attempts of a Stat or Read both succeed, the later one is
// superseded (closing its reader) and the first result is returned.
func TestConcurrentAttemptsKeepTheFirstResult(t *testing.T) {
	ctx := t.Context()
	drv := &countingDriver{Driver: memory.New()}
	var second error
	d := mustDisk(t, "d", drv, filesystem.WithInterceptors(hedgeTwice(&second)))
	if _, err := d.WriteBytes(ctx, "k", []byte("body"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e, err := d.Stat(ctx, "k"); err != nil || e.Size != 4 {
		t.Fatalf("stat = %+v, %v", e, err)
	}
	if !errors.Is(second, filesystem.ErrSuperseded) {
		t.Fatalf("second stat returned %v, want ErrSuperseded", second)
	}
	second = nil
	b, err := d.ReadAll(ctx, "k")
	if err != nil || string(b) != "body" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if !errors.Is(second, filesystem.ErrSuperseded) {
		t.Fatalf("second read returned %v, want ErrSuperseded", second)
	}
	if n := drv.closes.n.Load(); n != 2 {
		t.Fatalf("%d readers closed, want 2 (the superseded one and the one returned)", n)
	}
}

// TestFailedReadClosesAnOpenedReader proves a read that opened the object
// but whose interceptor chain then failed closes that reader and runs the
// operation's OnClose functions.
func TestFailedReadClosesAnOpenedReader(t *testing.T) {
	ctx := t.Context()
	drv := &countingDriver{Driver: memory.New()}
	released := 0
	failAfter := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind != filesystem.OpRead {
			return call(ctx)
		}
		op.OnClose(func() { released++ })
		if err := call(ctx); err != nil {
			return err
		}
		return errBroken
	}
	d := mustDisk(t, "d", drv, filesystem.WithInterceptors(failAfter))
	if _, err := d.WriteBytes(ctx, "k", []byte("body"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if rc, err := d.Read(ctx, "k"); !isBroken(err) || rc != nil {
		t.Fatalf("read = %v, %v; want the interceptor's error and no reader", rc, err)
	}
	if n := drv.closes.n.Load(); n != 1 || released != 1 {
		t.Fatalf("closed %d readers and released %d times, want 1 and 1", n, released)
	}
}

// TestWrapBodyAndOnClose proves an interceptor's WrapBody sees the bytes
// of a write before the driver and of a read before the caller, and that
// OnClose runs exactly once when the caller closes the reader.
func TestWrapBodyAndOnClose(t *testing.T) {
	ctx := t.Context()
	upper := func(r io.Reader) io.Reader {
		b, err := io.ReadAll(r)
		if err != nil {
			return iotest.ErrReader(err)
		}
		return bytes.NewReader(bytes.ToUpper(b))
	}
	closed := 0
	wrap := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		switch op.Kind {
		case filesystem.OpWrite:
			op.WrapBody(upper)
		case filesystem.OpRead:
			op.WrapBody(func(r io.Reader) io.Reader { return io.MultiReader(r, strings.NewReader("!")) })
			op.OnClose(func() { closed++ })
		}
		return call(ctx)
	}
	raw := memory.New()
	d := mustDisk(t, "d", raw, filesystem.WithInterceptors(wrap))
	if _, err := d.WriteBytes(ctx, "k", []byte("abc"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	stored := mustDisk(t, "raw", raw)
	if b, err := stored.ReadAll(ctx, "k"); err != nil || string(b) != "ABC" {
		t.Fatalf("stored %q, %v; want the wrapped write body", b, err)
	}
	rc, err := d.Read(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil || string(b) != "ABC!" {
		t.Fatalf("read %q, %v; want the wrapped read body", b, err)
	}
	if closed != 0 {
		t.Fatal("OnClose ran before the reader was closed")
	}
	_ = rc.Close()
	_ = rc.Close()
	if closed != 1 {
		t.Fatalf("OnClose ran %d times, want once", closed)
	}
}

// brokenBody is the memory driver whose readers fail after opening.
type brokenBody struct{ *memory.Driver }

func (brokenBody) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return io.NopCloser(iotest.ErrReader(errBroken)), nil
}

// TestDataURIReadFailures proves DataURI returns the failure of opening
// or reading the object instead of a URI of what it got so far.
func TestDataURIReadFailures(t *testing.T) {
	ctx := t.Context()
	unopenable := newBreakable()
	unreadable := brokenBody{memory.New()}
	for name, drv := range map[string]filesystem.Driver{"open": unopenable, "read": unreadable} {
		d := mustDisk(t, "d", drv)
		if _, err := d.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		unopenable.set(true, filesystem.OpRead)
		if uri, err := d.DataURI(ctx, "a.txt", 100); !isBroken(err) || uri != "" {
			t.Errorf("%s failure: %q, %v; want the failure", name, uri, err)
		}
	}
}

// TestDeletePrefixFallbackPages proves the portable DeletePrefix of a
// driver without the capability follows the listing past its first page.
func TestDeletePrefixFallbackPages(t *testing.T) {
	ctx := t.Context()
	d := mustDisk(t, "d", newBasic())
	n := filesystem.DefaultPageSize + 2
	for i := range n {
		if _, err := d.WriteBytes(ctx, fmt.Sprintf("dir/%05d", i), nil, filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.WriteBytes(ctx, "keep", nil, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeletePrefix(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	if keys := allKeys(t, d, ""); len(keys) != 1 || keys[0] != "keep" {
		t.Fatalf("left %d keys (%v...), want only keep", len(keys), keys[:min(len(keys), 3)])
	}
}

// leakyVersions returns a reader together with an error from
// ReadVersion, as a careless driver might.
type leakyVersions struct {
	*memory.Driver
	closes closeCount
}

func (d *leakyVersions) ReadVersion(context.Context, string, string) (io.ReadCloser, error) {
	return countedReader{Reader: strings.NewReader("partial"), c: &d.closes}, errBroken
}

// TestReadVersionRefusalsAndFailures proves ReadVersion refuses an
// invalid key and an empty id before the driver is called, and that a
// driver failing with a reader in hand has that reader closed.
func TestReadVersionRefusalsAndFailures(t *testing.T) {
	ctx := t.Context()
	drv := &leakyVersions{Driver: memory.New(memory.WithVersions())}
	d := mustDisk(t, "d", drv)
	if _, err := d.ReadVersion(ctx, "../x", "1"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("invalid key: %v", err)
	}
	if _, err := d.ReadVersion(ctx, "k", ""); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("empty id: %v", err)
	}
	if drv.closes.n.Load() != 0 {
		t.Fatal("a refused call reached the driver")
	}
	if rc, err := d.ReadVersion(ctx, "k", "1"); !isBroken(err) || rc != nil {
		t.Fatalf("failing driver: %v, %v; want its error and no reader", rc, err)
	}
	if drv.closes.n.Load() != 1 {
		t.Fatal("the reader returned with the error was not closed")
	}
}

// TestParseAllowedIPMappedPrefixes pins that an IPv4-mapped prefix is
// unmapped to the IPv4 prefix it covers, and one shorter than /96, which
// would also cover non-IPv4 addresses, is refused as non-canonical.
func TestParseAllowedIPMappedPrefixes(t *testing.T) {
	for in, want := range map[string]string{"::ffff:203.0.113.0/120": "203.0.113.0/24", "::ffff:0.0.0.0/96": "0.0.0.0/0"} {
		if p, err := filesystem.ParseAllowedIP(in); err != nil || p.String() != want {
			t.Errorf("ParseAllowedIP(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	// Every mapped prefix shorter than /96 has ::ffff bits past its
	// length, so it is not canonical and is refused before being unmapped.
	for bits := range 96 {
		in := fmt.Sprintf("::ffff:203.0.113.0/%d", bits)
		if _, err := filesystem.ParseAllowedIP(in); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("ParseAllowedIP(%q) = %v, want CodeInvalidArgument", in, err)
		}
	}
}

// halfOpen fails reads as unavailable while handing back an opened
// reader, which the failover must close before trying the next disk.
type halfOpen struct {
	*memory.Driver
	closes closeCount
}

func (d *halfOpen) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return countedReader{Reader: strings.NewReader("stale"), c: &d.closes}, filesystem.ErrUnavailable
}

func TestFailoverClosesTheFailedReader(t *testing.T) {
	ctx := t.Context()
	primary := &halfOpen{Driver: memory.New()}
	secondary := mustDisk(t, "secondary", memory.New())
	if _, err := secondary.WriteBytes(ctx, "k", []byte("good"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := filesystem.Failover("f", mustDisk(t, "primary", primary), []*filesystem.Disk{secondary}, filesystem.FailoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := f.ReadAll(ctx, "k"); err != nil || string(b) != "good" {
		t.Fatalf("read = %q, %v; want the secondary's copy", b, err)
	}
	if primary.closes.n.Load() != 1 {
		t.Fatal("the primary's reader was not closed")
	}
}
