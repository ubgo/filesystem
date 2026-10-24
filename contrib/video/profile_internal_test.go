package video

import (
	"slices"
	"testing"
)

// TestArgs_DefaultUnchanged pins the command line Transcode ran before
// profiles existed, argument for argument.
func TestArgs_DefaultUnchanged(t *testing.T) {
	got := Transcode{}.args([]string{"-in"}, "in", "out")
	want := []string{"-v", "error", "-nostdin", "-in", "-i", "in", "-t", "1800.000",
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", "-fpsmax", "60", "-frames:v", "108001",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128000", "-ac", "2",
		"-map_metadata", "-1", "-map_chapters", "-1", "-movflags", "+faststart"}
	if !slices.Equal(got[:len(want)], want) || got[len(got)-4] != "-f" || got[len(got)-3] != "mp4" {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}
}
