// Package office converts office documents (word-processing,
// spreadsheet and presentation files, OOXML and OpenDocument) to PDF with
// LibreOffice's soffice in headless mode, so the rest of the library can
// describe them like PDFs: contrib/pdf's plugin takes a Tool as its
// Converter (pdf.WithConverter) and stores the page count, a preview and
// the text of every converted file. It is a module of its own because it
// runs an external program; the core never does.
//
// An office document is untrusted input to a very large program, so every
// conversion is fenced: at most MaxInput bytes are copied in, each run
// gets a fresh temporary HOME and LibreOffice profile (so runs never share
// state, and run in parallel), the profile turns macros off and stops
// linked content from being fetched, the run and every process it starts
// are killed after Timeout, and the PDF is stopped at MaxOutput bytes
// while it is being written. The program runs without a shell. What this
// package cannot fence is the network: run conversions where the process
// has no outbound access if documents come from strangers.
package office

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// DefaultSOffice is LibreOffice's program, looked up on PATH when
// Tool.SOffice is empty. On macOS it is
// /Applications/LibreOffice.app/Contents/MacOS/soffice.
const DefaultSOffice = "soffice"

// Defaults used when a Tool leaves a field zero.
const (
	// DefaultTimeout bounds one conversion. LibreOffice takes a few
	// seconds to start with a fresh profile, and a long spreadsheet
	// more to lay out.
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxInput bounds the bytes of a document copied in.
	DefaultMaxInput int64 = 100 << 20
	// DefaultMaxOutput bounds the PDF a conversion may write.
	DefaultMaxOutput int64 = 200 << 20
)

// The content types a Tool converts by default.
const (
	DOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	XLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	PPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	ODT  = "application/vnd.oasis.opendocument.text"
	ODS  = "application/vnd.oasis.opendocument.spreadsheet"
	ODP  = "application/vnd.oasis.opendocument.presentation"
)

// DefaultContentTypes returns the types a Tool converts when
// Tool.ContentTypes is empty: OOXML and OpenDocument. The binary formats
// of older versions (doc, xls, ppt) are left out: they are where macro
// documents mostly live, and an application that needs them lists them.
func DefaultContentTypes() []string { return []string{DOCX, XLSX, PPTX, ODT, ODS, ODP} }

// extensions name the input file by type: LibreOffice picks its import
// filter by content, but the extension settles ambiguous files.
var extensions = map[string]string{DOCX: ".docx", XLSX: ".xlsx", PPTX: ".pptx", ODT: ".odt", ODS: ".ods", ODP: ".odp"}

// outputWatchInterval is how often the size of the PDF being written is
// checked against MaxOutput.
const outputWatchInterval = 100 * time.Millisecond

// waitDelay bounds how long a finished or killed run may hold its output
// pipes open through a process it left behind.
const waitDelay = 5 * time.Second

// maxStderr bounds what is kept of the program's messages for logs.
const maxStderr = 4 << 10

// Names inside a run's private directory.
const (
	dirHome     = "home"
	dirProfile  = "profile"
	dirTmp      = "tmp"
	dirIn       = "in"
	dirOut      = "out"
	inputBase   = "document"
	pdfExt      = ".pdf"
	profileFile = "user/registrymodifications.xcu"
)

// hardenedProfile is the LibreOffice configuration every run starts
// from: macros never run (security level "very high" and execution
// disabled), links to other documents and remote content are never
// updated while loading, and untrusted referer links are blocked.
const hardenedProfile = `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry" xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="MacroSecurityLevel" oor:op="fuse"><value>3</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="DisableMacrosExecution" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Security/Scripting"><prop oor:name="BlockUntrustedRefererLinks" oor:op="fuse"><value>true</value></prop></item>
<item oor:path="/org.openoffice.Office.Writer/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>0</value></prop></item>
<item oor:path="/org.openoffice.Office.Calc/Content/Update"><prop oor:name="Link" oor:op="fuse"><value>0</value></prop></item>
<item oor:path="/org.openoffice.Office.Common/Misc"><prop oor:name="FirstRun" oor:op="fuse"><value>false</value></prop></item>
</oor:items>
`

