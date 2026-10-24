// Package pdf reads what applications need from uploaded PDFs with
// Poppler's programs: pdfinfo for the page count, the first page's size
// and the document's title, author, creation date and encryption,
// pdftoppm for a preview image of the first page, and pdftotext for the
// text (text.go), with OCR of the pages through a Recognizer when the
// PDF has no text layer. It is a
// module of its own because it runs external programs; the core never
// does.
//
// Everything goes through a Tool, whose zero value is usable: the programs
// are looked up on PATH as "pdfinfo", "pdftoppm" and "pdftotext", and every limit has a
// default. A PDF is untrusted input to a large C library, so inputs are
// bounded three ways: at most MaxInput bytes are copied to a work file
// (CodeTooLarge beyond), each program run is killed after Timeout, and its
// output is capped. Programs run without a shell, and a preview is scaled
// to fit MaxSide pixels whatever page size the file claims, so a page
// declared kilometres wide cannot exhaust memory.
//
// On top of the Tool: Inspect returns an upload.Inspector that refuses what is
// not a readable PDF or has too many pages; Plugin (plugin.go) is a
// records plugin that stores the page count and document information,
// and optionally a preview and the text, with every PDF file (and every
// file a Converter turns into one).
package pdf

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// Default program names, looked up on PATH when Tool leaves them empty.
const (
	DefaultPDFInfo  = "pdfinfo"
	DefaultPDFToPPM = "pdftoppm"
)

// Default limits, used when Tool leaves a field zero.
const (
	// DefaultTimeout bounds one program run.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxInput bounds the bytes copied to the work file.
	DefaultMaxInput int64 = 200 << 20
	// DefaultMaxPreview bounds a preview's encoded size.
	DefaultMaxPreview int64 = 10 << 20
	// DefaultMaxSide is the longest side of a preview in pixels.
	DefaultMaxSide = 1024
)

// ContentType is the type the package acts on.
const ContentType = "application/pdf"

// PreviewContentType is the type of a preview: pdftoppm's JPEG.
const PreviewContentType = "image/jpeg"

// maxInfoOutput bounds pdfinfo's report; a real one is under 2 KiB.
const maxInfoOutput = 64 << 10

// maxStderr bounds what is kept of a program's error output for logs.
const maxStderr = 4 << 10

// ErrNoProgram is in the chain of the error of a run whose program is not
// installed (or not at the configured path), classified CodeUnavailable:
// a deployment problem, not the client's file.
var ErrNoProgram = errors.New("pdf: program not found")

// Tool runs pdfinfo and pdftoppm. The zero value uses the defaults.
type Tool struct {
	// PDFInfo, PDFToPPM and PDFToText are the programs' paths or names;
	// empty means DefaultPDFInfo, DefaultPDFToPPM and DefaultPDFToText,
	// looked up on PATH.
	PDFInfo   string
	PDFToPPM  string
	PDFToText string
	// Timeout bounds each program run; 0 means DefaultTimeout. A run cut
	// short is CodeUnavailable.
	Timeout time.Duration
	// MaxInput bounds the bytes of a PDF copied to the work file; 0 means
	// DefaultMaxInput. A longer input is CodeTooLarge.
	MaxInput int64
	// MaxPreview bounds a preview's encoded size; 0 means
	// DefaultMaxPreview.
	MaxPreview int64
	// MaxSide is a preview's longest side in pixels; 0 means
	// DefaultMaxSide.
	MaxSide int
	// MaxText bounds extracted text in bytes; 0 means DefaultMaxText.
	// Longer text is cut there and reported as Extract.Truncated.
	MaxText int64
	// MaxTextPages bounds the pages pdftotext reads; 0 means
	// DefaultMaxTextPages.
	MaxTextPages int
	// MaxOCRPages bounds the pages rendered for a Recognizer; 0 means
	// DefaultMaxOCRPages. Each one is a render and a recognition, so this
	// is the bound on the time and cost of reading a scanned PDF.
	MaxOCRPages int
	// OCRSide is the longest side in pixels of a page rendered for a
	// Recognizer; 0 means DefaultOCRSide.
	OCRSide int
	// TempDir is where work files go; empty means os.TempDir(). Work files
	// are removed before each call returns.
	TempDir string
	// Env adds variables to the program's environment, as "KEY=value"
	// (TESSDATA_PREFIX, a font path). The program otherwise sees only
	// PATH, HOME, TMPDIR, TMP, LANG and LC_ALL of this process, so the
	// application's secrets never reach a parser of untrusted files.
	Env []string
}

