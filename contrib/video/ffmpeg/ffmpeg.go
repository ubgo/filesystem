// Package ffmpeg runs FFmpeg's programs (ffprobe, ffmpeg) on untrusted
// media with every run bounded: the input copied to a work file is
// capped, each run is killed after a timeout, its output is capped, and
// FFmpeg may open only the demuxers the caller lists and the "file"
// protocol, so a playlist or concat script disguised as media cannot make
// it read other files or URLs. Programs run through os/exec without a
// shell. It is the mechanism under contrib/video and contrib/audio, and
// is usable on its own for any other FFmpeg job.
package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
)

// Default program names, looked up on PATH.
const (
	FFprobe = "ffprobe"
	FFmpeg  = "ffmpeg"
)

// Defaults used when a Runner leaves a field zero.
const (
	// DefaultTimeout bounds one program run.
	DefaultTimeout = time.Minute
	// DefaultMaxInput bounds the bytes copied to a work file.
	DefaultMaxInput int64 = 2 << 30
)

// MaxErrorOutput bounds what is kept of a program's error output, for
// ProgramError.Detail.
const MaxErrorOutput = 4 << 10

// MaxFileRunOutput bounds the standard output of a run that writes its
// result to a file (OutputFile): such a run prints nothing there, so more
// than this is a misbehaving program, stopped as CodeTooLarge.
const MaxFileRunOutput = 64 << 10

// maxPacketReport bounds ffprobe's packet listing in Measure. A packet
// prints about 45 bytes, so it covers some 6 million packets, more than
// a DefaultMaxInput file of real media holds (30 fps video for two days);
// a longer listing is CodeTooLarge.
const maxPacketReport = 256 << 20

// maxPacketLine bounds one line of the packet listing; the two fields
// Measure asks for print well under it.
const maxPacketLine = 1 << 10

// ErrNoProgram is in the chain of the error of a run whose program is not
// installed (or not at the configured path), classified CodeUnavailable:
// a deployment problem, not the client's file.
var ErrNoProgram = errors.New("ffmpeg: program not found")

// ProgramError is the cause of a run that failed on its input, inside a
// CodeRejected error. Its Error text is safe to show a client (it is what
// fshttp.ClientMessage shows for a rejected upload); Detail is the
// program's own error output, which names server paths, for logs only.
// Program is the program's base name, because a configured path would
// name the server's layout to a client; Package names the caller
// ("video", "audio") for the message.
type ProgramError struct {
	Package string
	Program string
	Detail  string
}

func (e *ProgramError) Error() string {
	return e.Package + ": " + e.Program + " could not read the file"
}

// Runner runs FFmpeg's programs with bounds. Formats is required: a
// Runner without it refuses to run, because an empty demuxer list would
// leave every FFmpeg demuxer, playlists included, open to the input.
type Runner struct {
	// Package prefixes error messages ("video").
	Package string
	// Formats are the demuxers FFmpeg may use (-format_whitelist).
	Formats []string
	// Timeout bounds each run; 0 means DefaultTimeout. A run cut short is
	// CodeUnavailable.
	Timeout time.Duration
	// MaxInput bounds the bytes WorkFile copies; 0 means DefaultMaxInput.
	// A longer input is CodeTooLarge.
	MaxInput int64
	// TempDir is where work files go; empty means os.TempDir().
	TempDir string
	// Env adds variables to the program's environment, as "KEY=value"
	// (FONTCONFIG_FILE, a library path). The program otherwise sees only
	// PATH, HOME, TMPDIR, TMP, LANG and LC_ALL of this process, so the
	// application's secrets never reach a parser of untrusted files.
	Env []string
}