// ErrNoProgram is in the chain of the error of a conversion whose program
// is not installed (or not at the configured path), classified
// CodeUnavailable: a deployment problem, not the client's file.
var ErrNoProgram = errors.New("office: program not found")

// ProgramError is the cause of a conversion that failed on its input,
// inside a CodeRejected error. Its Error text is safe to show a client;
// Detail is the program's own output, which names server paths, for logs
// only.
type ProgramError struct {
	Program string
	Detail  string
}

func (e *ProgramError) Error() string { return "office: " + e.Program + " could not convert the file" }

// Tool converts office documents to PDF. The zero value uses the
// defaults. It implements contrib/pdf's Converter.
type Tool struct {
	// SOffice is the program's path or name; empty means DefaultSOffice,
	// looked up on PATH.
	SOffice string
	// ContentTypes are the types Converts accepts; empty means
	// DefaultContentTypes. A type outside DefaultContentTypes is written
	// to the work file without an extension and left to LibreOffice to
	// recognise.
	ContentTypes []string
	// Timeout bounds each conversion; 0 means DefaultTimeout. A run cut
	// short is CodeUnavailable.
	Timeout time.Duration
	// MaxInput bounds the bytes of a document; 0 means DefaultMaxInput.
	// A longer one is CodeTooLarge.
	MaxInput int64
	// MaxOutput bounds the PDF; 0 means DefaultMaxOutput. A conversion
	// writing more is stopped and is CodeTooLarge.
	MaxOutput int64
	// TempDir is where each run's private directory goes; empty means
	// os.TempDir(). The directory is removed before each call returns.
	TempDir string
	// Env adds variables to LibreOffice's environment, as "KEY=value"
	// (SAL_USE_VCLPLUGIN, a font path). It otherwise sees only PATH,
	// LANG and LC_ALL of this process, and the HOME and temporary
	// directory of its own run, which Env cannot override; the
	// application's secrets never reach a parser of untrusted files.
	Env []string
}

// Converts reports whether t converts contentType (parameters and case
// are ignored).
func (t Tool) Converts(contentType string) bool {
	mt := mediaType(contentType)
	return slices.ContainsFunc(t.contentTypes(), func(ct string) bool { return mediaType(ct) == mt })
}

// ConvertToPDF converts the document r holds, of contentType, and
// returns the PDF, which the caller closes. contentType names the work
// file's extension, so LibreOffice reads an ambiguous file as that type.
// The PDF is an unlinked file: nothing is left on disk whatever the
// caller does. A document LibreOffice cannot convert is CodeRejected.
func (t Tool) ConvertToPDF(ctx context.Context, r io.Reader, contentType string) (io.ReadCloser, error) {
	ext := extensions[mediaType(contentType)]
	prog := t.SOffice
	if prog == "" {
		prog = DefaultSOffice
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%w: %s (install LibreOffice or set Tool.SOffice): %w", ErrNoProgram, prog, err))
	}
	root, err := os.MkdirTemp(t.TempDir, "office-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	for _, d := range []string{dirHome, dirTmp, dirIn, dirOut, filepath.Join(dirProfile, filepath.Dir(profileFile))} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(root, dirProfile, profileFile), []byte(hardenedProfile), 0o600); err != nil {
		return nil, err
	}
	in := filepath.Join(root, dirIn, inputBase+ext)
	if err := t.copyIn(in, r); err != nil {
		return nil, err
	}
	out := filepath.Join(root, dirOut, inputBase+pdfExt)
	if err := t.run(ctx, path, root, in, out); err != nil {
		return nil, err
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeRejected, &ProgramError{Program: filepath.Base(path), Detail: "no PDF was written"})
	}
	// The deferred RemoveAll unlinks the PDF; the open file stays
	// readable until the caller closes it.
	return f, nil
}

