package video_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// requirePrograms skips the test when FFmpeg is not installed: the only
// reason a test of this package may skip.
func requirePrograms(t *testing.T) {
	t.Helper()
	for _, p := range []string{video.DefaultFFmpeg, video.DefaultFFprobe} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed (brew install ffmpeg, apt install ffmpeg)", p)
		}
	}
}

// ffmpeg runs ffmpeg to build a test input.
func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), video.DefaultFFmpeg, append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// clipSeconds is the length of the test clip.
const clipSeconds = 2

// clip returns a 2-second 320x240 H.264/AAC MP4 at 25 fps carrying a
// title and a recording location, the metadata Sanitize must remove.
func clip(t *testing.T) []byte {
	t.Helper()
	requirePrograms(t)
	path := filepath.Join(t.TempDir(), "clip.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2", "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
		"-metadata", "title=holiday", "-metadata", "location=+48.8577+002.2950/", path)
	return readFile(t, path)
}

// rotated returns the clip stored sideways with a 90-degree display
// rotation, as phones record portrait video.
func rotated(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.mp4"), filepath.Join(dir, "rotated.mp4")
	if err := os.WriteFile(in, clip(t), 0o600); err != nil {
		t.Fatal(err)
	}
	ffmpeg(t, "-display_rotation", "90", "-i", in, "-c", "copy", out)
	return readFile(t, out)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestProbe(t *testing.T) {
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(clip(t)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration != clipSeconds*time.Second || info.Width != 320 || info.Height != 240 || info.VideoCodec != "h264" || info.AudioCodec != "aac" || info.FrameRate != 25 || info.Rotation != 0 || info.BitRate <= 0 || !strings.Contains(info.Format, "mp4") {
		t.Fatalf("info = %+v", info)
	}
}

func TestProbe_ReportsTheDisplaySize(t *testing.T) {
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(rotated(t)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 240 || info.Height != 320 || info.Rotation%180 != 90 {
		t.Fatalf("rotated = %dx%d rotation %d, want 240x320", info.Width, info.Height, info.Rotation)
	}
}

func TestProbe_Refuses(t *testing.T) {
	requirePrograms(t)
	audio := filepath.Join(t.TempDir(), "tone.m4a")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac", audio)
	// The playlists name a real video elsewhere on the machine: a probe
	// that followed them would report it, so only the demuxer and
	// protocol limits make them fail.
	secret := filepath.Join(t.TempDir(), "elsewhere.mp4")
	if err := os.WriteFile(secret, clip(t), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"text":                  []byte("quarterly numbers\n"),
		"audio only":            readFile(t, audio),
		"playlist named as mp4": []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nfile://" + secret + "\n#EXT-X-ENDLIST\n"),
		"concat script":         []byte("ffconcat version 1.0\nfile '" + secret + "'\n"),
	} {
		if _, err := (video.Tool{}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s: err = %v, want rejected", name, err)
		}
	}
}

func TestTool_Bounds(t *testing.T) {
	body := clip(t)
	if _, err := (video.Tool{MaxInput: 100}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("MaxInput: %v", err)
	}
	if _, err := (video.Tool{Timeout: time.Nanosecond}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("Timeout: %v", err)
	}
	if _, err := (video.Tool{MaxPoster: 100}).Poster(t.Context(), bytes.NewReader(body), 0); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("MaxPoster: %v", err)
	}
	if _, err := (video.Tool{Formats: []string{"matroska"}}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("Formats without mp4: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (video.Tool{}).Probe(ctx, bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("cancelled: %v", err)
	}
}

func TestTool_MissingProgram(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ffprobe")
	_, err := video.Tool{FFprobe: missing}.Probe(t.Context(), strings.NewReader("x"))
	if !errors.Is(err, video.ErrNoProgram) || filesystem.CodeOf(err) != filesystem.CodeUnavailable || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v", err)
	}
	_, err = video.Tool{FFmpeg: missing}.Poster(t.Context(), strings.NewReader("x"), 0)
	if !errors.Is(err, video.ErrNoProgram) {
		t.Fatalf("poster err = %v", err)
	}
}

func TestPoster(t *testing.T) {
	body := clip(t)
	for _, at := range []time.Duration{0, time.Second, time.Hour} {
		jpg, err := video.Tool{}.Poster(t.Context(), bytes.NewReader(body), at)
		if err != nil || !bytes.HasPrefix(jpg, []byte{0xFF, 0xD8, 0xFF}) {
			t.Errorf("at %s: %d bytes, %v", at, len(jpg), err)
		}
	}
}

// receive stores body under a policy accepting video and text.
func receive(t *testing.T, d *filesystem.Disk, name string, body []byte, p upload.Policy) (upload.Result, error) {
	t.Helper()
	p.MaxSize = 50 << 20
	p.Allowed = []string{"video/*", "text/plain"}
	return upload.Receive(t.Context(), d, bytes.NewReader(body), name, p)
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInspect(t *testing.T) {
	body := clip(t)
	d := newDisk(t)
	short := upload.Policy{Inspect: video.Tool{}.Inspect(video.Limits{MaxDuration: time.Second})}
	if _, err := receive(t, d, "clip.mp4", body, short); filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), "longer") {
		t.Fatalf("too long: %v", err)
	}
	small := upload.Policy{Inspect: video.Tool{}.Inspect(video.Limits{MaxWidth: 160, MaxHeight: 120})}
	if _, err := receive(t, d, "clip.mp4", body, small); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("too large: %v", err)
	}
	ok := upload.Policy{Inspect: video.Tool{}.Inspect(video.Limits{MaxDuration: time.Minute, MaxWidth: 1920, MaxHeight: 1080})}
	if _, err := receive(t, d, "clip.mp4", body, ok); err != nil {
		t.Fatal(err)
	}
	if _, err := receive(t, d, "notes.txt", []byte("quarterly numbers\n"), ok); err != nil {
		t.Fatalf("a text file must pass untouched: %v", err)
	}
	n := 0
	for _, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d objects stored, want the accepted video and the text file", n)
	}
}

