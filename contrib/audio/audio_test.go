package audio_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/audio"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// requirePrograms skips the test when FFmpeg is not installed: the only
// reason a test of this package may skip.
func requirePrograms(t *testing.T) {
	t.Helper()
	for _, p := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed (brew install ffmpeg, apt install ffmpeg)", p)
		}
	}
}

// generate runs ffmpeg with args, writing name, and returns its bytes.
func generate(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	requirePrograms(t)
	path := filepath.Join(t.TempDir(), name)
	out, err := exec.CommandContext(t.Context(), "ffmpeg", append(append([]string{"-v", "error", "-y"}, args...), path)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rampSeconds is the length of the ramp test clip.
const rampSeconds = 2

// ramp is a 2-second stereo 44.1 kHz WAV, silent for its first second
// and a full-scale tone for its second, with a title: a waveform that
// must come out half quiet, half loud.
func ramp(t *testing.T) []byte {
	return generate(t, "ramp.wav", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo:d=1", "-f", "lavfi", "-i", "aevalsrc=0.95*sin(440*2*PI*t)|0.95*sin(440*2*PI*t):s=44100:d=1",
		"-filter_complex", "[0][1]concat=n=2:v=0:a=1", "-metadata", "title=voice note")
}

// fakeProgram writes a shell script standing in for ffprobe or ffmpeg;
// $last is its last argument.
func fakeProgram(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nfor last; do :; done\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbe(t *testing.T) {
	info, err := audio.Tool{}.Probe(t.Context(), bytes.NewReader(ramp(t)))
	if err != nil || info.Codec != "pcm_s16le" || info.Duration != rampSeconds*time.Second || info.SampleRate != 44100 || info.Channels != 2 || info.BitRate == 0 {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	video := generate(t, "silent.mp4", "-f", "lavfi", "-i", "testsrc=size=64x64:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	for name, body := range map[string][]byte{"no audio stream": video, "not audio": []byte("hello")} {
		if _, err := (audio.Tool{}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s: %v", name, err)
		}
	}
	var pe *audio.ProgramError
	if _, err := (audio.Tool{}).Probe(t.Context(), strings.NewReader("hello")); !errors.As(err, &pe) || pe.Error() != "audio: ffprobe could not read the file" {
		t.Fatalf("client message: %v", err)
	}
	if _, err := (audio.Tool{FFprobe: filepath.Join(t.TempDir(), "absent")}).Probe(t.Context(), strings.NewReader("x")); !errors.Is(err, audio.ErrNoProgram) {
		t.Fatalf("missing program: %v", err)
	}
	garbled := fakeProgram(t, "ffprobe", "echo not-json")
	if _, err := (audio.Tool{FFprobe: garbled}).Probe(t.Context(), strings.NewReader("x")); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("garbled report: %v", err)
	}
}

func TestWaveform(t *testing.T) {
	peaks, err := audio.Tool{}.Waveform(t.Context(), bytes.NewReader(ramp(t)), 8)
	if err != nil || len(peaks) != 8 {
		t.Fatalf("Waveform = %v, %v", peaks, err)
	}
	for i, p := range peaks {
		quiet := i < 4
		if quiet && p > 0.01 || !quiet && (p < 0.9 || p > 1) {
			t.Fatalf("peak %d = %.3f; want the first half silent and the second loud: %v", i, p, peaks)
		}
	}
}

func TestWaveform_Bounds(t *testing.T) {
	body := ramp(t)
	for name, tc := range map[string]struct {
		tool   audio.Tool
		points int
		code   filesystem.Code
	}{
		"no points":           {audio.Tool{}, 0, filesystem.CodeInvalidArgument},
		"too many points":     {audio.Tool{}, audio.MaxWaveformPoints + 1, filesystem.CodeInvalidArgument},
		"longer than allowed": {audio.Tool{MaxDuration: time.Second}, 8, filesystem.CodeRejected},
		"over MaxInput":       {audio.Tool{MaxInput: 100}, 8, filesystem.CodeTooLarge},
		"over Timeout":        {audio.Tool{Timeout: time.Nanosecond}, 8, filesystem.CodeUnavailable},
		"unknown length":      {audio.Tool{FFprobe: fakeProgram(t, "ffprobe", `echo '{"format":{},"streams":[{"codec_type":"audio"}]}'`)}, 8, filesystem.CodeRejected},
	} {
		if _, err := tc.tool.Waveform(t.Context(), bytes.NewReader(body), tc.points); filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
}

// TestWaveform_DecodingIsCutAtMaxDuration pins that a container claiming
// to be short cannot make FFmpeg decode past MaxDuration: the decoded
// stream is cut (-t) and capped.
func TestWaveform_DecodingIsCutAtMaxDuration(t *testing.T) {
	short := fakeProgram(t, "ffprobe", `echo '{"format":{"duration":"0.5"},"streams":[{"codec_type":"audio"}]}'`)
	long := generate(t, "long.wav", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000:duration=30")
	peaks, err := audio.Tool{FFprobe: short, MaxDuration: time.Second}.Waveform(t.Context(), bytes.NewReader(long), 4)
	if err != nil || len(peaks) != 4 {
		t.Fatalf("Waveform = %v, %v", peaks, err)
	}
	flood := fakeProgram(t, "ffmpeg", "head -c 100000 /dev/zero")
	if _, err := (audio.Tool{FFprobe: short, FFmpeg: flood, MaxDuration: time.Second}).Waveform(t.Context(), bytes.NewReader(long), 4); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("a decoder writing past MaxDuration's samples: %v", err)
	}
}

// transcode runs Transcode and reads its output.
func transcode(t *testing.T, tool audio.Tool, body []byte, opts audio.Transcode) ([]byte, error) {
	t.Helper()
	rc, err := tool.Transcode(t.Context(), bytes.NewReader(body), opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func TestTranscode(t *testing.T) {
	body := ramp(t)
	for _, tc := range []struct {
		opts             audio.Transcode
		codec, container string
		channels         int
	}{
		{audio.Transcode{Mono: true}, "opus", "ogg", 1},
		{audio.Transcode{Format: audio.AAC, BitRate: 64_000}, "aac", "mov,mp4,m4a,3gp,3g2,mj2", 2},
	} {
		out, err := transcode(t, audio.Tool{}, body, tc.opts)
		if err != nil {
			t.Fatal(err)
		}
		info, err := audio.Tool{}.Probe(t.Context(), bytes.NewReader(out))
		if err != nil || info.Codec != tc.codec || info.Format != tc.container || info.Channels != tc.channels {
			t.Fatalf("%s: %+v, %v", tc.opts.Format, info, err)
		}
		if bytes.Contains(out, []byte("voice note")) {
			t.Fatalf("%s: the title survived", tc.opts.Format)
		}
	}
	if audio.Opus.ContentType() != "audio/ogg" || audio.AAC.Extension() != ".m4a" || audio.Format("flac").ContentType() != "" {
		t.Fatal("format types")
	}
}

func TestTranscode_Bounds(t *testing.T) {
	body := ramp(t)
	for name, tc := range map[string]struct {
		tool audio.Tool
		opts audio.Transcode
		code filesystem.Code
	}{
		"unknown format":   {audio.Tool{}, audio.Transcode{Format: "flac"}, filesystem.CodeInvalidArgument},
		"negative":         {audio.Tool{}, audio.Transcode{BitRate: -1}, filesystem.CodeInvalidArgument},
		"too long":         {audio.Tool{MaxDuration: time.Second}, audio.Transcode{}, filesystem.CodeRejected},
		"over MaxOutput":   {audio.Tool{}, audio.Transcode{MaxOutput: 100}, filesystem.CodeTooLarge},
		"not audio":        {audio.Tool{}, audio.Transcode{}, filesystem.CodeRejected},
		"encoder failures": {audio.Tool{FFmpeg: fakeProgram(t, "ffmpeg", "exit 1")}, audio.Transcode{}, filesystem.CodeRejected},
	} {
		in := body
		if name == "not audio" {
			in = []byte("hello")
		}
		if _, err := transcode(t, tc.tool, in, tc.opts); filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
	echo := fakeProgram(t, "ffmpeg", `echo "$@" > "$last"`)
	out, err := transcode(t, audio.Tool{FFmpeg: echo, MaxDuration: 5 * time.Second}, body, audio.Transcode{MaxOutput: 9000})
	for _, want := range []string{"-t 5.000", "-fs 9000", "-protocol_whitelist file", "-map_metadata -1"} {
		if err != nil || !strings.Contains(string(out), want) {
			t.Errorf("ffmpeg ran without %q: %s %v", want, out, err)
		}
	}
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func receive(t *testing.T, d *filesystem.Disk, name string, body []byte, p upload.Policy) (upload.Result, error) {
	t.Helper()
	return upload.Receive(t.Context(), d, bytes.NewReader(body), name, p)
}

var policy = upload.Policy{MaxSize: 10 << 20, Allowed: []string{"audio/*", "text/plain"}, Key: upload.RandomKey("voice")}

func TestTranscodeProcessor(t *testing.T) {
	d := newDisk(t)
	p := policy
	p.Process = audio.Tool{}.TranscodeProcessor(audio.Transcode{Mono: true})
	res, err := receive(t, d, "note.wav", ramp(t), p)
	if err != nil || res.ContentType != "audio/ogg" || res.Filename != "note.ogg" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if res, err := receive(t, d, "notes.txt", []byte("quarterly numbers\n"), p); err != nil || res.Filename != "notes.txt" {
		t.Fatalf("a text file must pass untouched: %+v %v", res, err)
	}
	_, info, err := audio.Tool{}.TranscodeProcessor(audio.Transcode{Format: audio.AAC})(t.Context(), upload.Info{Filename: "a.wav", Path: "trip/a.wav", ContentType: "audio/wav"}, bytes.NewReader(ramp(t)))
	if err != nil || info.Path != "trip/a.m4a" || info.ContentType != "audio/mp4" {
		t.Fatalf("folder upload = %+v, %v", info, err)
	}
	for name, opts := range map[string]audio.Transcode{"bad format": {Format: "flac"}, "too long": {}} {
		tool := audio.Tool{}
		if name == "too long" {
			tool.MaxDuration = time.Second
		}
		if _, _, err := tool.TranscodeProcessor(opts)(t.Context(), upload.Info{ContentType: "audio/wav"}, bytes.NewReader(ramp(t))); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestInspect(t *testing.T) {
	d := newDisk(t)
	p := policy
	p.Inspect = audio.Tool{}.Inspect(audio.Limits{MaxDuration: time.Second})
	if _, err := receive(t, d, "note.wav", ramp(t), p); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("over MaxDuration: %v", err)
	}
	p.Inspect = audio.Tool{}.Inspect(audio.Limits{})
	if _, err := receive(t, d, "note.wav", ramp(t), p); err != nil {
		t.Fatalf("within limits: %v", err)
	}
	if _, err := receive(t, d, "fake.mp3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00 not really"), p); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("an unreadable audio file was stored: %v", err)
	}
	if _, err := receive(t, d, "notes.txt", []byte("hello\n"), p); err != nil {
		t.Fatalf("other types pass: %v", err)
	}
	if err := (audio.Tool{}).Inspect(audio.Limits{})(t.Context(), d, upload.Result{Key: "absent", ContentType: "audio/ogg"}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing object: %v", err)
	}
}

// TestInspect_LengthIsMeasured pins Inspect's length check against a
// lying container: one claiming 1 second over 20 seconds of packets, one
// claiming 1e22 seconds (which wrapped negative on amd64 before the
// clamp; run with GOARCH=amd64 too), and one whose packets carry no
// timestamps, all refused by a 5-second limit.
func TestInspect_LengthIsMeasured(t *testing.T) {
	d := newDisk(t)
	if _, err := d.WriteBytes(t.Context(), "a.m4a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	r := upload.Result{Key: "a.m4a", ContentType: "audio/mp4"}
	for name, tc := range map[string]struct{ claimed, packets, want string }{
		"short claim":   {"1.0", `pts_time=0|duration_time=0.5\npts_time=19.5|duration_time=0.5\n`, "longer"},
		"absurd claim":  {"9223372036854.775807", `pts_time=0|duration_time=1\n`, "longer"},
		"no timestamps": {"1.0", `pts_time=N/A\n`, "unknown length"},
	} {
		rep := `{"format":{"format_name":"mov","duration":"` + tc.claimed + `"},"streams":[{"codec_type":"audio","codec_name":"aac"}]}`
		tool := audio.Tool{FFprobe: fakeProgram(t, "ffprobe", `case "$*" in *packet=*) printf '`+tc.packets+`' ;; *) echo '`+rep+`' ;; esac`)}
		err := tool.Inspect(audio.Limits{MaxDuration: 5 * time.Second})(t.Context(), d, r)
		if filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTool_EnvReachesFFmpeg pins that Tool.Env is forwarded to the
// programs and that the parent's other variables are not.
func TestTool_EnvReachesFFmpeg(t *testing.T) {
	t.Setenv("FS_TEST_SECRET", "planted-secret")
	log := filepath.Join(t.TempDir(), "env")
	tool := audio.Tool{FFprobe: fakeProgram(t, "ffprobe", "env > "+log+"; exit 1"), Env: []string{"FS_TEST_EXTRA=passed"}}
	_, _ = tool.Probe(t.Context(), strings.NewReader("media"))
	b, err := os.ReadFile(log)
	if env := string(b); err != nil || !strings.Contains(env, "FS_TEST_EXTRA=passed") || strings.Contains(env, "planted-secret") {
		t.Fatalf("environment:\n%s\n%v", env, err)
	}
}