// copyIn copies at most MaxInput bytes of r to path.
func (t Tool) copyIn(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	limit := t.maxInput()
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return filesystem.Errorf(filesystem.CodeTooLarge, "office: larger than %d bytes", limit)
	}
	return nil
}

// run runs soffice on in, writing out, with its own HOME, temporary
// directory and profile under root, killed with every process it started
// after the timeout or when out grows past MaxOutput.
func (t Tool) run(ctx context.Context, path, root, in, out string) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	profile := (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(root, dirProfile))}).String()
	args := []string{
		"--headless", "--invisible", "--norestore", "--nologo", "--nodefault", "--nolockcheck", "--nofirststartwizard",
		"-env:UserInstallation=" + profile,
		"--convert-to", "pdf", "--outdir", filepath.Dir(out), in,
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = root
	cmd.Env = runEnv(t.Env, filepath.Join(root, dirHome), filepath.Join(root, dirTmp))
	var msgs capped
	cmd.Stdout, cmd.Stderr = &msgs, &msgs
	cmd.WaitDelay = waitDelay
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	tooLarge := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t.watchOutput(ctx, out, cancel, tooLarge)
	}()
	err := cmd.Wait()
	ctxErr := ctx.Err() // read before cancel, which stops the watcher
	killGroup(cmd)
	cancel()
	<-watchDone
	select {
	case <-tooLarge:
		return filesystem.Errorf(filesystem.CodeTooLarge, "office: the PDF is larger than %d bytes", t.maxOutput())
	default:
	}
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("office: %s did not finish within %s: %w", filepath.Base(path), t.timeout(), ctxErr))
	case err != nil && ctxErr != nil:
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("office: conversion stopped: %w", ctxErr))
	case err != nil:
		return filesystem.NewError(filesystem.CodeRejected, &ProgramError{Program: filepath.Base(path), Detail: strings.TrimSpace(msgs.buf.String())})
	}
	if fi, err := os.Stat(out); err == nil && fi.Size() > t.maxOutput() {
		return filesystem.Errorf(filesystem.CodeTooLarge, "office: the PDF is larger than %d bytes", t.maxOutput())
	}
	return nil
}

// watchOutput stops the run (cancel) and closes tooLarge when out grows
// past MaxOutput, checking until ctx ends.
func (t Tool) watchOutput(ctx context.Context, out string, cancel context.CancelFunc, tooLarge chan<- struct{}) {
	tick := time.NewTicker(outputWatchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if fi, err := os.Stat(out); err == nil && fi.Size() > t.maxOutput() {
				close(tooLarge)
				cancel()
				return
			}
		}
	}
}

// runEnv is the environment of a run: PATH, LANG and LC_ALL of this
// process, then extra (Tool.Env), then HOME and the temporary directory
// of the run itself, last so nothing overrides them and LibreOffice
// writes nothing outside the run's directory. The application's secrets
// (database URLs, cloud keys) never reach it.
func runEnv(extra []string, home, tmp string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); passedEnv(name) {
			env = append(env, kv)
		}
	}
	env = append(env, extra...)
	return append(env, "HOME="+home, "TMPDIR="+tmp, "TMP="+tmp, "TEMP="+tmp)
}

// passedEnv reports whether a variable of this process's environment
// reaches LibreOffice: only what it needs to find itself and its locale.
func passedEnv(name string) bool {
	switch name {
	case "PATH", "LANG", "LC_ALL":
		return true
	}
	return false
}

// capped keeps the first maxStderr bytes written to it and drops the
// rest, so a chatty program cannot grow ProgramError.Detail.
type capped struct{ buf bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxStderr - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// mediaType strips parameters from a content type and lowercases it.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(strings.ToLower(mt))
}

func (t Tool) contentTypes() []string {
	if len(t.ContentTypes) == 0 {
		return DefaultContentTypes()
	}
	return t.ContentTypes
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

func (t Tool) maxOutput() int64 {
	if t.MaxOutput <= 0 {
		return DefaultMaxOutput
	}
	return t.MaxOutput
}
