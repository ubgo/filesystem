// Package ocr reads the text in images with the Tesseract OCR engine's
// tesseract program: the text of a photographed receipt, a screenshot or
// a scanned page, for a search index. Tool.Recognize reads one image; Tool
// is also a contrib/pdf Recognizer, so pdf.WithOCR reads scanned PDFs
// page by page; Plugin (plugin.go) is a records plugin that stores the
// text of every image file beside it. It is a module of its own because
// it runs an external program; the core never does.
//
// An image is untrusted input to a C++ program, so every call is
// bounded: at most MaxInput bytes are copied to a work file, only PNG,
// JPEG and GIF files of at most MaxPixels pixels reach the program (a
// text file is never passed: tesseract reads one as a list of image paths
// to open), each run is killed after Timeout, and its text is capped at
// MaxText bytes. Languages are checked against a strict pattern before
// they become an argument. The program runs without a shell.
package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // the formats Recognize accepts register their decoders
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
)

// DefaultTesseract is the program's name, looked up on PATH when
// Tool.Tesseract is empty.
const DefaultTesseract = "tesseract"

// DefaultLanguage is the language read when Tool.Languages is empty:
// tesseract's English model, the one every installation has.
const DefaultLanguage = "eng"

// Defaults used when a Tool leaves a field zero.
const (
	// DefaultTimeout bounds one recognition. A dense A4 page at 300 dpi
	// takes a few seconds.
	DefaultTimeout = time.Minute
	// DefaultMaxInput bounds the bytes of an image copied to the work
	// file.
	DefaultMaxInput int64 = 50 << 20
	// DefaultMaxPixels bounds an image's width times height: an A4 page
	// at 600 dpi is 35 million.
	DefaultMaxPixels = 50_000_000
	// DefaultMaxText bounds the text of one image in bytes; a dense page
	// is a few KiB.
	DefaultMaxText int64 = 1 << 20
)

// The content types Recognize reads.
const (
	PNG  = "image/png"
	JPEG = "image/jpeg"
	GIF  = "image/gif"
)

// ContentTypes returns the content types Recognize reads.
func ContentTypes() []string { return []string{PNG, JPEG, GIF} }

// acceptedFormat reports whether image.DecodeConfig's format name is one
// of ContentTypes.
func acceptedFormat(name string) bool {
	switch name {
	case "png", "jpeg", "gif":
		return true
	}
	return false
}

// languagePattern is what a language must look like: a tesseract model
// name ("eng", "chi_sim", "deu_latf"). Nothing that starts with "-" (an
// option) or holds a path separator (another file) passes.
var languagePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)

// maxStderr bounds what is kept of the program's messages for logs.
const maxStderr = 4 << 10

// stdoutBase is tesseract's output base meaning standard output.
const stdoutBase = "stdout"

// ErrNoProgram is in the chain of the error of a run whose program is not
// installed (or not at the configured path), classified CodeUnavailable:
// a deployment problem, not the client's file.
var ErrNoProgram = errors.New("ocr: program not found")

// ProgramError is the cause of a run that failed on its input, inside a
// CodeRejected error. Its Error text is safe to show a client; Detail is
// the program's own output, which names server paths, for logs only.
type ProgramError struct {
	Program string
	Detail  string
}

func (e *ProgramError) Error() string { return "ocr: " + e.Program + " could not read the image" }

// Tool runs tesseract. The zero value reads English with the defaults.
type Tool struct {
	// Tesseract is the program's path or name; empty means
	// DefaultTesseract, looked up on PATH.
	Tesseract string
	// Languages are the models to read with, in order of preference;
	// empty means DefaultLanguage. Each must be installed (tesseract
	// --list-langs) and match [A-Za-z][A-Za-z0-9_]{0,31}; another is
	// CodeInvalidArgument before anything runs.
	Languages []string
	// Timeout bounds each run; 0 means DefaultTimeout. A run cut short is
	// CodeUnavailable.
	Timeout time.Duration
	// MaxInput bounds an image's bytes; 0 means DefaultMaxInput. A longer
	// one is CodeTooLarge.
	MaxInput int64
	// MaxPixels bounds an image's width times height; 0 means
	// DefaultMaxPixels. A larger one is CodeTooLarge.
	MaxPixels int
	// MaxText bounds the text of one image in bytes; 0 means
	// DefaultMaxText. More is CodeTooLarge.
	MaxText int64
	// TempDir is where work files go; empty means os.TempDir(). Work files
	// are removed before each call returns.
	TempDir string
	// Env adds variables to the program's environment, as "KEY=value"
	// (TESSDATA_PREFIX, a font path). The program otherwise sees only
	// PATH, HOME, TMPDIR, TMP, LANG and LC_ALL of this process, so the
	// application's secrets never reach a parser of untrusted files.
	Env []string
}

