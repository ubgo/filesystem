package crypt_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/crypt"
	"github.com/ubgo/filesystem/memory"
)

// errInner is the failure a faultyInner returns.
var errInner = filesystem.Errorf(filesystem.CodeUnavailable, "inner down")

// faultyInner is a memory driver whose operations fail on demand: the
// read numbered failRead (from 1, counting every Read), every Stat or
// List while failStat or failList is set, and the body of every read
// after cutAfter bytes while cutAfter is positive.
type faultyInner struct {
	*memory.Driver
	mu       sync.Mutex
	reads    int
	failRead int
	failStat bool
	failList bool
	cutAfter int64
}

func (f *faultyInner) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	f.mu.Lock()
	f.reads++
	fail, cut := f.reads == f.failRead, f.cutAfter
	f.mu.Unlock()
	if fail {
		return nil, errInner
	}
	rc, err := f.Driver.Read(ctx, key, rng)
	if err != nil || cut <= 0 {
		return rc, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, cut), failing{errInner}), rc}, nil
}

func (f *faultyInner) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	f.mu.Lock()
	fail := f.failStat
	f.mu.Unlock()
	if fail {
		return filesystem.Entry{}, errInner
	}
	return f.Driver.Stat(ctx, key)
}

func (f *faultyInner) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	f.mu.Lock()
	fail := f.failList
	f.mu.Unlock()
	if fail {
		return filesystem.Page{}, errInner
	}
	return f.Driver.List(ctx, dir, opts)
}

// arm sets the faults; it resets the read count.
func (f *faultyInner) arm(set func(f *faultyInner)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads, f.failRead, f.failStat, f.failList, f.cutAfter = 0, 0, false, false, 0
	if set != nil {
		set(f)
	}
}

// failing is a reader failing with err.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// keyringFunc is a Keyring built from two functions, for keyrings that
// misbehave.
type keyringFunc struct {
	wrap   func(ctx context.Context, objectKey string, dek []byte) (crypt.WrappedKey, error)
	unwrap func(ctx context.Context, objectKey string, w crypt.WrappedKey) ([]byte, error)
}

func (k keyringFunc) WrapKey(ctx context.Context, objectKey string, dek []byte) (crypt.WrappedKey, error) {
	return k.wrap(ctx, objectKey, dek)
}

func (k keyringFunc) UnwrapKey(ctx context.Context, objectKey string, w crypt.WrappedKey) ([]byte, error) {
	return k.unwrap(ctx, objectKey, w)
}

// faultySetup is setup over a faultyInner.
func faultySetup(t *testing.T) (*crypt.Driver, *faultyInner) {
	t.Helper()
	inner := &faultyInner{Driver: memory.New()}
	return cryptOver(t, inner), inner
}

