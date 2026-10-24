package video_test

import (
	"bytes"
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
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// fakeProgram writes an executable shell script standing in for ffprobe
// or ffmpeg, so a test controls exactly what it prints and how it exits.
// $last in the script is the program's last argument (ffmpeg's output).
func fakeProgram(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	body := "#!/bin/sh\nfor last; do :; done\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// report is an ffprobe JSON report with one stream.
func report(stream string) string {
	return `{"format":{"format_name":"mov","duration":"2.5","bit_rate":"x"},"streams":[` + stream + `]}`
}

func TestProbe_ParsesWhatFfprobePrints(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want video.Info
		code filesystem.Code
	}{
		"not JSON":           {"not json", video.Info{}, filesystem.CodeRejected},
		"no streams":         {report(""), video.Info{}, filesystem.CodeRejected},
		"cover image only":   {report(`{"codec_type":"video","codec_name":"mjpeg","width":1,"height":1,"disposition":{"attached_pic":1}}`), video.Info{}, filesystem.CodeRejected},
		"rotate tag":         {report(`{"codec_type":"video","codec_name":"h264","width":640,"height":360,"avg_frame_rate":"0/0","tags":{"rotate":"90"}}`), video.Info{Format: "mov", Duration: 2500 * time.Millisecond, Width: 360, Height: 640, Rotation: 90, VideoCodec: "h264"}, ""},
		"odd frame rates":    {report(`{"codec_type":"video","codec_name":"vp9","width":2,"height":4,"avg_frame_rate":"abc"}`), video.Info{Format: "mov", Duration: 2500 * time.Millisecond, Width: 2, Height: 4, VideoCodec: "vp9"}, ""},
		"frame rate no den":  {report(`{"codec_type":"video","codec_name":"vp9","width":2,"height":4,"avg_frame_rate":"30/x"}`), video.Info{Format: "mov", Duration: 2500 * time.Millisecond, Width: 2, Height: 4, VideoCodec: "vp9"}, ""},
		"upside-down matrix": {report(`{"codec_type":"video","codec_name":"h264","width":8,"height":6,"side_data_list":[{"rotation":180}]}`), video.Info{Format: "mov", Duration: 2500 * time.Millisecond, Width: 8, Height: 6, Rotation: 180, VideoCodec: "h264"}, ""},
	} {
		tool := video.Tool{FFprobe: fakeProgram(t, "ffprobe", "cat <<'EOF'\n"+tc.out+"\nEOF")}
		info, err := tool.Probe(t.Context(), strings.NewReader("x"))
		if filesystem.CodeOf(err) != tc.code || info != tc.want {
			t.Errorf("%s: %+v, %v; want %+v, %q", name, info, err, tc.want, tc.code)
		}
	}
}

func TestPoster_NoFrameIsRejected(t *testing.T) {
	tool := video.Tool{FFmpeg: fakeProgram(t, "ffmpeg", "exit 0")}
	if _, err := tool.Poster(t.Context(), strings.NewReader("x"), time.Second); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("err = %v", err)
	}
}

// TestRun_ErrorOutputIsBoundedAndNamesNoPath pins that a program printing
// megabytes of errors cannot grow ProgramError.Detail past a few KiB, and
// that the client message names the program, never its configured path.
func TestRun_ErrorOutputIsBoundedAndNamesNoPath(t *testing.T) {
	tool := video.Tool{FFprobe: fakeProgram(t, "ffprobe", `i=0; while [ $i -lt 2000 ]; do echo "error: line $i of noise" >&2; i=$((i+1)); done; exit 1`)}
	_, err := tool.Probe(t.Context(), strings.NewReader("x"))
	var pe *video.ProgramError
	if !errors.As(err, &pe) || len(pe.Detail) == 0 || len(pe.Detail) > 4<<10 {
		t.Fatalf("detail is %d bytes, err %v", len(pe.Detail), err)
	}
	if pe.Program != "ffprobe" || strings.Contains(fshttp.ClientMessage(err), string(filepath.Separator)) {
		t.Fatalf("the client message names the configured path: %q", fshttp.ClientMessage(err))
	}
}

func TestTool_WorkFileFailures(t *testing.T) {
	if _, err := (video.Tool{TempDir: filepath.Join(t.TempDir(), "missing")}).Probe(t.Context(), strings.NewReader("x")); err == nil {
		t.Fatal("a missing TempDir was not reported")
	}
	errRead := errors.New("client went away")
	if _, err := (video.Tool{}).Probe(t.Context(), iotest.ErrReader(errRead)); !errors.Is(err, errRead) {
		t.Fatalf("a failing reader: %v", err)
	}
}

