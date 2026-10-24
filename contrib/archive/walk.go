package archive

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/ubgo/filesystem"
)

// headSize is how much of each entry and archive the walk looks at to
// detect formats and programs: a tar header block, which holds the tar
// magic at offset 257.
const headSize = 512

// tarMagicOffset is where a POSIX or GNU tar header keeps "ustar".
const tarMagicOffset = 257

// zipEndSize and zipMaxComment bound where a zip's end-of-central-
// directory record can sit: in the last 22+65535 bytes, as archive/zip
// searches for it. tailSize is the window a streamed entry keeps to find
// one.
const (
	zipEndSize    = 22
	zipMaxComment = 1<<16 - 1
	tailSize      = zipEndSize + zipMaxComment
)

// gzipContentName names the content of a gzip file whose header records
// no name and whose own name does not end in ".gz".
const gzipContentName = "content"

// Magic numbers the walk detects formats and programs by.
var (
	zipMagic      = []byte("PK\x03\x04")
	zipEmptyMagic = []byte("PK\x05\x06")
	gzipMagic     = []byte{0x1f, 0x8b}
	tarMagic      = []byte("ustar")
	// programMagics are the first bytes of native executables and
	// scripts: ELF, Windows PE ("MZ"), Mach-O 32/64-bit in both byte
	// orders, fat Mach-O, and "#!".
	programMagics = [][]byte{
		[]byte("\x7fELF"), []byte("MZ"),
		{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
		{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
		{0xca, 0xfe, 0xba, 0xbe}, []byte("#!"),
	}
	// driveLetter matches a Windows absolute name ("C:/x", "c:\x").
	driveLetter = regexp.MustCompile(`^[A-Za-z]:`)
)

// VisitFunc receives each entry of a Walk. content is nil for a
// directory; for a file it yields the entry's bytes, and the walk reads
// whatever the function leaves unread so every byte is counted and
// checksummed. Returning an error stops the walk with that error.
//
// content is valid only until the function returns: the walk moves on to
// the next entry afterwards, so a function that keeps the bytes copies
// them first.
type VisitFunc func(ctx context.Context, e Entry, content io.Reader) error

// walker holds the counters one walk shares across every depth.
type walker struct {
	limits Limits
	// base is the size of the archive the walk started from: MaxRatio is
	// measured against it at every depth.
	base    int64
	entries int
	total   int64
	// seen holds every entry path, case-folded, to refuse duplicates.
	seen  map[string]bool
	visit VisitFunc
}

// Walk walks the archive in r (size bytes) and calls fn for every entry,
// nested archives' entries included (Entry.Depth above 1), in archive
// order. It is the primitive under List and Extract, exported so an
// application can send entries somewhere else: one records row per entry
// with its quota and scan, each entry through an upload.Policy, or a
// filter that drops __MACOSX and .DS_Store before storing.
//
// Every safety rule of the package runs inside the walk and cannot be
// turned off by fn: the limits (entries, total bytes, ratio, depth,
// nested size) stop it at the first byte past one, unsafe names and
// duplicates are refused before fn sees the entry, and a zip hidden
// behind other content is refused after it. So fn may already have seen
// earlier entries when Walk fails: a function that stores entries cleans
// up on error, as Extract does. Input that is not an archive is
// ErrFormat.
func Walk(ctx context.Context, r io.ReaderAt, size int64, limits Limits, fn VisitFunc) error {
	return walkArchive(ctx, r, size, limits, fn)
}

// walkArchive walks the archive in r (size bytes) with visit, refusing
// input that is not an archive.
func walkArchive(ctx context.Context, r io.ReaderAt, size int64, limits Limits, visit VisitFunc) error {
	w := &walker{limits: limits.withDefaults(), base: size, seen: map[string]bool{}, visit: visit}
	ok, err := w.walk(ctx, r, size, "", 1)
	if err != nil {
		return err
	}
	if !ok {
		return rejected(ErrFormat, "unrecognised content")
	}
	return nil
}

// walk walks one archive level and reports whether r held an archive at
// all; false with a nil error means it is some other file. Every format
// r parses as is walked: a tar or gzip by its magic at the start, and a
// zip by its end-of-central-directory record wherever it sits, as
// archive/zip finds it, so a zip behind a PDF header (a polyglot) is
// walked like any other.
func (w *walker) walk(ctx context.Context, r io.ReaderAt, size int64, in string, depth int) (bool, error) {
	head := make([]byte, headSize)
	n, err := r.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, classify(err)
	}
	head = head[:n]
	found := false
	switch {
	case bytes.HasPrefix(head, gzipMagic):
		found = true
		if err := w.gzip(ctx, r, size, in, depth); err != nil {
			return true, err
		}
	case isTar(head):
		found = true
		if err := w.tar(ctx, tar.NewReader(meter{ctx: ctx, w: w, r: io.NewSectionReader(r, 0, size)}), in, depth, FormatTar); err != nil {
			return true, err
		}
	}
	zr, err := zip.NewReader(r, size)
	switch {
	// ErrInsecurePath comes with a usable reader; the names are checked
	// in zip, more strictly.
	case err == nil, errors.Is(err, zip.ErrInsecurePath):
		return true, w.zip(ctx, zr, in, depth)
	case bytes.HasPrefix(head, zipMagic), bytes.HasPrefix(head, zipEmptyMagic):
		return true, classify(err)
	}
	return found, nil
}

// gzip walks a gzip stream: a tar inside it is walked as a tar.gz, any
// other content as a one-entry archive, so a program or an archive
// compressed alone is checked like one inside a zip.
func (w *walker) gzip(ctx context.Context, r io.ReaderAt, size int64, in string, depth int) error {
	gz, err := gzip.NewReader(io.NewSectionReader(r, 0, size))
	if err != nil {
		return classify(err)
	}
	// The stream is metered as it decompresses, tar headers and padding
	// included, so nothing it expands to goes uncounted.
	br := bufio.NewReaderSize(meter{ctx: ctx, w: w, r: gz}, headSize)
	peek, err := br.Peek(headSize)
	if err != nil && !isShort(err) {
		return classify(err)
	}
	if isTar(peek) {
		return w.tar(ctx, tar.NewReader(br), in, depth, FormatTarGzip)
	}
	name := gz.Name
	if name == "" {
		name = gzipContentName
		if base := path.Base(in); in != "" && strings.HasSuffix(base, ".gz") {
			name = strings.TrimSuffix(base, ".gz")
		}
	}
	e := Entry{In: in, Depth: depth, Format: FormatGzip, ModTime: gz.ModTime}
	if err := w.entry(&e, name); err != nil {
		return err
	}
	return w.file(ctx, e, br)
}

// zip walks a zip archive. Declared sizes are checked against the limits
// before anything is decompressed; archive/zip then holds every entry to
// its declared size, and the meter counts the bytes as they come.
func (w *walker) zip(ctx context.Context, zr *zip.Reader, in string, depth int) error {
	if w.entries+len(zr.File) > w.limits.MaxEntries {
		return rejected(ErrLimit, "more than %d entries", w.limits.MaxEntries)
	}
	declared := w.total
	for _, f := range zr.File {
		declared += int64(min(f.UncompressedSize64, uint64(w.limits.MaxTotal)+1))
		if err := w.check(declared); err != nil {
			return err
		}
	}
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := Entry{In: in, Depth: depth, Format: FormatZip, ModTime: f.Modified, Size: int64(f.UncompressedSize64)}
		mode := f.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return rejected(ErrUnsafeEntry, "symlink %q", f.Name)
		case mode&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeIrregular) != 0:
			return rejected(ErrUnsafeEntry, "special file %q", f.Name)
		}
		e.IsDir = mode.IsDir() || strings.HasSuffix(f.Name, "/")
		if err := w.entry(&e, f.Name); err != nil {
			return err
		}
		if e.IsDir {
			if e.Name != "" {
				if err := w.visit(ctx, e, nil); err != nil {
					return err
				}
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return classify(err)
		}
		err = w.file(ctx, e, meter{ctx: ctx, w: w, r: rc})
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// tar walks a tar stream already wrapped in the meter.
func (w *walker) tar(ctx context.Context, tr *tar.Reader, in string, depth int, format Format) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		// ErrInsecurePath comes with the header; the name is checked
		// below, more strictly.
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return classify(err)
		}
		e := Entry{In: in, Depth: depth, Format: format, ModTime: hdr.ModTime, Size: hdr.Size}
		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			e.IsDir = true
			e.Size = 0
		case tar.TypeXGlobalHeader:
			// PAX global headers (git archive writes one) carry metadata
			// for the entries after them, never content to extract. They
			// count as an entry; their bytes were already metered.
			w.entries++
			if w.entries > w.limits.MaxEntries {
				return rejected(ErrLimit, "more than %d entries", w.limits.MaxEntries)
			}
			continue
		case tar.TypeSymlink:
			return rejected(ErrUnsafeEntry, "symlink %q", hdr.Name)
		case tar.TypeLink:
			return rejected(ErrUnsafeEntry, "hard link %q", hdr.Name)
		default:
			return rejected(ErrUnsafeEntry, "entry %q of tar type %q", hdr.Name, hdr.Typeflag)
		}
		if err := w.entry(&e, hdr.Name); err != nil {
			return err
		}
		if err := w.check(w.total + e.Size); err != nil {
			return err
		}
		if e.IsDir {
			if e.Name != "" {
				if err := w.visit(ctx, e, nil); err != nil {
					return err
				}
			}
			continue
		}
		if err := w.file(ctx, e, tr); err != nil {
			return err
		}
	}
}