// tags returns ffprobe's container and stream tags of body, flattened.
func tags(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.mp4")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), video.DefaultFFprobe, "-v", "error", "-show_entries", "format_tags:stream_tags", "-of", "flat", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSanitize_RemovesTheMetadata(t *testing.T) {
	body := clip(t)
	if before := tags(t, body); !strings.Contains(before, "holiday") || !strings.Contains(before, "48.8577") {
		t.Fatalf("the test clip lacks its metadata: %s", before)
	}
	d := newDisk(t)
	res, err := receive(t, d, "clip.mp4", body, upload.Policy{Process: video.Tool{}.Sanitize()})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := d.ReadAll(t.Context(), res.Key)
	if err != nil {
		t.Fatal(err)
	}
	if after := tags(t, stored); strings.Contains(after, "holiday") || strings.Contains(after, "48.8577") {
		t.Fatalf("metadata left after Sanitize: %s", after)
	}
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(stored))
	if err != nil || info.Duration != clipSeconds*time.Second || info.AudioCodec != "aac" || res.ContentType != "video/mp4" {
		t.Fatalf("sanitized video = %+v %s, %v", info, res.ContentType, err)
	}
	if res, err := receive(t, d, "notes.txt", []byte("quarterly numbers\n"), upload.Policy{Process: video.Tool{}.Sanitize()}); err != nil || res.Size != 18 {
		t.Fatalf("a text file must pass untouched: %+v %v", res, err)
	}
}

// TestProgramError_KeepsServerPathsOutOfTheClientMessage pins that a
// rejected file's client-visible text names no work file, while the
// program's output stays reachable for logs.
func TestProgramError_KeepsServerPathsOutOfTheClientMessage(t *testing.T) {
	requirePrograms(t)
	_, err := video.Tool{}.Probe(t.Context(), strings.NewReader("not a video"))
	var pe *video.ProgramError
	if !errors.As(err, &pe) || pe.Detail == "" {
		t.Fatalf("err = %v", err)
	}
	if msg := fshttp.ClientMessage(err); strings.Contains(msg, os.TempDir()) || strings.Contains(msg, "video-") || msg != pe.Error() {
		t.Fatalf("client message %q names the work file", msg)
	}
}

// lyingClip returns a 20-second MP4 whose headers (mvhd, every tkhd and
// mdhd) were patched to claim 1 second, as a client would to slip a long
// video past a length limit; its packets still span 20 seconds.
func lyingClip(t *testing.T) []byte {
	t.Helper()
	requirePrograms(t)
	path := filepath.Join(t.TempDir(), "long.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc=size=32x24:rate=5:duration=20", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-an", path)
	b := readFile(t, path)
	// Offsets after the box type, for version 0 boxes: mvhd and mdhd hold
	// version/flags, creation, modification, timescale, duration; tkhd
	// holds version/flags, creation, modification, track ID, reserved,
	// duration.
	for _, box := range []string{"mvhd", "mdhd", "tkhd"} {
		for i := bytes.Index(b, []byte(box)); i >= 0; i = nextBox(b, box, i) {
			if b[i+4] != 0 {
				t.Fatalf("%s is not version 0", box)
			}
			if box == "tkhd" {
				binary.BigEndian.PutUint32(b[i+24:], 1)
				continue
			}
			copy(b[i+20:i+24], b[i+16:i+20]) // duration = timescale: 1 second
		}
	}
	return b
}

func nextBox(b []byte, box string, after int) int {
	j := bytes.Index(b[after+4:], []byte(box))
	if j < 0 {
		return -1
	}
	return after + 4 + j
}

// TestInspect_MeasuresThePackets pins the measured length: a video whose
// headers claim 1 second over 20 seconds of packets is refused by a
// 5-second limit, while Probe still reports the claim.
func TestInspect_MeasuresThePackets(t *testing.T) {
	body := lyingClip(t)
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(body))
	if err != nil || info.Duration != time.Second {
		t.Fatalf("Probe = %v, %v; the patch did not take", info.Duration, err)
	}
	d := newDisk(t)
	short := upload.Policy{Inspect: video.Tool{}.Inspect(video.Limits{MaxDuration: 5 * time.Second})}
	if _, err := receive(t, d, "long.mp4", body, short); filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), "longer") {
		t.Fatalf("a 20-second video claiming 1 second passed a 5-second limit: %v", err)
	}
	roomy := upload.Policy{Inspect: video.Tool{}.Inspect(video.Limits{MaxDuration: 30 * time.Second})}
	if _, err := receive(t, d, "long.mp4", body, roomy); err != nil {
		t.Fatal(err)
	}
}