func TestSanitize_FfmpegFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		code   filesystem.Code
	}{
		"ffmpeg fails":      {"exit 1", filesystem.CodeRejected},
		"output disappears": {`rm -f "$last"`, ""},
		"output is empty":   {`: > "$last"`, filesystem.CodeRejected},
		"a clean copy":      {`echo ok > "$last"`, "ok"},
	} {
		out, _, err := video.Tool{FFmpeg: fakeProgram(t, "ffmpeg", tc.script)}.Sanitize()(t.Context(), upload.Info{Filename: "clip.mp4", ContentType: "video/mp4"}, strings.NewReader("x"))
		switch tc.code {
		case "":
			if err == nil {
				t.Errorf("%s: no error", name)
			}
		case "ok":
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got, _ := io.ReadAll(out)
			if string(got) != "ok\n" {
				t.Errorf("%s: %q", name, got)
			}
			// Read after the end: the file is closed, and EOF again.
			if n, err := out.Read(make([]byte, 8)); n != 0 || err != io.EOF {
				t.Errorf("%s: read after EOF = %d, %v", name, n, err)
			}
		default:
			if filesystem.CodeOf(err) != tc.code {
				t.Errorf("%s: err = %v, want %s", name, err, tc.code)
			}
		}
	}
}

// TestSanitize_CleanFileCannotBeCreated covers a work directory that
// stops accepting files after the input was copied: the clean copy cannot
// be created, and the error is reported rather than an empty video.
func TestSanitize_CleanFileCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	body := &lockAtEOF{r: strings.NewReader("x"), dir: dir}
	_, _, err := video.Tool{TempDir: dir, FFmpeg: fakeProgram(t, "ffmpeg", "exit 0")}.Sanitize()(t.Context(), upload.Info{ContentType: "video/mp4"}, body)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}

// lockAtEOF makes dir read-only once r is exhausted.
type lockAtEOF struct {
	r   io.Reader
	dir string
}

func (l *lockAtEOF) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err == io.EOF {
		_ = os.Chmod(l.dir, 0o500)
	}
	return n, err
}