// entry counts one entry and sets its cleaned name, refusing absolute,
// traversing and duplicate names. Names that differ only in letter case
// are duplicates on every disk, because the same archive extracted onto
// a case-insensitive filesystem would let the second overwrite the
// first. A directory that cleans to the root ("./") keeps an empty Name
// and is not visited.
func (w *walker) entry(e *Entry, raw string) error {
	w.entries++
	if w.entries > w.limits.MaxEntries {
		return rejected(ErrLimit, "more than %d entries", w.limits.MaxEntries)
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\`) || driveLetter.MatchString(raw) {
		return rejected(ErrUnsafeEntry, "absolute name %q", raw)
	}
	var (
		name string
		err  error
	)
	if e.IsDir {
		name, err = filesystem.CleanDir(raw)
	} else {
		name, err = filesystem.CleanKey(raw)
	}
	if err != nil {
		return rejected(ErrUnsafeEntry, "name %q: %v", raw, err)
	}
	e.Name = name
	if name == "" {
		return nil
	}
	folded := strings.ToLower(e.Path())
	if w.seen[folded] {
		return rejected(ErrUnsafeEntry, "duplicate name %q", raw)
	}
	w.seen[folded] = true
	return nil
}

// file visits one file entry. A file that starts like an archive is read
// whole (up to MaxNested bytes), walked one level deeper, then visited
// from memory. Any other file streams to visit while its last bytes are
// kept, and is refused afterwards if they hold a zip directory: a zip
// hidden behind other content would otherwise pass unwalked.
func (w *walker) file(ctx context.Context, e Entry, content io.Reader) error {
	br := bufio.NewReaderSize(content, headSize)
	head, err := br.Peek(headSize)
	if err != nil && !isShort(err) {
		return classify(err)
	}
	e.Executable = isProgram(head)
	if mayBeArchive(head) {
		b, err := io.ReadAll(io.LimitReader(br, w.limits.MaxNested+1))
		if err != nil {
			return classify(err)
		}
		if int64(len(b)) > w.limits.MaxNested {
			return rejected(ErrLimit, "nested archive %q larger than %d bytes", e.Path(), w.limits.MaxNested)
		}
		isArchive, err := w.nested(ctx, b, e.Path(), e.Depth+1)
		if err != nil {
			return err
		}
		e.Archive = isArchive
		return w.visit(ctx, e, bytes.NewReader(b))
	}
	t := &tail{r: br}
	if err := w.visit(ctx, e, t); err != nil {
		return err
	}
	// Whatever visit left unread is still decompressed, counted and
	// checksummed, so a walk that succeeds has read every byte.
	if _, err := io.Copy(io.Discard, t); err != nil {
		return classify(err)
	}
	if t.endsLikeZip() {
		return rejected(ErrUnsafeEntry, "%q hides a zip archive after other content", e.Path())
	}
	return nil
}

// nested walks a file that starts like an archive, refusing it when it is
// one level too deep. b always starts with an archive magic (file calls
// nested only then), so a file too deep is refused without parsing it.
func (w *walker) nested(ctx context.Context, b []byte, in string, depth int) (bool, error) {
	r := bytes.NewReader(b)
	if depth > w.limits.MaxDepth {
		return false, rejected(ErrLimit, "archive %q nested deeper than %d levels", in, w.limits.MaxDepth)
	}
	return w.walk(ctx, r, int64(len(b)), in, depth)
}

// check refuses a decompressed total past MaxTotal or, beyond
// RatioGrace, past MaxRatio times the archive's size.
func (w *walker) check(total int64) error {
	if total > w.limits.MaxTotal {
		return rejected(ErrLimit, "decompresses to more than %d bytes", w.limits.MaxTotal)
	}
	if total > RatioGrace && total/w.limits.MaxRatio > w.base {
		return rejected(ErrLimit, "decompresses to more than %d times its size", w.limits.MaxRatio)
	}
	return nil
}

// meter counts every byte read through it against the walk's limits and
// stops the read at the first byte past one.
type meter struct {
	ctx context.Context
	w   *walker
	r   io.Reader
}

// Read implements io.Reader.
func (m meter) Read(p []byte) (int, error) {
	if err := m.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := m.r.Read(p)
	if n > 0 {
		m.w.total += int64(n)
		if lerr := m.w.check(m.w.total); lerr != nil {
			return n, lerr
		}
	}
	return n, err
}

// tail passes a stream through, keeping its last tailSize bytes.
type tail struct {
	r   io.Reader
	buf []byte
}

// Read implements io.Reader.
func (t *tail) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.buf = append(t.buf, p[:n]...)
	if len(t.buf) > 2*tailSize {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-tailSize:]...)
	}
	return n, err
}

// endsLikeZip reports whether the kept bytes hold a zip
// end-of-central-directory record whose comment fits in what follows it,
// the test archive/zip uses to find one.
func (t *tail) endsLikeZip() bool {
	b := t.buf[max(0, len(t.buf)-tailSize):]
	for i := len(b) - zipEndSize; i >= 0; i-- {
		if bytes.Equal(b[i:i+4], zipEmptyMagic) {
			comment := int(binary.LittleEndian.Uint16(b[i+20:]))
			if i+zipEndSize+comment <= len(b) {
				return true
			}
		}
	}
	return false
}

// classify keeps errors that already carry a code (a limit, a failed read
// of the disk, the context ending) and makes anything else from a reader
// a corrupt-archive ErrFormat.
func classify(err error) error {
	var fe *filesystem.Error
	if errors.As(err, &fe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return rejected(ErrFormat, "%v", err)
}

// isShort reports whether a Peek error only means the input is shorter
// than the peek.
func isShort(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, bufio.ErrBufferFull)
}

// isTar reports whether head is a POSIX or GNU tar header block.
func isTar(head []byte) bool {
	return len(head) >= tarMagicOffset+len(tarMagic) && bytes.Equal(head[tarMagicOffset:tarMagicOffset+len(tarMagic)], tarMagic)
}

// mayBeArchive reports whether head starts like a zip, gzip or tar.
func mayBeArchive(head []byte) bool {
	return bytes.HasPrefix(head, zipMagic) || bytes.HasPrefix(head, zipEmptyMagic) || bytes.HasPrefix(head, gzipMagic) || isTar(head)
}

// isArchiveContent reports whether r parses as an archive the walk
// enters: a gzip or tar by its magic, or a zip wherever its directory
// sits.
func isArchiveContent(r io.ReaderAt, size int64) bool {
	head := make([]byte, headSize)
	n, _ := r.ReadAt(head, 0)
	if mayBeArchive(head[:n]) {
		return true
	}
	_, err := zip.NewReader(r, size)
	return err == nil || errors.Is(err, zip.ErrInsecurePath)
}

// isProgram reports whether head starts like an executable.
func isProgram(head []byte) bool {
	for _, m := range programMagics {
		if bytes.HasPrefix(head, m) {
			return true
		}
	}
	return false
}
