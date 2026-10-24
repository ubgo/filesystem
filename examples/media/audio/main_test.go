package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

// needFFmpeg skips a test when FFmpeg is not installed, as the other
// media examples do.
func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, p := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed", p)
		}
	}
}

func TestRun(t *testing.T) {
	needFFmpeg(t)
	var out strings.Builder
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"stored   note.ogg (audio/ogg)", "audio    2s, opus, 48000 Hz, 1 channel(s)",
		"waveform 32 peaks, loudest 0.8", "aac copy aac in mov,mp4", "broken.wav: rejected",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestTooLong pins the duration limit: a note longer than maxNote is
// refused and nothing is recorded.
func TestTooLong(t *testing.T) {
	needFFmpeg(t)
	s, err := newStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Upload(t.Context(), scope, diskMedia, bytes.NewReader(tone(maxNote+time.Second)), "long.wav", policy)
	if filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a note over maxNote = %v, want rejected", err)
	}
}
