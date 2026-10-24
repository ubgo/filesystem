package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/archive"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// errInjected is the failure a test makes a disk operation return.
var errInjected = filesystem.Errorf(filesystem.CodeUnavailable, "injected")

// failingOn returns a memory disk whose operations of kind fail with
// errInjected.
func failingOn(t *testing.T, kind filesystem.OpKind) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("f", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == kind {
			return errInjected
		}
		return call(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// rawZip writes one entry whose bytes are given as stored in the archive
// (already compressed with method, or not at all), so a test can carry an
// unknown method or a broken stream that zip.Writer would never produce.
func rawZip(t *testing.T, name string, method uint16, raw []byte, size uint64) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{Name: name, Method: method, CompressedSize64: uint64(len(raw)), UncompressedSize64: size, CRC32: crc32.ChecksumIEEE(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// truncatedDeflate is a zip entry named name whose deflate stream of 8 KiB
// (starting with head) is cut to keep bytes: what precedes the cut
// decompresses, then the stream ends early.
func truncatedDeflate(t *testing.T, name string, head []byte, keep int) []byte {
	t.Helper()
	body := make([]byte, 8<<10)
	_, _ = rand.Read(body)
	copy(body, head)
	var c bytes.Buffer
	fw, err := flate.NewWriter(&c, flate.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(body)
	_ = fw.Close()
	return rawZip(t, name, zip.Deflate, c.Bytes()[:keep], uint64(len(body)))
}

// cutInHead cuts a stream inside the 512 bytes the walk peeks at;
// cutPastHead cuts it after them.
const (
	cutInHead   = 100
	cutPastHead = 4 << 10
)

// errReaderAt fails every read.
type errReaderAt struct{}

func (errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, errInjected }

// TestCorruptInput pins that every way an archive can be broken is a
// CodeRejected ErrFormat (or the reader's own coded error), never a panic
// or a partial success: an unreadable source, a gzip with a bad header or
// a broken stream, a tar with a bad header, a zip entry of an unknown
// method, and an entry whose stream ends early (inside its first block,
// in a nested archive, and after a visit that left it unread).
func TestCorruptInput(t *testing.T) {
	if _, err := archive.List(t.Context(), errReaderAt{}, 100, archive.Limits{}); !errors.Is(err, errInjected) {
		t.Fatalf("unreadable source: %v, want the read error", err)
	}
	brokenGzip := gzipOf(t, bytes.Repeat([]byte("hello "), 200))
	for i := 10; i < len(brokenGzip)-8; i++ {
		brokenGzip[i] = 0xff
	}
	brokenTar := tarOf(t, false, fixture{name: "a", body: []byte("a")})
	brokenTar[0] ^= 0xff // the header's checksum no longer matches
	for name, b := range map[string][]byte{
		"gzip header":        append([]byte{0x1f, 0x8b, 0x00}, bytes.Repeat([]byte{1}, 20)...),
		"gzip stream":        brokenGzip,
		"tar header":         brokenTar,
		"unknown zip method": rawZip(t, "a", 99, []byte("x"), 1),
		"short first block":  truncatedDeflate(t, "a.bin", nil, cutInHead),
		"short nested":       truncatedDeflate(t, "in.zip", []byte("PK\x03\x04"), cutPastHead),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := list(t, b, archive.Limits{})
			wantRejected(t, err, archive.ErrFormat)
		})
	}
	t.Run("short entry left unread", func(t *testing.T) {
		d := newDisk(t)
		b := truncatedDeflate(t, "a.bin", nil, cutPastHead)
		if _, err := d.WriteBytes(t.Context(), "u", b, filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		err := archive.Inspect(archive.Limits{}, nil)(t.Context(), d, upload.Result{Key: "u"})
		wantRejected(t, err, archive.ErrFormat)
	})
}

// TestGzipNamedOutsideTheRoot pins that the name a gzip header records
// goes through the same name rules as an archive entry.
func TestGzipNamedOutsideTheRoot(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Name = "/etc/passwd"
	_, _ = gw.Write([]byte("x"))
	_ = gw.Close()
	_, err := list(t, buf.Bytes(), archive.Limits{})
	wantRejected(t, err, archive.ErrUnsafeEntry)
}

// TestPaxGlobalHeadersCountAsEntries pins that metadata-only tar headers
// count against MaxEntries, so a stream of them is not a free loop.
func TestPaxGlobalHeadersCountAsEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for range 2 {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_, err := list(t, buf.Bytes(), archive.Limits{MaxEntries: 1})
	wantRejected(t, err, archive.ErrLimit)
}

// TestCancelledTar pins that a tar walk stops with the context's own
// error (not ErrFormat) when its context has ended.
func TestCancelledTar(t *testing.T) {
	b := tarOf(t, false, fixture{name: "a", body: []byte("a")})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := archive.List(ctx, bytes.NewReader(b), int64(len(b)), archive.Limits{}); !errors.Is(err, context.Canceled) || errors.Is(err, archive.ErrFormat) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestListDirectoriesAndStreamedLimits pins that List reports directory
// entries (with no content) and that a limit crossed while it counts a
// gzip's content refuses the listing.
func TestListDirectoriesAndStreamedLimits(t *testing.T) {
	got, err := list(t, zipOf(t, fixture{name: "./"}, fixture{name: "d/"}, fixture{name: "d/a", body: []byte("a")}), archive.Limits{})
	if err != nil || len(got) != 2 || !got[0].IsDir || got[0].Name != "d" || got[1].Size != 1 {
		t.Fatalf("entries %+v, %v", got, err)
	}
	_, err = list(t, gzipOf(t, make([]byte, 2<<20)), archive.Limits{MaxTotal: 1 << 20, MaxRatio: 1 << 30})
	wantRejected(t, err, archive.ErrLimit)
}

// TestExtractFailures pins Extract's failures: a prefix that escapes, a
// Stat that fails (returned as is), and a cleanup that fails after a late
// refusal (joined to the refusal, so neither is lost).
func TestExtractFailures(t *testing.T) {
	ok := zipOf(t, fixture{name: "a.txt", body: []byte("a")})
	if _, err := archive.Extract(t.Context(), newDisk(t), "../up", bytes.NewReader(ok), int64(len(ok)), archive.ExtractOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Fatalf("escaping prefix: %v, want CodeInvalidKey", err)
	}
	if _, err := archive.Extract(t.Context(), failingOn(t, filesystem.OpStat), "p", bytes.NewReader(ok), int64(len(ok)), archive.ExtractOptions{Overwrite: true}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing stat: %v, want the stat error", err)
	}
	late := zipOf(t, fixture{name: "a.txt", body: []byte("a")}, fixture{name: "../c", body: []byte("c")})
	_, err := archive.Extract(t.Context(), failingOn(t, filesystem.OpDeleteKeys), "p", bytes.NewReader(late), int64(len(late)), archive.ExtractOptions{})
	if !errors.Is(err, archive.ErrUnsafeEntry) || !errors.Is(err, filesystem.ErrUnavailable) {
		t.Fatalf("failed cleanup: %v, want the refusal joined with the cleanup error", err)
	}
}

// TestInspectEdges pins Inspect past the policy cases: a missing upload is
// its Stat error, no check means only the limits apply, a limit crossed
// while a check's entry is counted refuses, a plain error from a check is
// CodeRejected, and a check sees (and may refuse) directory entries of zip
// and tar archives alike.
func TestInspectEdges(t *testing.T) {
	run := func(t *testing.T, body []byte, limits archive.Limits, check func(archive.Entry) error) error {
		t.Helper()
		d := newDisk(t)
		if body != nil {
			if _, err := d.WriteBytes(t.Context(), "u", body, filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		return archive.Inspect(limits, check)(t.Context(), d, upload.Result{Key: "u"})
	}
	if err := run(t, nil, archive.Limits{}, nil); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing upload: %v, want CodeNotFound", err)
	}
	if err := run(t, zipOf(t, fixture{name: "a.exe", body: []byte("MZ")}), archive.Limits{}, nil); err != nil {
		t.Fatalf("no check: %v", err)
	}
	pass := func(archive.Entry) error { return nil }
	err := run(t, gzipOf(t, make([]byte, 2<<20)), archive.Limits{MaxTotal: 1 << 20, MaxRatio: 1 << 30}, pass)
	wantRejected(t, err, archive.ErrLimit)
	plain := errors.New("not on my watch")
	if err := run(t, zipOf(t, fixture{name: "a", body: []byte("a")}), archive.Limits{}, func(archive.Entry) error { return plain }); !errors.Is(err, plain) || filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("plain check error: %v, want it wrapped as CodeRejected", err)
	}
	noDirs := func(e archive.Entry) error {
		if e.IsDir {
			return filesystem.Errorf(filesystem.CodeRejected, "no directories")
		}
		return nil
	}
	for name, b := range map[string][]byte{
		"zip": zipOf(t, fixture{name: "d/"}),
		"tar": tarOf(t, false, fixture{name: "d/", typeflag: tar.TypeDir}),
	} {
		if err := run(t, b, archive.Limits{}, noDirs); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s directory: %v, want the check's refusal", name, err)
		}
	}
	if err := archive.DenyExecutables(archive.Entry{Name: "tools.exe", IsDir: true}); err != nil {
		t.Fatalf("a directory named like a program: %v", err)
	}
}

// shortRead is a memory driver whose reads fail after their first byte
// once broken is set.
type shortRead struct {
	*memory.Driver
	broken bool
}

func (s *shortRead) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := s.Driver.Read(ctx, key, rng)
	if err != nil || !s.broken {
		return rc, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 1), iotestErr{}), rc}, nil
}

// iotestErr fails every read with errInjected.
type iotestErr struct{}

func (iotestErr) Read([]byte) (int, error) { return 0, errInjected }

// TestReaderAtFailures pins ReaderAt's refusals: a missing object, a
// negative offset, a ranged read the disk refuses, a body that fails part
// way, and an object that shrank since it was opened (CodeConflict, not a
// short read the decompressor would take for the end).
func TestReaderAtFailures(t *testing.T) {
	ctx := t.Context()
	if _, err := archive.OpenReaderAt(ctx, newDisk(t), "nope"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing object: %v", err)
	}
	drv := &shortRead{Driver: memory.New()}
	d, err := filesystem.NewDisk("s", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("0123456789"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ra, err := archive.OpenReaderAt(ctx, d, "k")
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 4)
	if _, err := ra.ReadAt(p, -1); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("negative offset: %v", err)
	}
	drv.broken = true
	if _, err := ra.ReadAt(p, 0); !errors.Is(err, errInjected) {
		t.Fatalf("failing body: %v, want the read error", err)
	}
	drv.broken = false
	if _, err := d.WriteBytes(ctx, "k", []byte("01"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.ReadAt(p, 0); filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Fatalf("shrunk object: %v, want CodeConflict", err)
	}
	refusing := failingOn(t, filesystem.OpRead)
	if _, err := refusing.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ra, err = archive.OpenReaderAt(ctx, refusing, "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ra.ReadAt(p, 0); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("refused read: %v, want the read error", err)
	}
}

// plainErrReaderAt fails every read with an error that carries no code,
// as an arbitrary io.ReaderAt may.
type plainErrReaderAt struct{}

func (plainErrReaderAt) ReadAt([]byte, int64) (int, error) { return 0, errors.New("short read") }

// TestBrokenStreamsAreErrFormatEverywhere pins that a truncated entry is a
// CodeRejected ErrFormat whichever entry point reads it: List (which
// counts each entry), Extract (whose disk write consumes it; nothing is
// left behind) and Inspect with a check. An uncoded failure of the
// source itself is ErrFormat too, as every other read of the walk is.
func TestBrokenStreamsAreErrFormatEverywhere(t *testing.T) {
	b := truncatedDeflate(t, "a.bin", nil, cutPastHead)
	_, err := list(t, b, archive.Limits{})
	wantRejected(t, err, archive.ErrFormat)

	d := newDisk(t)
	_, err = archive.Extract(t.Context(), d, "x", bytes.NewReader(b), int64(len(b)), archive.ExtractOptions{})
	wantRejected(t, err, archive.ErrFormat)
	if keys := keysOn(t, d); len(keys) != 0 {
		t.Fatalf("left %v", keys)
	}

	if _, err := d.WriteBytes(t.Context(), "u", b, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	err = archive.Inspect(archive.Limits{}, func(archive.Entry) error { return nil })(t.Context(), d, upload.Result{Key: "u"})
	wantRejected(t, err, archive.ErrFormat)

	_, err = archive.List(t.Context(), plainErrReaderAt{}, 100, archive.Limits{})
	wantRejected(t, err, archive.ErrFormat)
}

// TestExtractKeepsTheDisksOwnError pins the other side of that split: a
// write the disk refuses, of an entry that reads fine, is the disk's
// error, not ErrFormat.
func TestExtractKeepsTheDisksOwnError(t *testing.T) {
	ok := zipOf(t, fixture{name: "a.txt", body: []byte("a")})
	_, err := archive.Extract(t.Context(), failingOn(t, filesystem.OpWrite), "p", bytes.NewReader(ok), int64(len(ok)), archive.ExtractOptions{})
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || errors.Is(err, archive.ErrFormat) {
		t.Fatalf("got %v, want the disk's CodeUnavailable", err)
	}
}