// WorkFile copies at most MaxInput bytes of r to a work file, runs fn on
// its path and removes it. FFmpeg needs a seekable input: MP4 files from
// phones keep their index at the end.
func (r Runner) WorkFile(src io.Reader, fn func(path string) error) error {
	f, err := os.CreateTemp(r.TempDir, r.pkg()+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	limit := r.maxInput()
	n, err := io.Copy(f, io.LimitReader(src, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return filesystem.Errorf(filesystem.CodeTooLarge, "%s: larger than %d bytes", r.pkg(), limit)
	}
	return fn(f.Name())
}

// OutputFile runs fn with the path of a fresh work file for a program to
// write, then opens it and unlinks it, so nothing is left on disk
// whatever the caller does with the file; the caller closes it. An output
// larger than max is CodeTooLarge and an empty one CodeRejected. Pair it
// with OutputLimit in the program's arguments so FFmpeg stops writing at
// the cap instead of filling the disk.
func (r Runner) OutputFile(max int64, fn func(path string) error) (*os.File, error) {
	dst, err := os.CreateTemp(r.TempDir, r.pkg()+"-out-*")
	if err != nil {
		return nil, err
	}
	name := dst.Name()
	_ = dst.Close()
	defer func() { _ = os.Remove(name) }()
	if err := fn(name); err != nil {
		return nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
		_ = f.Close()
		return nil, err
	case fi.Size() == 0:
		_ = f.Close()
		return nil, filesystem.Errorf(filesystem.CodeRejected, "%s: the program wrote nothing", r.pkg())
	case fi.Size() >= max:
		// -fs stops FFmpeg at the cap, so an output that reached it was
		// cut short.
		_ = f.Close()
		return nil, filesystem.Errorf(filesystem.CodeTooLarge, "%s: output larger than %d bytes", r.pkg(), max)
	}
	return f, nil
}

// CloseAtEOF returns a reader of rc that closes rc when reading reaches
// the end or fails, so an OutputFile handed to code that only reads (an
// upload.Processor's caller) releases its descriptor without a Close. A
// reader abandoned before its end (a size cap tripped) releases it when
// the file is collected.
func CloseAtEOF(rc io.ReadCloser) io.Reader { return &closeAtEOF{rc: rc} }

type closeAtEOF struct{ rc io.ReadCloser }

func (c *closeAtEOF) Read(p []byte) (int, error) {
	if c.rc == nil {
		return 0, io.EOF
	}
	n, err := c.rc.Read(p)
	if err != nil {
		_ = c.rc.Close()
		c.rc = nil
	}
	return n, err
}

// OutputLimit is FFmpeg's -fs option for an output of at most max bytes.
func OutputLimit(max int64) []string {
	return []string{"-fs", strconv.FormatInt(max, 10)}
}

// InputOptions limit what FFmpeg may open while reading the input; put
// them before -i (and in ffprobe's arguments).
func (r Runner) InputOptions() []string {
	return []string{"-protocol_whitelist", "file", "-format_whitelist", strings.Join(r.Formats, ",")}
}

// Run executes program with args under the timeout and returns at most
// maxOut bytes of its standard output; more is CodeTooLarge.
func (r Runner) Run(ctx context.Context, program string, args []string, maxOut int64) ([]byte, error) {
	var out bytes.Buffer
	if err := r.Stream(ctx, program, args, &out, maxOut); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Stream executes program with args under the timeout, passing its
// standard output to w as it is written; past maxOut bytes the program is
// stopped and the run is CodeTooLarge. An error from w stops the program
// and is returned.
func (r Runner) Stream(ctx context.Context, program string, args []string, w io.Writer, maxOut int64) error {
	if len(r.Formats) == 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "%s: no demuxers allowed (Runner.Formats is empty)", r.pkg())
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%w: %s (install FFmpeg or configure its path): %w", ErrNoProgram, program, err))
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = programEnv(r.Env)
	stdout := &capped{w: w, room: maxOut}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &truncating{buf: &stderr, room: MaxErrorOutput}
	err = cmd.Run()
	switch {
	case stdout.over:
		return filesystem.Errorf(filesystem.CodeTooLarge, "%s: %s wrote more than %d bytes", r.pkg(), filepath.Base(program), maxOut)
	case stdout.err != nil:
		return stdout.err
	case ctx.Err() != nil:
		return filesystem.NewError(filesystem.CodeUnavailable, fmt.Errorf("%s: %s did not finish within %s: %w", r.pkg(), filepath.Base(program), r.timeout(), ctx.Err()))
	case err != nil:
		return filesystem.NewError(filesystem.CodeRejected, &ProgramError{Package: r.pkg(), Program: filepath.Base(program), Detail: strings.TrimSpace(stderr.String())})
	}
	return nil
}

// FromSeconds converts a figure in seconds that a program or a file
// reported to a time.Duration. Zero, a negative figure and NaN are 0
// (unknown); a figure past the longest Duration (a container claiming
// 1e22 seconds) is the longest Duration rather than a value that wraps
// negative on conversion, so a limit check refuses it instead of
// passing it.
func FromSeconds(sec float64) time.Duration {
	if !(sec > 0) {
		return 0
	}
	ns := sec * float64(time.Second)
	if ns >= math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(ns)
}

// ParseSeconds is FromSeconds of ffprobe's text form ("2.500000");
// anything unparseable ("N/A") is 0.
func ParseSeconds(s string) time.Duration {
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return FromSeconds(sec)
}

// Measure returns how long the media at path plays according to its
// packets' timestamps (the end of the last packet minus the start of
// the first, over every stream), not the duration its container claims:
// a header can be patched to say 1 second over 20 seconds of packets,
// and the timestamps are what a player follows. ffprobe lists the packets
// without decoding them, under the Runner's bounds; 0 means no packet
// carried a timestamp, which a caller enforcing a length should refuse.
func (r Runner) Measure(ctx context.Context, ffprobe, path string) (time.Duration, error) {
	args := append(r.InputOptions(), "-v", "error", "-show_entries", "packet=pts_time,duration_time", "-of", "compact=p=0", path)
	span := &packetSpan{}
	if err := r.Stream(ctx, ffprobe, args, span, maxPacketReport); err != nil {
		return 0, err
	}
	if err := span.flush(); err != nil {
		return 0, err
	}
	if !span.seen {
		return 0, nil
	}
	return FromSeconds(span.end - span.start), nil
}

// CheckLength refuses (CodeRejected) the media at path when it plays
// longer than limit: by the duration its container claims, or by the
// packets' span (Measure), since either alone can be forged; media whose
// packets carry no timestamps is refused as of unknown length. A limit of
// 0 checks nothing and runs nothing.
func (r Runner) CheckLength(ctx context.Context, ffprobe, path string, claimed, limit time.Duration) error {
	if limit <= 0 {
		return nil
	}
	measured, err := r.Measure(ctx, ffprobe, path)
	switch {
	case err != nil:
		return err
	case measured <= 0:
		return filesystem.Errorf(filesystem.CodeRejected, "%s: unknown length", r.pkg())
	case max(claimed, measured) > limit:
		return filesystem.Errorf(filesystem.CodeRejected, "%s is longer than %s", r.pkg(), limit)
	}
	return nil
}

// packetSpan folds ffprobe's compact packet lines
// ("pts_time=0.4|duration_time=0.1") into the earliest start and the
// latest end, holding at most one partial line.
type packetSpan struct {
	start, end float64
	seen       bool
	line       []byte
}

// errPacketLine stops a listing whose line is longer than maxPacketLine.
var errPacketLine = errors.New("ffmpeg: packet listing line too long")

func (p *packetSpan) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.line = append(p.line, b...)
			if len(p.line) > maxPacketLine {
				return 0, errPacketLine
			}
			return n, nil
		}
		p.line = append(p.line, b[:i]...)
		if err := p.flush(); err != nil {
			return 0, err
		}
		b = b[i+1:]
	}
	return n, nil
}

