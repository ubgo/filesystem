package archive_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/archive"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// fixture is one entry of an archive built by a test. Fixtures are built in
// the test rather than checked in, so the attack each one carries is
// readable here.
type fixture struct {
	name     string
	body     []byte
	mode     fs.FileMode // zip: the entry's mode when non-zero
	typeflag byte        // tar: the entry's type when non-zero
	stored   bool        // zip: store without compressing
}

func zipOf(t *testing.T, entries ...fixture) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range entries {
		fh := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		if f.stored {
			fh.Method = zip.Store
		}
		if f.mode != 0 {
			fh.SetMode(f.mode)
		}
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarOf(t *testing.T, gz bool, entries ...fixture) []byte {
	t.Helper()
	var buf bytes.Buffer
	var out io.Writer = &buf
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(&buf)
		out = gzw
	}
	tw := tar.NewWriter(out)
	for _, f := range entries {
		h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if f.typeflag != 0 {
			h.Typeflag = f.typeflag
			if f.typeflag != tar.TypeReg {
				h.Size = 0
				h.Linkname = "/etc/passwd"
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(f.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gzw != nil {
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// counting is an io.ReaderAt that records how many bytes were read, to
// show a refusal happened before the whole archive was consumed.
type counting struct {
	r    *bytes.Reader
	read atomic.Int64
}

func (c *counting) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read.Add(int64(n))
	return n, err
}

func list(t *testing.T, b []byte, l archive.Limits) ([]archive.Entry, error) {
	t.Helper()
	return archive.List(t.Context(), bytes.NewReader(b), int64(len(b)), l)
}

func wantRejected(t *testing.T, err, sentinel error) {
	t.Helper()
	if !errors.Is(err, sentinel) || filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("got %v (code %q), want %v with CodeRejected", err, filesystem.CodeOf(err), sentinel)
	}
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("a", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func keysOn(t *testing.T, d *filesystem.Disk) []string {
	t.Helper()
	var keys []string
	for e, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// zeros is the classic bomb payload: it compresses about a thousand times.
var zeros = make([]byte, 32<<20)

func TestBombs(t *testing.T) {
	for name, b := range map[string][]byte{
		"zip":    zipOf(t, fixture{name: "zeros.bin", body: zeros}),
		"tar.gz": tarOf(t, true, fixture{name: "zeros.bin", body: zeros}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := list(t, b, archive.Limits{})
			wantRejected(t, err, archive.ErrLimit)
			// The same archive passes when the application allows it,
			// so the refusal is the ratio and not something else.
			if _, err := list(t, b, archive.Limits{MaxRatio: 1 << 20}); err != nil {
				t.Fatalf("with a high ratio: %v", err)
			}
		})
	}
}

// The limits stop the decompression part-way: a tar.gz of many entries,
// each honestly declared, is refused at the entry that crosses MaxTotal,
// long before the archive has been read.
func TestLimitsEnforcedWhileStreaming(t *testing.T) {
	var entries []fixture
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	for i := range 64 {
		entries = append(entries, fixture{name: "f" + strings.Repeat("x", i), body: chunk})
	}
	b := tarOf(t, true, entries...)
	src := &counting{r: bytes.NewReader(b)}
	_, err := archive.List(t.Context(), src, int64(len(b)), archive.Limits{MaxTotal: 512 << 10, MaxRatio: 1 << 20})
	wantRejected(t, err, archive.ErrLimit)
	if got := src.read.Load(); got > int64(len(b))/2 {
		t.Fatalf("read %d of %d archive bytes before refusing", got, len(b))
	}
}

// Headers decompress too: a tar.gz of empty entries is all header blocks,
// which the meter counts like content.
func TestHeadersCountAgainstRatio(t *testing.T) {
	var entries []fixture
	for i := range 8000 {
		entries = append(entries, fixture{name: fmt.Sprintf("e/%05d", i)})
	}
	b := tarOf(t, true, entries...)
	_, err := list(t, b, archive.Limits{MaxRatio: 10})
	wantRejected(t, err, archive.ErrLimit)
}

func TestMaxEntries(t *testing.T) {
	var entries []fixture
	for i := range 11 {
		entries = append(entries, fixture{name: string(rune('a' + i)), body: []byte("x")})
	}
	for name, b := range map[string][]byte{"zip": zipOf(t, entries...), "tar": tarOf(t, false, entries...)} {
		_, err := list(t, b, archive.Limits{MaxEntries: 10})
		if !errors.Is(err, archive.ErrLimit) {
			t.Errorf("%s: %v", name, err)
		}
		if got, err := list(t, b, archive.Limits{MaxEntries: 11}); err != nil || len(got) != 11 {
			t.Errorf("%s at the limit: %d entries, %v", name, len(got), err)
		}
	}
}

// Every zip-slip shape and every non-regular entry is refused, by List and
// by Extract, which stores nothing.
func TestUnsafeEntries(t *testing.T) {
	ok := fixture{name: "fine.txt", body: []byte("fine")}
	cases := map[string][]byte{
		"zip parent":         zipOf(t, ok, fixture{name: "../evil.txt", body: []byte("x")}),
		"zip deep parent":    zipOf(t, ok, fixture{name: "a/b/../../../evil.txt", body: []byte("x")}),
		"zip backslash":      zipOf(t, ok, fixture{name: `..\..\evil.txt`, body: []byte("x")}),
		"zip absolute":       zipOf(t, ok, fixture{name: "/etc/cron.d/evil", body: []byte("x")}),
		"zip drive":          zipOf(t, ok, fixture{name: `C:\Windows\evil.dll`, body: []byte("x")}),
		"zip symlink":        zipOf(t, ok, fixture{name: "link", body: []byte("/etc/passwd"), mode: fs.ModeSymlink | 0o777}),
		"zip fifo":           zipOf(t, ok, fixture{name: "pipe", mode: fs.ModeNamedPipe | 0o644}),
		"zip duplicate":      zipOf(t, ok, fixture{name: "fine.txt", body: []byte("shadow")}),
		"zip control char":   zipOf(t, ok, fixture{name: "a\x00.txt", body: []byte("x")}),
		"tar parent":         tarOf(t, false, ok, fixture{name: "../evil.txt", body: []byte("x")}),
		"tar absolute":       tarOf(t, false, ok, fixture{name: "/etc/passwd", body: []byte("x")}),
		"tar symlink":        tarOf(t, false, ok, fixture{name: "link", typeflag: tar.TypeSymlink}),
		"tar hard link":      tarOf(t, false, ok, fixture{name: "link", typeflag: tar.TypeLink}),
		"tar char device":    tarOf(t, false, ok, fixture{name: "dev", typeflag: tar.TypeChar}),
		"tar block device":   tarOf(t, false, ok, fixture{name: "dev", typeflag: tar.TypeBlock}),
		"tar fifo":           tarOf(t, false, ok, fixture{name: "pipe", typeflag: tar.TypeFifo}),
		"tar.gz parent":      tarOf(t, true, ok, fixture{name: "x/../../evil.txt", body: []byte("x")}),
		"tar.gz duplicate":   tarOf(t, true, ok, fixture{name: "./fine.txt", body: []byte("shadow")}),
		"nested zip in zip":  zipOf(t, ok, fixture{name: "inner.zip", body: zipOf(t, fixture{name: "../evil.txt", body: []byte("x")})}),
		"nested tar symlink": zipOf(t, ok, fixture{name: "inner.tar", body: tarOf(t, false, fixture{name: "l", typeflag: tar.TypeSymlink})}),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := list(t, b, archive.Limits{})
			wantRejected(t, err, archive.ErrUnsafeEntry)
			d := newDisk(t)
			_, err = archive.Extract(t.Context(), d, "up", bytes.NewReader(b), int64(len(b)), archive.ExtractOptions{Limits: archive.Limits{}})
			wantRejected(t, err, archive.ErrUnsafeEntry)
			if keys := keysOn(t, d); len(keys) != 0 {
				t.Fatalf("extract left %v behind", keys)
			}
		})
	}
}

func TestNestedArchivesCountAgainstDepth(t *testing.T) {
	inner := zipOf(t, fixture{name: "a.txt", body: []byte("inner")})
	outer := zipOf(t, fixture{name: "docs/inner.zip", body: inner})
	entries, err := list(t, outer, archive.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path())
	}
	if strings.Join(paths, ",") != "docs/inner.zip!/a.txt,docs/inner.zip" {
		t.Fatalf("paths %v", paths)
	}
	if entries[1].Depth != 1 || !entries[1].Archive || entries[0].Depth != 2 || entries[0].In != "docs/inner.zip" {
		t.Fatalf("entries %+v", entries)
	}
	_, err = list(t, outer, archive.Limits{MaxDepth: 1})
	wantRejected(t, err, archive.ErrLimit)

	tgz := tarOf(t, true, fixture{name: "deep.tar.gz", body: tarOf(t, true, fixture{name: "x.zip", body: inner})})
	_, err = list(t, tgz, archive.Limits{})
	wantRejected(t, err, archive.ErrLimit)
	if _, err := list(t, tgz, archive.Limits{MaxDepth: 3}); err != nil {
		t.Fatal(err)
	}

	// A gzip file that holds no tar is content, not a level.
	plainGz := func() []byte {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		_, _ = gw.Write([]byte("just text"))
		_ = gw.Close()
		return buf.Bytes()
	}()
	// A gzip file is a level of its own, whose content is checked.
	_, err = list(t, zipOf(t, fixture{name: "notes.txt.gz", body: plainGz}), archive.Limits{MaxDepth: 1})
	wantRejected(t, err, archive.ErrLimit)
	got, err := list(t, zipOf(t, fixture{name: "notes.txt.gz", body: plainGz}), archive.Limits{})
	if err != nil || len(got) != 2 || got[0].Path() != "notes.txt.gz!/notes.txt" || got[0].Format != archive.FormatGzip || got[0].Size != int64(len("just text")) {
		t.Fatalf("plain gzip inside: %+v, %v", got, err)
	}
}

// A bomb stored (not compressed) inside an innocent outer archive is
// measured against the outer archive's size and refused.
func TestNestedBomb(t *testing.T) {
	outer := zipOf(t, fixture{name: "readme.txt", body: []byte("hi")}, fixture{name: "x.zip", body: zipOf(t, fixture{name: "z", body: zeros}), stored: true})
	_, err := list(t, outer, archive.Limits{})
	wantRejected(t, err, archive.ErrLimit)
}

func TestNotAnArchive(t *testing.T) {
	for name, b := range map[string][]byte{
		"text":        []byte("hello"),
		"empty":       nil,
		"corrupt zip": append([]byte("PK\x03\x04"), bytes.Repeat([]byte{1}, 100)...),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := list(t, b, archive.Limits{})
			wantRejected(t, err, archive.ErrFormat)
		})
	}
}

func TestExtract(t *testing.T) {
	b := tarOf(t, true,
		fixture{name: "./", typeflag: tar.TypeDir},
		fixture{name: "photos/", typeflag: tar.TypeDir},
		fixture{name: "photos/a.txt", body: []byte("alpha")},
		fixture{name: "./b.txt", body: []byte("beta")},
		fixture{name: "inner.zip", body: zipOf(t, fixture{name: "c.txt", body: []byte("gamma")})},
	)
	d := newDisk(t)
	got, err := archive.Extract(t.Context(), d, "/imports/42/", bytes.NewReader(b), int64(len(b)), archive.ExtractOptions{Limits: archive.Limits{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("stored %+v", got)
	}
	want := map[string]string{"imports/42/photos/a.txt": "alpha", "imports/42/b.txt": "beta"}
	for k, v := range want {
		if b, err := d.ReadAll(t.Context(), k); err != nil || string(b) != v {
			t.Errorf("%s: %q, %v", k, b, err)
		}
	}
	if keys := keysOn(t, d); len(keys) != 3 {
		t.Errorf("keys %v: the nested archive is stored as a file, not expanded", keys)
	}
}

// A refusal found after files were stored removes them.
func TestExtractRollsBack(t *testing.T) {
	b := zipOf(t,
		fixture{name: "a.txt", body: []byte("a")},
		fixture{name: "b.txt", body: []byte("b")},
		fixture{name: "c.bin", body: zeros},
	)
	d := newDisk(t)
	_, err := archive.Extract(t.Context(), d, "", bytes.NewReader(b), int64(len(b)), archive.ExtractOptions{Limits: archive.Limits{MaxRatio: 1 << 20, MaxTotal: 1 << 20}})
	wantRejected(t, err, archive.ErrLimit)
	if keys := keysOn(t, d); len(keys) != 0 {
		t.Fatalf("left %v", keys)
	}
}

func TestCanceled(t *testing.T) {
	b := zipOf(t, fixture{name: "a", body: []byte("a")})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := archive.List(ctx, bytes.NewReader(b), int64(len(b)), archive.Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// Inspect plugged into an upload policy's Inspect hook, through the
// public API: a stored upload is walked from the disk in ranged reads.
func TestInspect(t *testing.T) {
	elf := append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 64)...)
	inspector := upload.Inspectors(archive.Inspect(archive.Limits{}, archive.DenyExecutables))
	cases := map[string]struct {
		body []byte
		want error
	}{
		"clean zip":          {zipOf(t, fixture{name: "a.txt", body: []byte("a")}), nil},
		"not an archive":     {[]byte("%PDF-1.7 not an archive"), nil},
		"bomb":               {zipOf(t, fixture{name: "z", body: zeros}), archive.ErrLimit},
		"zip slip":           {tarOf(t, false, fixture{name: "../x", body: []byte("x")}), archive.ErrUnsafeEntry},
		"program by content": {zipOf(t, fixture{name: "readme.txt", body: elf}), filesystem.ErrRejected},
		"program by name":    {zipOf(t, fixture{name: "docs/Setup.EXE", body: []byte("text")}), filesystem.ErrRejected},
		"nested program":     {zipOf(t, fixture{name: "in.tar", body: tarOf(t, false, fixture{name: "run.sh", body: []byte("echo")})}), filesystem.ErrRejected},
		"corrupt zip":        {append([]byte("PK\x03\x04"), bytes.Repeat([]byte{1}, 100)...), archive.ErrFormat},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDisk(t)
			if _, err := d.WriteBytes(t.Context(), "u/file", c.body, filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			err := inspector(t.Context(), d, upload.Result{Key: "u/file"})
			if c.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, c.want) || filesystem.CodeOf(err) != filesystem.CodeRejected {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// ReaderAt returns exactly the object's bytes across block boundaries.
func TestReaderAt(t *testing.T) {
	d := newDisk(t)
	body := make([]byte, 2<<20+12345)
	for i := range body {
		body[i] = byte(i * 7)
	}
	if _, err := d.WriteBytes(t.Context(), "big", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ra, err := archive.OpenReaderAt(t.Context(), d, "big")
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 1<<20 - 3, 2<<20 - 1, int64(len(body)) - 5} {
		p := make([]byte, 10)
		n, err := ra.ReadAt(p, off)
		want := body[off:min(off+10, int64(len(body)))]
		if !bytes.Equal(p[:n], want) || (n < 10) != errors.Is(err, io.EOF) {
			t.Errorf("off %d: %d bytes, %v", off, n, err)
		}
	}
	got, err := io.ReadAll(io.NewSectionReader(ra, 0, ra.Size()))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("section read: %d bytes, %v", len(got), err)
	}
}

// pdfHeader makes a polyglot: archive/zip opens a zip with bytes in front
// of it, so a file can be a PDF to one reader and a zip to another.
var pdfHeader = []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj << >> endobj\n")

func gzipOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPolyglots(t *testing.T) {
	elf := append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 64)...)
	bomb := append(bytes.Clone(pdfHeader), zipOf(t, fixture{name: "z", body: zeros})...)
	_, err := list(t, bomb, archive.Limits{})
	wantRejected(t, err, archive.ErrLimit)

	inspector := archive.Inspect(archive.Limits{}, archive.DenyExecutables)
	for name, c := range map[string]struct {
		body []byte
		want error
	}{
		"prefixed bomb":              {bomb, archive.ErrLimit},
		"prefixed program":           {append(bytes.Clone(pdfHeader), zipOf(t, fixture{name: "readme.txt", body: elf})...), filesystem.ErrRejected},
		"prefixed slip":              {append(bytes.Clone(pdfHeader), zipOf(t, fixture{name: "../x", body: []byte("x")})...), archive.ErrUnsafeEntry},
		"polyglot inside an archive": {zipOf(t, fixture{name: "doc.pdf", body: append(bytes.Clone(pdfHeader), zipOf(t, fixture{name: "a", body: []byte("a")})...)}), archive.ErrUnsafeEntry},
		"gzip of a program":          {gzipOf(t, elf), filesystem.ErrRejected},
		"gzip of a program in a zip": {zipOf(t, fixture{name: "tool.gz", body: gzipOf(t, elf)}), filesystem.ErrRejected},
		"plain pdf":                  {append(bytes.Clone(pdfHeader), "%%EOF"...), nil},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDisk(t)
			if _, err := d.WriteBytes(t.Context(), "u", c.body, filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			err := inspector(t.Context(), d, upload.Result{Key: "u"})
			if c.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, c.want) || filesystem.CodeOf(err) != filesystem.CodeRejected {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// noConditionalWrite is a memory driver that does not declare
// ConditionalWrite, to exercise Extract's Stat-before-write path.
type noConditionalWrite struct{ *memory.Driver }

func (n noConditionalWrite) Capabilities() filesystem.Capabilities {
	c := n.Driver.Capabilities()
	c.ConditionalWrite = false
	return c
}

func TestExtractNeverOverwritesByDefault(t *testing.T) {
	good := zipOf(t, fixture{name: "new.txt", body: []byte("new")}, fixture{name: "config.json", body: []byte("attacker")})
	for name, drv := range map[string]filesystem.Driver{"atomic": memory.New(), "stat first": noConditionalWrite{memory.New()}} {
		t.Run(name, func(t *testing.T) {
			d, err := filesystem.NewDisk("x", drv)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.WriteBytes(t.Context(), "app/config.json", []byte("original"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err = archive.Extract(t.Context(), d, "app", bytes.NewReader(good), int64(len(good)), archive.ExtractOptions{})
			if filesystem.CodeOf(err) != filesystem.CodeExists {
				t.Fatalf("got %v, want CodeExists", err)
			}
			if b, _ := d.ReadAll(t.Context(), "app/config.json"); string(b) != "original" {
				t.Fatalf("existing file now %q", b)
			}
			if keys := keysOn(t, d); len(keys) != 1 {
				t.Fatalf("cleanup left %v", keys)
			}
		})
	}
}

// With Overwrite, existing files are replaced, and a failure later
// deletes only what Extract created.
func TestExtractOverwrite(t *testing.T) {
	d := newDisk(t)
	if _, err := d.WriteBytes(t.Context(), "app/a.txt", []byte("original"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	ok := zipOf(t, fixture{name: "a.txt", body: []byte("replaced")})
	if _, err := archive.Extract(t.Context(), d, "app", bytes.NewReader(ok), int64(len(ok)), archive.ExtractOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.ReadAll(t.Context(), "app/a.txt"); string(b) != "replaced" {
		t.Fatalf("got %q", b)
	}
	bad := zipOf(t, fixture{name: "a.txt", body: []byte("again")}, fixture{name: "b.txt", body: []byte("b")}, fixture{name: "../c", body: []byte("c")})
	_, err := archive.Extract(t.Context(), d, "app", bytes.NewReader(bad), int64(len(bad)), archive.ExtractOptions{Overwrite: true})
	wantRejected(t, err, archive.ErrUnsafeEntry)
	if keys := keysOn(t, d); len(keys) != 1 || keys[0] != "app/a.txt" {
		t.Fatalf("keys %v: the pre-existing file must stay, the created one go", keys)
	}
}

func TestDenyExecutables(t *testing.T) {
	for _, name := range []string{"setup.exe. ", "SETUP.EXE...", "x.pif", "fix.reg", "console.msc", "disk.iso", "run.HTA", "a.jar", "s.vbs", "p.ps1", "screen.scr"} {
		if err := archive.DenyExecutables(archive.Entry{Name: name}); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%q passed", name)
		}
	}
	if err := archive.DenyExecutables(archive.Entry{Name: "notes.txt"}); err != nil {
		t.Errorf("notes.txt: %v", err)
	}
	if err := archive.DenyExecutables(archive.Entry{Name: "notes.txt", Executable: true}); err == nil {
		t.Error("a program by content passed")
	}
	withPython := archive.DenyExecutablesWith(append(archive.DefaultExecutableExtensions(), ".py"))
	if err := withPython(archive.Entry{Name: "tool.py"}); err == nil {
		t.Error("an added extension passed")
	}
	if err := archive.DenyExecutables(archive.Entry{Name: "tool.py"}); err != nil {
		t.Errorf("the default list changed: %v", err)
	}
}

func TestCaseCollisions(t *testing.T) {
	for name, b := range map[string][]byte{
		"zip": zipOf(t, fixture{name: "Report.txt", body: []byte("a")}, fixture{name: "report.TXT", body: []byte("b")}),
		"tar": tarOf(t, false, fixture{name: "dir/A", body: []byte("a")}, fixture{name: "DIR/a", body: []byte("b")}),
	} {
		_, err := list(t, b, archive.Limits{})
		if !errors.Is(err, archive.ErrUnsafeEntry) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// git archive writes a pax global header first; it is metadata and passes.
func TestPaxGlobalHeader(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "0123abc"}}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "repo/a.txt", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("a"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := list(t, buf.Bytes(), archive.Limits{})
	if err != nil || len(got) != 1 || got[0].Name != "repo/a.txt" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestMaxNested(t *testing.T) {
	inner := zipOf(t, fixture{name: "a.bin", body: bytes.Repeat([]byte{7, 1, 9}, 4000), stored: true})
	outer := zipOf(t, fixture{name: "inner.zip", body: inner, stored: true})
	_, err := list(t, outer, archive.Limits{MaxNested: int64(len(inner)) - 1})
	wantRejected(t, err, archive.ErrLimit)
	if _, err := list(t, outer, archive.Limits{MaxNested: int64(len(inner))}); err != nil {
		t.Fatal(err)
	}
}
