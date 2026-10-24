package ffmpeg_test

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// program writes a shell script standing in for an FFmpeg program.
func program(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

var runner = ffmpeg.Runner{Package: "media", Formats: []string{"mp4"}}

func TestRun(t *testing.T) {
	out, err := runner.Run(t.Context(), program(t, "printf hello"), nil, 5)
	if err != nil || string(out) != "hello" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if _, err := runner.Run(t.Context(), program(t, "printf hello"), nil, 4); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("over maxOut: %v", err)
	}
	slow := ffmpeg.Runner{Formats: runner.Formats, Timeout: 50 * time.Millisecond}
	if _, err := slow.Run(t.Context(), program(t, "sleep 5"), nil, 1); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("over Timeout: %v", err)
	}
	if _, err := runner.Run(t.Context(), filepath.Join(t.TempDir(), "absent"), nil, 1); !errors.Is(err, ffmpeg.ErrNoProgram) || filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("missing program: %v", err)
	}
}

// TestRun_RefusesAnEmptyDemuxerList pins the safe default: a Runner
// built without Formats does not run FFmpeg with every demuxer open.
func TestRun_RefusesAnEmptyDemuxerList(t *testing.T) {
	if _, err := (ffmpeg.Runner{}).Run(t.Context(), program(t, "printf x"), nil, 1); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(runner.InputOptions(), " "); got != "-protocol_whitelist file -format_whitelist mp4" {
		t.Fatalf("InputOptions = %q", got)
	}
}

func TestRun_FailureIsAClientSafeProgramError(t *testing.T) {
	noisy := program(t, `i=0; while [ $i -lt 2000 ]; do echo "error reading /srv/work/file $i" >&2; i=$((i+1)); done; exit 1`)
	_, err := runner.Run(t.Context(), noisy, nil, 1)
	var pe *ffmpeg.ProgramError
	if !errors.As(err, &pe) || filesystem.CodeOf(err) != filesystem.CodeRejected || len(pe.Detail) == 0 || len(pe.Detail) > ffmpeg.MaxErrorOutput {
		t.Fatalf("err = %v", err)
	}
	if pe.Error() != "media: ffmpeg could not read the file" {
		t.Fatalf("client text = %q", pe.Error())
	}
}

// failingWriter fails every write.
type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestStream_WriterErrorStopsTheProgram(t *testing.T) {
	stop := errors.New("consumer gave up")
	err := runner.Stream(t.Context(), program(t, "printf hello"), nil, failingWriter{stop}, 10)
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkFile(t *testing.T) {
	var seen string
	err := runner.WorkFile(strings.NewReader("abc"), func(path string) error {
		b, err := os.ReadFile(path)
		seen = string(b)
		return err
	})
	if err != nil || seen != "abc" {
		t.Fatalf("WorkFile = %q, %v", seen, err)
	}
	small := ffmpeg.Runner{Formats: runner.Formats, MaxInput: 2}
	if err := small.WorkFile(strings.NewReader("abc"), func(string) error { return nil }); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("over MaxInput: %v", err)
	}
	lost := errors.New("client went away")
	if err := runner.WorkFile(iotest.ErrReader(lost), func(string) error { return nil }); !errors.Is(err, lost) {
		t.Fatalf("a failing reader: %v", err)
	}
	if err := (ffmpeg.Runner{TempDir: filepath.Join(t.TempDir(), "absent")}).WorkFile(strings.NewReader("a"), func(string) error { return nil }); err == nil {
		t.Fatal("a missing TempDir was not reported")
	}
}