func TestInspect_Failures(t *testing.T) {
	d := newDisk(t)
	inspect := video.Tool{FFprobe: fakeProgram(t, "ffprobe", "exit 1")}.Inspect(video.Limits{})
	if err := inspect(t.Context(), d, upload.Result{Key: "missing.mp4", ContentType: "video/mp4"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing object: %v", err)
	}
	if _, err := d.WriteBytes(t.Context(), "bad.mp4", []byte("not a video"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := inspect(t.Context(), d, upload.Result{Key: "bad.mp4", ContentType: "video/mp4"}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("unreadable object: %v", err)
	}
}

func TestPlugin_Failures(t *testing.T) {
	body := clip(t)
	e := newEnv(t, video.NewPlugin(video.Tool{}))
	p := video.NewPlugin(video.Tool{}, video.WithPoster(0))
	ctx := t.Context()
	if err := p.BeforeCreate(ctx, e.store, &records.File{Disk: "nope", Key: "a.mp4", ContentType: "video/mp4"}); err == nil {
		t.Fatal("an unknown disk was not reported")
	}
	if err := p.BeforeCreate(ctx, e.store, &records.File{Disk: "media", Key: "missing.mp4", ContentType: "video/mp4"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing object: %v", err)
	}

	// Without WithPoster, only the metadata.
	f, err := e.upload(t, body)
	if _, ok := video.PosterKey(f); err != nil || ok {
		t.Fatalf("a poster without WithPoster: %v %v", f.Metadata, err)
	}

	// A strict plugin refuses a replacement it cannot read.
	strict := newEnv(t, video.NewPlugin(video.Tool{}, video.WithStrict()))
	g, err := strict.upload(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.store.Replace(ctx, scope, g.ID, bytes.NewReader(body[:200]), "", videoPolicy); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("strict replace: %v", err)
	}
}

// TestPlugin_PosterWriteFails covers a disk that refuses the poster.
func TestPlugin_PosterWriteFails(t *testing.T) {
	body := clip(t)
	drv := memory.New()
	writable, err := filesystem.NewDisk("media", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writable.WriteBytes(t.Context(), "clip.mp4", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	readOnly, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	p := video.NewPlugin(video.Tool{}, video.WithPoster(0))
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.BeforeCreate(t.Context(), store, &records.File{Disk: "media", Key: "clip.mp4", ContentType: "video/mp4"}); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Fatalf("err = %v", err)
	}
}

// probeScript is a fake ffprobe answering the metadata probe with a
// report whose container duration is duration, and Measure's packet
// listing with packets.
func probeScript(t *testing.T, duration, packets string) string {
	t.Helper()
	rep := `{"format":{"format_name":"mov","duration":"` + duration + `"},"streams":[{"codec_type":"video","codec_name":"h264","width":8,"height":6}]}`
	return fakeProgram(t, "ffprobe", `case "$*" in *packet=*) printf '`+packets+`' ;; *) echo '`+rep+`' ;; esac`)
}

// TestInspect_AbsurdOrUnknownLength pins the clamp and the refusals: a
// container claiming 1e22 seconds (ffprobe prints about 9.2e12) once
// wrapped to a negative Duration on amd64 and passed MaxDuration; run
// with GOARCH=amd64 too. A video whose packets carry no timestamps is
// refused when a length limit is set, and passes without one.
func TestInspect_AbsurdOrUnknownLength(t *testing.T) {
	d := newDisk(t)
	if _, err := d.WriteBytes(t.Context(), "v.mp4", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	r := upload.Result{Key: "v.mp4", ContentType: "video/mp4"}
	absurd := video.Tool{FFprobe: probeScript(t, "9223372036854.775807", `pts_time=0|duration_time=1\n`)}
	if info, err := absurd.Probe(t.Context(), strings.NewReader("x")); err != nil || info.Duration != math.MaxInt64 {
		t.Fatalf("Probe of an absurd length = %v, %v", info.Duration, err)
	}
	if err := absurd.Inspect(video.Limits{MaxDuration: 5 * time.Second})(t.Context(), d, r); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("an absurd claimed length passed: %v", err)
	}
	unknown := video.Tool{FFprobe: probeScript(t, "N/A", `pts_time=N/A|duration_time=1\n`)}
	if err := unknown.Inspect(video.Limits{MaxDuration: 5 * time.Second})(t.Context(), d, r); filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), "unknown length") {
		t.Fatalf("an unknown length passed a length limit: %v", err)
	}
	if err := unknown.Inspect(video.Limits{MaxWidth: 100})(t.Context(), d, r); err != nil {
		t.Fatalf("an unknown length without a length limit: %v", err)
	}
	failing := video.Tool{FFprobe: fakeProgram(t, "ffprobe", `case "$*" in *packet=*) exit 1 ;; *) echo '`+report(`{"codec_type":"video","codec_name":"h264","width":8,"height":6}`)+`' ;; esac`)}
	if err := failing.Inspect(video.Limits{MaxDuration: time.Minute})(t.Context(), d, r); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a failing packet scan: %v", err)
	}
}

// TestStoryboard_AbsurdLength pins the last tile's end: with the longest
// Duration, n×interval wrapped negative.
func TestStoryboard_AbsurdLength(t *testing.T) {
	tool := video.Tool{FFprobe: probeScript(t, "9223372036854.775807", ""), FFmpeg: fakeProgram(t, "ffmpeg", "printf jpeg")}
	s, err := tool.Storyboard(t.Context(), strings.NewReader("x"), video.Storyboard{})
	if err != nil {
		t.Fatal(err)
	}
	for i, tile := range s.Tiles {
		if tile.End <= tile.Start {
			t.Fatalf("tile %d spans %v to %v", i, tile.Start, tile.End)
		}
	}
	if last := s.Tiles[len(s.Tiles)-1]; last.End != math.MaxInt64 {
		t.Fatalf("last tile ends at %v", last.End)
	}
}

// TestSanitize_OutputBounds pins Sanitize on the shared runner: an output
// past twice MaxInput is CodeTooLarge (FFmpeg gets -fs to stop there),
// and what a file-writing run prints on standard output has its own
// limit, not the error output's 4 KiB.
func TestSanitize_OutputBounds(t *testing.T) {
	sanitize := func(script string) error {
		tool := video.Tool{MaxInput: 10, FFmpeg: fakeProgram(t, "ffmpeg", script)}
		_, _, err := tool.Sanitize()(t.Context(), upload.Info{ContentType: "video/mp4"}, strings.NewReader("x"))
		return err
	}
	if err := sanitize(`case "$*" in *"-fs 20"*) head -c 64 /dev/zero > "$last" ;; *) echo ok > "$last" ;; esac`); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("an output past the cap: %v", err)
	}
	if err := sanitize(`head -c 8192 /dev/zero; echo ok > "$last"`); err != nil {
		t.Fatalf("8 KiB of standard output: %v", err)
	}
}

// TestTool_EnvReachesFFmpeg pins that Tool.Env is forwarded to the
// programs and that the parent's other variables are not.
func TestTool_EnvReachesFFmpeg(t *testing.T) {
	t.Setenv("FS_TEST_SECRET", "planted-secret")
	log := filepath.Join(t.TempDir(), "env")
	tool := video.Tool{FFprobe: fakeProgram(t, "ffprobe", "env > "+log+"; exit 1"), Env: []string{"FS_TEST_EXTRA=passed"}}
	_, _ = tool.Probe(t.Context(), strings.NewReader("media"))
	b, err := os.ReadFile(log)
	if env := string(b); err != nil || !strings.Contains(env, "FS_TEST_EXTRA=passed") || strings.Contains(env, "planted-secret") {
		t.Fatalf("environment:\n%s\n%v", env, err)
	}
}
