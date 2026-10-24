package video

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

// TestIsSegmentOf pins which work files count as a rendition's segments:
// <rendition>_<at least five digits>.ts, nothing else.
func TestIsSegmentOf(t *testing.T) {
	for name, want := range map[string]bool{
		"720p_00001.ts":  true,
		"720p_123456.ts": true,
		"360p_00001.ts":  false, // another rendition
		"720p_001.ts":    false, // too few digits
		"720p_00001.mp4": false, // not a segment
		"720p_0000x.ts":  false, // not a number
	} {
		if got := isSegmentOf(name, "720p", hlsSegmentExt); got != want {
			t.Errorf("isSegmentOf(%q) = %t, want %t", name, got, want)
		}
	}
}

// TestLadderAndTimeout: a video of unknown size gets every rung, and a set
// Timeout replaces the default.
func TestLadderAndTimeout(t *testing.T) {
	o := HLS{}
	if got, all := o.ladder(Info{}), o.renditions(); len(got) != len(all) || len(all) == 0 {
		t.Errorf("unknown size: %d rungs, want all %d", len(got), len(all))
	}
	if got := (HLS{Timeout: time.Minute}).timeout(); got != time.Minute {
		t.Errorf("timeout = %s, want 1m", got)
	}
	if got := o.timeout(); got != DefaultTranscodeTimeout {
		t.Errorf("default timeout = %s", got)
	}
}

// TestFilesFMP4 pins the fMP4 output check: a ladder's directory with its
// segments, playlist, master and initialization file is listed in writing
// order (initialization and segments first, the master last); one without
// the initialization file, or with MPEG-TS segments, is refused.
func TestFilesFMP4(t *testing.T) {
	o, ladder := HLS{SegmentType: HLSSegmentFMP4}, []Rendition{{Name: "a"}}
	write := func(dir string, names ...string) {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir := t.TempDir()
	write(dir, "a_00000.m4s", "a.m3u8", HLSMaster, "a_init.mp4")
	got, err := o.files(dir, ladder)
	if err != nil || !slices.Equal(got, []string{"a_00000.m4s", "a_init.mp4", "a.m3u8", HLSMaster}) {
		t.Fatalf("files = %v, %v", got, err)
	}
	noInit := t.TempDir()
	write(noInit, "a_00000.m4s", "a.m3u8", HLSMaster)
	if _, err := o.files(noInit, ladder); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("no initialization file: %v", err)
	}
	ts := t.TempDir()
	write(ts, "a_00000.ts", "a.m3u8", HLSMaster, "a_init.mp4")
	if _, err := o.files(ts, ladder); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("MPEG-TS segments in an fMP4 ladder: %v", err)
	}
}