// Info is what pdfinfo reports about a PDF.
type Info struct {
	// Pages is the page count.
	Pages int
	// Width and Height are the first page's size in points (1/72 inch).
	Width, Height float64
	// Title and Author come from the document information dictionary,
	// written by whoever made the file: untrusted text, cleaned of control
	// characters and cut to MaxInfoChars characters; "" when absent.
	Title, Author string
	// Created is the creation date the file claims, zero when absent or
	// unreadable.
	Created time.Time
	// Encrypted reports a file encrypted with an owner password only
	// (restrictions on printing or copying): it opens, and is read. A
	// file locked with a user password does not open and is CodeRejected.
	Encrypted bool
}

// Probe reports what r holds. A file pdfinfo cannot read (not a PDF,
// damaged, or encrypted with a user password) is CodeRejected.
func (t Tool) Probe(ctx context.Context, r io.Reader) (Info, error) {
	var info Info
	err := t.withWorkFile(r, func(path string) error {
		var err error
		info, err = t.infoPath(ctx, path)
		return err
	})
	return info, err
}

// Preview renders the first page as a JPEG fitting MaxSide pixels.
func (t Tool) Preview(ctx context.Context, r io.Reader) ([]byte, error) {
	var out []byte
	err := t.withWorkFile(r, func(path string) error {
		var err error
		out, err = t.previewPath(ctx, path)
		return err
	})
	return out, err
}

// withWorkFile copies at most MaxInput bytes of r to a work file, runs fn
// on its path and removes it.
func (t Tool) withWorkFile(r io.Reader, fn func(path string) error) error {
	f, err := os.CreateTemp(t.TempDir, "pdf-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	limit := t.maxInput()
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return filesystem.Errorf(filesystem.CodeTooLarge, "pdf: larger than %d bytes", limit)
	}
	return fn(f.Name())
}

// pdfinfo report fields the package reads.
const (
	fieldPages      = "Pages"
	fieldPagePrefix = "Page "
	fieldSizeSuffix = " size"
)

// infoPath runs pdfinfo on path and parses its report.
func (t Tool) infoPath(ctx context.Context, path string) (Info, error) {
	args := []string{"-isodates", "-enc", textEncoding, "-f", "1", "-l", "1", path}
	out, err := t.run(ctx, program(t.PDFInfo, DefaultPDFInfo), args, maxInfoOutput)
	if err != nil {
		return Info{}, err
	}
	return parseInfo(out)
}

// parseInfo reads pdfinfo's "Name: value" lines. A report without a page
// count is CodeRejected.
func parseInfo(out []byte) (Info, error) {
	var info Info
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case name == fieldPages:
			info.Pages, _ = strconv.Atoi(value)
		case isPageSize(name) && info.Width == 0:
			info.Width, info.Height = pageSize(value)
		default:
			parseDocField(&info, name, value)
		}
	}
	if info.Pages < 1 {
		return Info{}, filesystem.Errorf(filesystem.CodeRejected, "pdf: no pages")
	}
	return info, nil
}

// pageSize parses "612 x 792 pts (letter)".
func pageSize(v string) (float64, float64) {
	f := strings.Fields(v)
	if len(f) < 3 || f[1] != "x" {
		return 0, 0
	}
	w, errW := strconv.ParseFloat(f[0], 64)
	h, errH := strconv.ParseFloat(f[2], 64)
	if errW != nil || errH != nil {
		return 0, 0
	}
	return w, h
}

// pdftoppm's output formats.
const (
	formatJPEG = "-jpeg"
	formatPNG  = "-png"
)

// previewPath renders the first page of path as a JPEG preview.
func (t Tool) previewPath(ctx context.Context, path string) ([]byte, error) {
	return t.renderPage(ctx, path, 1, t.maxSide(), formatJPEG, t.maxPreview())
}

// renderPage runs pdftoppm on path for one page, scaled to fit side
// pixels, as one image on standard output (pdftoppm writes there when
// given no output root) of at most maxOut bytes.
func (t Tool) renderPage(ctx context.Context, path string, page, side int, format string, maxOut int64) ([]byte, error) {
	n := strconv.Itoa(page)
	args := []string{"-f", n, "-l", n, "-singlefile", "-scale-to", strconv.Itoa(side), format, path}
	out, err := t.run(ctx, program(t.PDFToPPM, DefaultPDFToPPM), args, maxOut)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "pdf: no page to preview")
	}
	return out, nil
}