// Recognize returns the text in the PNG, JPEG or GIF image r holds, as
// UTF-8 with surrounding blank space trimmed ("" when it has none). It
// implements contrib/pdf's Recognizer. Another format is CodeRejected,
// as is an image tesseract cannot read.
func (t Tool) Recognize(ctx context.Context, r io.Reader) (string, error) {
	langs, err := t.languages()
	if err != nil {
		return "", err
	}
	prog := t.Tesseract
	if prog == "" {
		prog = DefaultTesseract
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		return "", filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%w: %s (install Tesseract or set Tool.Tesseract): %w", ErrNoProgram, prog, err))
	}
	f, err := os.CreateTemp(t.TempDir, "ocr-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	err = t.copyIn(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if err := t.checkImage(f.Name()); err != nil {
		return "", err
	}
	return t.run(ctx, path, f.Name(), langs)
}

// copyIn copies at most MaxInput bytes of r to f.
func (t Tool) copyIn(f *os.File, r io.Reader) error {
	limit := t.maxInput()
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return filesystem.Errorf(filesystem.CodeTooLarge, "ocr: larger than %d bytes", limit)
	}
	return nil
}

// checkImage refuses a file that is not a PNG, JPEG or GIF image by its
// header, or has more than MaxPixels pixels.
func (t Tool) checkImage(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil || !acceptedFormat(format) {
		return filesystem.Errorf(filesystem.CodeRejected, "ocr: not a PNG, JPEG or GIF image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > t.maxPixels()/cfg.Height {
		return filesystem.Errorf(filesystem.CodeTooLarge, "ocr: the image has more than %d pixels", t.maxPixels())
	}
	return nil
}

// run executes tesseract on path, writing the text to standard output.
func (t Tool) run(ctx context.Context, path, image string, langs string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, path, image, stdoutBase, "-l", langs)
	cmd.Env = programEnv(t.Env)
	stdout := &capped{max: t.maxText()}
	stderr := &capped{max: maxStderr, truncate: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	switch {
	case stdout.over:
		return "", filesystem.Errorf(filesystem.CodeTooLarge, "ocr: more than %d bytes of text", t.maxText())
	case ctx.Err() != nil:
		return "", filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("ocr: %s did not finish within %s: %w", filepath.Base(path), t.timeout(), ctx.Err()))
	case err != nil:
		return "", filesystem.NewError(filesystem.CodeRejected, &ProgramError{Program: filepath.Base(path), Detail: strings.TrimSpace(stderr.buf.String())})
	}
	return strings.TrimSpace(strings.ToValidUTF8(stdout.buf.String(), string(utf8.RuneError))), nil
}

// languages returns Languages joined as tesseract's -l argument, or
// CodeInvalidArgument for one that is not a model name.
func (t Tool) languages() (string, error) {
	if len(t.Languages) == 0 {
		return DefaultLanguage, nil
	}
	for _, l := range t.Languages {
		if !languagePattern.MatchString(l) {
			return "", filesystem.Errorf(filesystem.CodeInvalidArgument, "ocr: %q is not a language model name", l)
		}
	}
	return strings.Join(t.Languages, "+"), nil
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
var errOutputCap = errors.New("ocr: output cap reached")

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

func (t Tool) maxPixels() int {
	if t.MaxPixels <= 0 {
		return DefaultMaxPixels
	}
	return t.MaxPixels
}

func (t Tool) maxText() int64 {
	if t.MaxText <= 0 {
		return DefaultMaxText
	}
	return t.MaxText
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