// flush folds the held line, if any.
func (p *packetSpan) flush() error {
	if len(p.line) > maxPacketLine {
		return errPacketLine
	}
	var pts, dur float64
	hasPTS := false
	for field := range strings.SplitSeq(string(p.line), "|") {
		k, v, _ := strings.Cut(field, "=")
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		switch k {
		case "pts_time":
			pts, hasPTS = f, true
		case "duration_time":
			dur = max(f, 0)
		}
	}
	p.line = p.line[:0]
	if !hasPTS {
		return nil
	}
	if !p.seen || pts < p.start {
		p.start = pts
	}
	if !p.seen || pts+dur > p.end {
		p.end = pts + dur
	}
	p.seen = true
	return nil
}

// Seconds formats d for FFmpeg's time options (-ss, -t).
func Seconds(d time.Duration) string {
	return strconv.FormatFloat(max(d, 0).Seconds(), 'f', 3, 64)
}

// capped passes writes to w until room is spent; past it, it reports
// over and fails the write, which ends the program early. An error from w
// is kept and fails the write too.
type capped struct {
	w    io.Writer
	room int64
	over bool
	err  error
}

// errOutputCap stops a program that writes past its cap.
var errOutputCap = errors.New("ffmpeg: output cap reached")

func (c *capped) Write(p []byte) (int, error) {
	if int64(len(p)) > c.room {
		c.over = true
		return 0, errOutputCap
	}
	n, err := c.w.Write(p)
	c.room -= int64(n)
	if err != nil {
		c.err = err
	}
	return n, err
}