// ProgramError is the cause of a run that failed on its input, inside a
// CodeRejected error. Its Error text is safe to show a client (it is what
// fshttp.ClientMessage shows for a rejected upload); Detail is the
// program's own error output, which names server paths, for logs only.
// Program is the program's base name: a configured path would name the
// server's layout to a client.
type ProgramError struct {
	Program string
	Detail  string
}

func (e *ProgramError) Error() string { return "pdf: " + e.Program + " could not read the file" }

// run executes program with args (no shell) under the timeout, returning
// at most maxOut bytes of its standard output; more is CodeTooLarge.
func (t Tool) run(ctx context.Context, program string, args []string, maxOut int64) ([]byte, error) {
	stdout := &capped{max: maxOut}
	if err := t.runTo(ctx, program, args, stdout); err != nil {
		if stdout.over {
			return nil, filesystem.Errorf(filesystem.CodeTooLarge, "pdf: %s wrote more than %d bytes", program, maxOut)
		}
		return nil, err
	}
	return stdout.buf.Bytes(), nil
}

// runTo executes program with args (no shell) under the timeout, its
// standard output going to stdout. A run stopped because stdout reached
// its cap returns errOutputCap with stdout.over set, so a caller decides
// whether a cut output is an error (run) or a truncation (Text).
func (t Tool) runTo(ctx context.Context, program string, args []string, stdout *capped) error {
	path, err := exec.LookPath(program)
	if err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%w: %s (install Poppler or set Tool.PDFInfo, Tool.PDFToPPM and Tool.PDFToText): %w", ErrNoProgram, program, err))
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = programEnv(t.Env)
	stderr := &capped{max: maxStderr, truncate: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	switch {
	case stdout.over:
		return errOutputCap
	case ctx.Err() != nil:
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("pdf: %s did not finish within %s: %w", program, t.timeout(), ctx.Err()))
	case err != nil:
		return filesystem.NewError(filesystem.CodeRejected, &ProgramError{Program: filepath.Base(program), Detail: strings.TrimSpace(stderr.buf.String())})
	}
	return nil
}

// capped is a writer that keeps at most max bytes: past it, it either
// reports over (and fails the program's write, ending it early) or, with
// truncate, silently drops the rest.
type capped struct {
	buf      bytes.Buffer
	max      int64
	over     bool
	truncate bool
}

// errOutputCap stops a program that writes past its cap.
var errOutputCap = errors.New("pdf: output cap reached")

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - int64(c.buf.Len())
	if int64(len(p)) <= room {
		return c.buf.Write(p)
	}
	if room > 0 {
		c.buf.Write(p[:room])
	}
	if c.truncate {
		return len(p), nil
	}
	c.over = true
	return 0, errOutputCap
}

func program(set, def string) string {
	if set == "" {
		return def
	}
	return set
}

func (t Tool) timeout() time.Duration {
	if t.Timeout <= 0 {
		return DefaultTimeout
	}
	return t.Timeout
}

func (t Tool) maxInput() int64 {
	if t.MaxInput <= 0 {
		return DefaultMaxInput
	}
	return t.MaxInput
}

func (t Tool) maxPreview() int64 {
	if t.MaxPreview <= 0 {
		return DefaultMaxPreview
	}
	return t.MaxPreview
}

func (t Tool) maxSide() int {
	if t.MaxSide <= 0 {
		return DefaultMaxSide
	}
	return t.MaxSide
}

// isPageSize reports whether a pdfinfo field is a page size: "Page size"
// for the whole document, "Page    1 size" when a page range is asked for.
func isPageSize(name string) bool {
	return strings.HasPrefix(name, fieldPagePrefix) && strings.HasSuffix(name, fieldSizeSuffix)
}

// passedEnv reports whether a variable of this process's environment
// reaches an external program: only what a program needs to find itself,
// its temporary files and its locale. The application's secrets
// (database URLs, cloud keys) never reach a parser of untrusted files.
func passedEnv(name string) bool {
	switch name {
	case "PATH", "HOME", "TMPDIR", "TMP", "LANG", "LC_ALL":
		return true
	}
	return false
}

// programEnv is the environment of a program run: passedEnv's variables
// of this process, then extra (the Env option), which may add to them or
// override them.
func programEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); passedEnv(name) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}