func TestOutputFile(t *testing.T) {
	write := func(body string) func(string) error {
		return func(path string) error { return os.WriteFile(path, []byte(body), 0o600) }
	}
	f, err := runner.OutputFile(10, write("result"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "result" {
		t.Fatalf("output = %q", b)
	}
	if _, err := os.Stat(f.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the output file was left on disk: %v", err)
	}
	if _, err := runner.OutputFile(6, write("result")); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("at the cap: %v", err)
	}
	if _, err := runner.OutputFile(10, write("")); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("empty: %v", err)
	}
	boom := errors.New("encode failed")
	if _, err := runner.OutputFile(10, func(string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("fn's error: %v", err)
	}
	if _, err := runner.OutputFile(10, func(path string) error { return os.Remove(path) }); err == nil {
		t.Fatal("a vanished output was not reported")
	}
	if _, err := (ffmpeg.Runner{TempDir: filepath.Join(t.TempDir(), "absent")}).OutputFile(10, write("x")); err == nil {
		t.Fatal("a missing TempDir was not reported")
	}
	if got := ffmpeg.OutputLimit(10); len(got) != 2 || got[0] != "-fs" || got[1] != "10" {
		t.Fatalf("OutputLimit = %v", got)
	}
	if got := ffmpeg.Seconds(-time.Second); got != "0.000" {
		t.Fatalf("Seconds(-1s) = %q", got)
	}
}

// closeCounter counts Close calls.
type closeCounter struct {
	io.Reader
	closed int
}

func (c *closeCounter) Close() error { c.closed++; return nil }

// TestCloseAtEOF pins that the reader closes its source once, at the end,
// and reads nothing after.
func TestCloseAtEOF(t *testing.T) {
	src := &closeCounter{Reader: strings.NewReader("abc")}
	r := ffmpeg.CloseAtEOF(src)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "abc" || src.closed != 1 {
		t.Fatalf("read %q, %v, closed %d times", b, err, src.closed)
	}
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF || src.closed != 1 {
		t.Fatalf("after the end: %d, %v, closed %d times", n, err, src.closed)
	}
}

// TestFromSeconds pins the clamp: a figure past the longest Duration
// must not wrap negative (it does on amd64 without it), so a limit check
// refuses it; run with GOARCH=amd64 too.
func TestFromSeconds(t *testing.T) {
	for in, want := range map[float64]time.Duration{
		1.5: 1500 * time.Millisecond, 0: 0, -3: 0, math.NaN(): 0,
		1e22: math.MaxInt64, 9.2e12: math.MaxInt64, math.Inf(1): math.MaxInt64,
	} {
		if got := ffmpeg.FromSeconds(in); got != want {
			t.Errorf("FromSeconds(%g) = %v, want %v", in, got, want)
		}
	}
	if got := ffmpeg.ParseSeconds("9223372036854.775807"); got != math.MaxInt64 {
		t.Errorf("ParseSeconds(huge) = %v", got)
	}
	if got := ffmpeg.ParseSeconds("N/A"); got != 0 {
		t.Errorf("ParseSeconds(N/A) = %v", got)
	}
}

func TestMeasure(t *testing.T) {
	// Lines arrive across writes, out of order, with an N/A and a stream
	// starting before zero; the span is earliest start to latest end.
	listing := `printf 'pts_time=0.400000|duration_time=0.100000\npts_time=N/A|duration_time=0.1\npts_time=-0.5|'; printf 'duration_time=0.5|\npts_time=19.8|duration_time=0.2\n'`
	d, err := runner.Measure(t.Context(), program(t, listing), "in.mp4")
	if err != nil || d != 20500*time.Millisecond {
		t.Fatalf("Measure = %v, %v", d, err)
	}
	if d, err := runner.Measure(t.Context(), program(t, `printf 'duration_time=1\n'`), "in.mp4"); err != nil || d != 0 {
		t.Fatalf("no timestamps: %v, %v", d, err)
	}
	long := `head -c 2000 /dev/zero | tr '\0' x`
	if _, err := runner.Measure(t.Context(), program(t, long), "in.mp4"); err == nil {
		t.Fatal("an overlong line was accepted")
	}
	if _, err := runner.Measure(t.Context(), program(t, "exit 1"), "in.mp4"); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("failing ffprobe: %v", err)
	}
}

// TestRun_PassesOnlyAnAllowlistedEnvironment pins that FFmpeg sees PATH
// and the variables of Runner.Env, never the application's secrets.
func TestRun_PassesOnlyAnAllowlistedEnvironment(t *testing.T) {
	t.Setenv("FS_TEST_SECRET", "planted-secret")
	r := ffmpeg.Runner{Formats: runner.Formats, Env: []string{"FS_TEST_EXTRA=passed"}}
	out, err := r.Run(t.Context(), program(t, "env"), nil, 1<<20)
	if err != nil || strings.Contains(string(out), "planted-secret") || !strings.Contains(string(out), "PATH=") || !strings.Contains(string(out), "FS_TEST_EXTRA=passed") {
		t.Fatalf("environment:\n%s\n%v", out, err)
	}
}

// TestCheckLength pins the duration guard: a zero limit runs nothing, the
// longer of the claimed and the measured length decides (so neither a
// forged container duration nor forged packet times slips past alone),
// and media without packet timestamps is refused as of unknown length.
func TestCheckLength(t *testing.T) {
	const limit = 10 * time.Second
	// noError is what CodeOf answers for a nil error.
	const noError filesystem.Code = ""
	tenSeconds := program(t, `printf 'pts_time=0|duration_time=10\n'`)
	twentySeconds := program(t, `printf 'pts_time=0|duration_time=20\n'`)
	noTimestamps := program(t, `printf 'duration_time=1\n'`)
	failing := program(t, "exit 1")
	for _, tc := range []struct {
		name    string
		ffprobe string
		claimed time.Duration
		limit   time.Duration
		want    filesystem.Code
	}{
		{"no limit runs nothing", failing, 0, 0, noError},
		{"within the limit", tenSeconds, limit, limit, noError},
		{"packets longer than claimed", twentySeconds, time.Second, limit, filesystem.CodeRejected},
		{"claimed longer than packets", tenSeconds, time.Minute, limit, filesystem.CodeRejected},
		{"no timestamps", noTimestamps, time.Second, limit, filesystem.CodeRejected},
		{"ffprobe fails", failing, time.Second, limit, filesystem.CodeRejected},
	} {
		err := runner.CheckLength(t.Context(), tc.ffprobe, "in.mp4", tc.claimed, tc.limit)
		if got := filesystem.CodeOf(err); got != tc.want {
			t.Errorf("%s: %v (code %v), want %v", tc.name, err, got, tc.want)
		}
	}
}
