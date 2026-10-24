package audio

import (
	"slices"
	"testing"
	"time"
)

// TestArgs_DefaultUnchanged pins the command line of the built-in Opus
// format, argument for argument, as it was before profiles existed.
func TestArgs_DefaultUnchanged(t *testing.T) {
	o := Transcode{Mono: true}
	f, err := o.format()
	if err != nil {
		t.Fatal(err)
	}
	got := o.args(f, []string{"-in"}, "in", "out", time.Minute)
	want := []string{"-v", "error", "-nostdin", "-in", "-i", "in", "-t", "60.000", "-map", "0:a:0", "-vn",
		"-c:a", "libopus", "-b:a", "48000", "-map_metadata", "-1", "-map_chapters", "-1", "-ac", "1"}
	if !slices.Equal(got[:len(want)], want) || got[len(got)-4] != "-f" || got[len(got)-3] != "ogg" {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}
}