// truncating keeps at most room bytes and silently drops the rest, so a
// program printing megabytes of errors still runs to its own end.
type truncating struct {
	buf  *bytes.Buffer
	room int
}

func (t *truncating) Write(p []byte) (int, error) {
	keep := min(len(p), t.room)
	t.buf.Write(p[:keep])
	t.room -= keep
	return len(p), nil
}

func (r Runner) pkg() string {
	if r.Package == "" {
		return "ffmpeg"
	}
	return r.Package
}

func (r Runner) timeout() time.Duration {
	if r.Timeout <= 0 {
		return DefaultTimeout
	}
	return r.Timeout
}

func (r Runner) maxInput() int64 {
	if r.MaxInput <= 0 {
		return DefaultMaxInput
	}
	return r.MaxInput
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

// namePattern is what an encoder, muxer or pixel format name passed to
// FFmpeg looks like: "libx264", "h264_videotoolbox", "webm",
// "yuv420p10le", "libvpx-vp9". Nothing starting with "-" (an option) or
// holding a path separator passes.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// extensionPattern is a file extension with its dot.
var extensionPattern = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

// CheckProfile checks the names of a transcode profile before they reach
// FFmpeg's command line: each name must look like an FFmpeg encoder,
// muxer or pixel format name, the extension like ".webm", and the content
// type a media type with one of the prefixes. A profile is the
// application's configuration, never a client's input; the check keeps a
// mistyped one from being read as an option. Any failure is
// CodeInvalidArgument.
func CheckProfile(names []string, contentType, extension string, prefixes ...string) error {
	if err := CheckNames(names...); err != nil {
		return err
	}
	if !extensionPattern.MatchString(extension) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "ffmpeg: extension %q must be a dot and 1 to 8 lower-case letters or digits", extension)
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || !slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(mt, p) }) {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "ffmpeg: content type %q is not one of %v", contentType, prefixes)
	}
	return nil
}

// CheckNames checks encoder, muxer and pixel format names before they
// reach FFmpeg's command line, for an output that has no single file
// type of its own (an HLS ladder): each must look like an FFmpeg name,
// so a mistyped one is never read as an option or a path. Any failure is
// CodeInvalidArgument.
func CheckNames(names ...string) error {
	for _, n := range names {
		if !namePattern.MatchString(n) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "ffmpeg: %q is not an encoder, muxer or pixel format name", n)
		}
	}
	return nil
}

// boundOptions are the options a profile's args may not hold: those that
// read another input, change the streams or filters, or move a bound the
// caller set (duration, frames, frame rate, size, container, overwrite).
// An option with a stream specifier ("-filter:v:0") counts as its base.
var boundOptions = []string{"-i", "-vf", "-af", "-filter", "-filter_complex", "-lavfi", "-map", "-map_metadata", "-map_chapters", "-f", "-y", "-n", "-fs", "-t", "-to", "-ss", "-frames", "-vframes", "-aframes", "-fpsmax", "-r"}

// CheckProfileArgs refuses args that hold one of the options a transcode
// keeps for itself (boundOptions), so a profile cannot lift the safety
// bounds whatever else it sets: CodeInvalidArgument naming the option.
func CheckProfileArgs(args ...[]string) error {
	for _, list := range args {
		for _, a := range list {
			base, _, _ := strings.Cut(a, ":")
			if slices.Contains(boundOptions, base) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "ffmpeg: a profile may not set %s; the transcode sets it", a)
			}
		}
	}
	return nil
}