// TestInnerFailuresAreReturned pins that every failure of the wrapped
// driver reaches the caller unchanged: a List, the header read of a whole
// or ranged read, the Stat and chunk read of a ranged read, and a body
// that fails part way through the header or a chunk.
func TestInnerFailuresAreReturned(t *testing.T) {
	ctx := t.Context()
	drv, inner := faultySetup(t)
	if _, err := drv.Write(ctx, "k", bytes.NewReader(body(2*crypt.ChunkSize+5)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ranged := filesystem.ByteRange{Offset: 1, Length: 10}
	readAll := func(rng filesystem.ByteRange) error {
		rc, err := drv.Read(ctx, "k", rng)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		_, err = io.ReadAll(rc)
		return err
	}
	for name, c := range map[string]struct {
		set func(*faultyInner)
		run func() error
	}{
		"list":                {func(f *faultyInner) { f.failList = true }, func() error { _, err := drv.List(ctx, "", filesystem.ListOptions{}); return err }},
		"whole read":          {func(f *faultyInner) { f.failRead = 1 }, func() error { return readAll(filesystem.ByteRange{}) }},
		"ranged header read":  {func(f *faultyInner) { f.failRead = 1 }, func() error { return readAll(ranged) }},
		"ranged stat":         {func(f *faultyInner) { f.failStat = true }, func() error { return readAll(ranged) }},
		"ranged chunk read":   {func(f *faultyInner) { f.failRead = 2 }, func() error { return readAll(ranged) }},
		"body cut in header":  {func(f *faultyInner) { f.cutAfter = 10 }, func() error { return readAll(filesystem.ByteRange{}) }},
		"body cut in a chunk": {func(f *faultyInner) { f.cutAfter = crypt.HeaderSize + 100 }, func() error { return readAll(filesystem.ByteRange{}) }},
		"body cut at a chunk end": {func(f *faultyInner) { f.cutAfter = crypt.HeaderSize + sealed }, func() error {
			return readAll(filesystem.ByteRange{})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			inner.arm(c.set)
			defer inner.arm(nil)
			if err := c.run(); !errors.Is(err, errInner) {
				t.Fatalf("got %v, want the wrapped driver's error", err)
			}
		})
	}
}

// TestImpossibleStoredSizes pins that a stored size no plaintext encrypts
// to fails List and a ranged read with ErrTampered, as it fails Stat.
func TestImpossibleStoredSizes(t *testing.T) {
	ctx := t.Context()
	drv, inner := faultySetup(t)
	if _, err := drv.Write(ctx, "k", bytes.NewReader(body(100)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := inner.Driver.Read(ctx, "k", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	// The header intact, the body shorter than one tag.
	if _, err := inner.Driver.Write(ctx, "k", bytes.NewReader(stored[:crypt.HeaderSize+3]), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.List(ctx, "", filesystem.ListOptions{}); !errors.Is(err, crypt.ErrTampered) {
		t.Fatalf("List: %v, want ErrTampered", err)
	}
	if _, err := drv.Read(ctx, "k", filesystem.ByteRange{Offset: 1}); !errors.Is(err, crypt.ErrTampered) {
		t.Fatalf("ranged read: %v, want ErrTampered", err)
	}
	// A whole read finds a final chunk shorter than its tag.
	rc, err = drv.Read(ctx, "k", filesystem.ByteRange{})
	if err == nil {
		_, err = io.ReadAll(rc)
		_ = rc.Close()
	}
	if !errors.Is(err, crypt.ErrTampered) {
		t.Fatalf("whole read: %v, want ErrTampered", err)
	}
}

// TestForgedHeaders pins that a header this version did not write fails
// with ErrTampered: another version, another chunk size, and lengths that
// run past the header.
func TestForgedHeaders(t *testing.T) {
	d, inner, _ := setup(t)
	if _, err := d.WriteBytes(t.Context(), "a", body(10), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	orig := raw(t, inner, "a")
	forge := func(edit func(b []byte)) []byte { b := bytes.Clone(orig); edit(b); return b }
	for name, b := range map[string][]byte{
		"version":    forge(func(b []byte) { b[4]++ }),
		"chunk size": forge(func(b []byte) { binary.BigEndian.PutUint32(b[5:], crypt.ChunkSize/2) }),
		"lengths":    forge(func(b []byte) { binary.BigEndian.PutUint16(b[25:], crypt.HeaderSize) }),
	} {
		t.Run(name, func(t *testing.T) {
			putRaw(t, inner, "a", b)
			if err := readErr(t, d, "a"); !errors.Is(err, crypt.ErrTampered) {
				t.Fatalf("got %v, want ErrTampered", err)
			}
			if _, err := d.ReadRange(t.Context(), "a", filesystem.ByteRange{Offset: 1, Length: 2}); !errors.Is(err, crypt.ErrTampered) {
				t.Fatalf("ranged read: %v, want ErrTampered", err)
			}
		})
	}
}

// TestKeyringFailures pins the keyring's failures: a key ID the keyring
// cannot choose, a master key the KMS does not hold, a wrapped key too
// large for the header, a data key of the wrong size, and a wrapped key
// shorter than a nonce. None of them stores anything.
func TestKeyringFailures(t *testing.T) {
	ctx := t.Context()
	noID := errors.New("no tenant in key")
	kms := crypt.NewMemoryKMS()
	if err := kms.Add("k1", masterKey); err != nil {
		t.Fatal(err)
	}
	write := func(t *testing.T, k crypt.Keyring) error {
		t.Helper()
		inner := memory.New()
		drv, err := crypt.New(inner, k)
		if err != nil {
			t.Fatal(err)
		}
		_, err = drv.Write(ctx, "a", strings.NewReader("x"), filesystem.WriteOptions{})
		if _, serr := inner.Stat(ctx, "a"); serr == nil {
			t.Fatal("a failed write stored the object")
		}
		return err
	}
	if err := write(t, crypt.NewKMSKeyring(kms, func(context.Context, string) (string, error) { return "", noID })); !errors.Is(err, noID) {
		t.Errorf("key ID: %v", err)
	}
	if err := write(t, crypt.NewKMSKeyring(kms, func(context.Context, string) (string, error) { return "absent", nil })); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("missing master key: %v, want CodePermission", err)
	}
	huge := keyringFunc{wrap: func(context.Context, string, []byte) (crypt.WrappedKey, error) {
		return crypt.WrappedKey{KeyID: "k", Ciphertext: make([]byte, crypt.HeaderSize)}, nil
	}}
	if err := write(t, huge); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("wrapped key too large: %v, want CodeInvalidArgument", err)
	}

	// Reads through keyrings that unwrap badly.
	d, inner, _ := setup(t)
	if _, err := d.WriteBytes(ctx, "a", body(10), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	short := keyringFunc{unwrap: func(context.Context, string, crypt.WrappedKey) ([]byte, error) { return make([]byte, 16), nil }}
	drv, err := crypt.New(inner, short)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Read(ctx, "a", filesystem.ByteRange{}); !errors.Is(err, crypt.ErrTampered) {
		t.Errorf("short data key: %v, want ErrTampered", err)
	}
	if _, err := kms.Decrypt(ctx, "k1", []byte("short"), nil); !errors.Is(err, crypt.ErrTampered) {
		t.Errorf("wrapped key shorter than a nonce: %v, want ErrTampered", err)
	}
}

// TestRotateFailures pins Rotate's refusals: an unclean key, a missing
// object, a read that fails, an object that is not crypt's, and a keyring
// that cannot wrap or whose wrapped key does not fit. The object is left
// as it was.
func TestRotateFailures(t *testing.T) {
	ctx := t.Context()
	drv, inner := faultySetup(t)
	if _, err := drv.Write(ctx, "k", strings.NewReader("secret"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := drv.Rotate(ctx, "../k"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("unclean key: %v", err)
	}
	if err := drv.Rotate(ctx, "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
	inner.arm(func(f *faultyInner) { f.failRead = 1 })
	if err := drv.Rotate(ctx, "k"); !errors.Is(err, errInner) {
		t.Errorf("failing read: %v", err)
	}
	inner.arm(nil)
	if _, err := inner.Driver.Write(ctx, "plain", strings.NewReader(strings.Repeat("p", crypt.HeaderSize+40)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := drv.Rotate(ctx, "plain"); !errors.Is(err, crypt.ErrTampered) {
		t.Errorf("not a crypt object: %v", err)
	}

	kms := crypt.NewMemoryKMS()
	if err := kms.Add("k1", masterKey); err != nil {
		t.Fatal(err)
	}
	ring := crypt.NewKMSKeyring(kms, func(context.Context, string) (string, error) { return "k1", nil })
	noWrap := errors.New("kms down")
	for name, k := range map[string]crypt.Keyring{
		"wrap fails": keyringFunc{unwrap: ring.UnwrapKey, wrap: func(context.Context, string, []byte) (crypt.WrappedKey, error) {
			return crypt.WrappedKey{}, noWrap
		}},
		"wrapped key too large": keyringFunc{unwrap: ring.UnwrapKey, wrap: func(context.Context, string, []byte) (crypt.WrappedKey, error) {
			return crypt.WrappedKey{KeyID: "k", Ciphertext: make([]byte, crypt.HeaderSize)}, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			rot, err := crypt.New(inner, k)
			if err != nil {
				t.Fatal(err)
			}
			before := rawOf(t, inner.Driver, "k")
			if err := rot.Rotate(ctx, "k"); err == nil {
				t.Fatal("Rotate succeeded")
			}
			if !bytes.Equal(rawOf(t, inner.Driver, "k"), before) {
				t.Fatal("a failed rotation changed the object")
			}
		})
	}
}

// rawOf is raw for any memory driver.
func rawOf(t *testing.T, inner *memory.Driver, key string) []byte {
	t.Helper()
	return raw(t, inner, key)
}

// errAfterChunk is a body whose first read fills a whole chunk and whose
// next read fails, so the failure is found by the look-ahead that decides
// whether the chunk is the final one.
type errAfterChunk struct{ sent bool }

func (b *errAfterChunk) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errInner
	}
	b.sent = true
	n := min(len(p), crypt.ChunkSize)
	for i := range p[:n] {
		p[i] = 'x'
	}
	return n, nil
}

// TestWriteBodyFailingAtAChunkBoundary pins that a body failing just
// after a full chunk fails the write, rather than sealing that chunk as
// the final one.
func TestWriteBodyFailingAtAChunkBoundary(t *testing.T) {
	drv, inner := faultySetup(t)
	if _, err := drv.Write(t.Context(), "k", &errAfterChunk{}, filesystem.WriteOptions{}); !errors.Is(err, errInner) {
		t.Fatalf("got %v, want the body's error", err)
	}
	if _, err := inner.Driver.Stat(t.Context(), "k"); err == nil {
		t.Fatal("a failed body was stored")
	}
}

// TestEveryForwardWithoutCapability pins that every forwarded optional
// interface answers CodeUnsupported over a driver that lacks it, rather
// than panicking or pretending.
func TestEveryForwardWithoutCapability(t *testing.T) {
	drv := cryptOver(t, bareDriver{inner: memory.New()})
	ctx := t.Context()
	errs := map[string]error{
		"SetVisibility":   drv.SetVisibility(ctx, "a", filesystem.VisibilityPublic),
		"DeletePrefix":    drv.DeletePrefix(ctx, "a"),
		"DeleteKeys":      drv.DeleteKeys(ctx, []string{"a"}),
		"MakeDir":         drv.MakeDir(ctx, "a"),
		"SetModTime":      drv.SetModTime(ctx, "a", time.Now()),
		"SetTags":         drv.SetTags(ctx, "a", nil),
		"SetStorageClass": drv.SetStorageClass(ctx, "a", "COLD"),
	}
	_, errs["SweepAbandoned"] = drv.SweepAbandoned(ctx, time.Now())
	_, errs["FindAbandoned"] = drv.FindAbandoned(ctx, time.Now())
	_, errs["DirExists"] = drv.DirExists(ctx, "a")
	_, errs["Tags"] = drv.Tags(ctx, "a")
	for name, err := range errs {
		if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
			t.Errorf("%s: %v, want CodeUnsupported", name, err)
		}
	}
	inner := memory.New()
	if got := cryptOver(t, inner).Unwrap(); got != inner {
		t.Fatalf("Unwrap = %v, want the wrapped driver (filesystem.As follows it)", got)
	}
}
