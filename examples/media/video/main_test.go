package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun uploads a generated clip and checks the report. It skips only
// when FFmpeg is not installed.
func TestRun(t *testing.T) {
	for _, p := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed", p)
		}
	}
	path := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=30:duration=3",
		"-f", "lavfi", "-i", "sine=duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var buf strings.Builder
	if err := run(t.Context(), &buf, f, "clip.mp4"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"video/mp4", "duration  3s", "size      640x360", "codecs    h264 / aac", ".poster-", "image/jpeg",
		"web copy  .web.mp4 (video/mp4)", "storyboard 6 tiles of 120x68, 3 per row, last 2.5s-3s at 240,68",
		// The 640x360 clip gets the 360p and 180p rungs; 720p would upscale.
		"hls       master.m3u8 (application/vnd.apple.mpegurl) names 360p.m3u8, 180p.m3u8",
		"hls       2 segments, then 3 playlists, the master last: true"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, buf.String())
		}
	}
}
