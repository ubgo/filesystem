package ffmpeg_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// TestCheckProfile pins the profile name checks on their own: names that
// could be read as options, a malformed extension and a content type
// outside the prefixes are refused; a well-formed profile passes.
func TestCheckProfile(t *testing.T) {
	if err := ffmpeg.CheckProfile([]string{"libvpx-vp9", "libopus", "webm", "yuv420p"}, "video/webm", ".webm", "video/"); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	for name, c := range map[string]struct {
		names    []string
		ct, ext  string
		prefixes []string
	}{
		"option as name":   {[]string{"-i"}, "video/webm", ".webm", []string{"video/"}},
		"bad extension":    {[]string{"webm"}, "video/webm", "webm", []string{"video/"}},
		"path extension":   {[]string{"webm"}, "video/webm", "../x", []string{"video/"}},
		"other prefix":     {[]string{"mp3"}, "audio/mpeg", ".mp3", []string{"video/"}},
		"no media type":    {[]string{"mp3"}, ";;", ".mp3", []string{"audio/"}},
		"several prefixes": {[]string{"mp3"}, "text/html", ".mp3", []string{"audio/", "video/"}},
	} {
		if err := ffmpeg.CheckProfile(c.names, c.ct, c.ext, c.prefixes...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestCheckProfileArgs pins that every option a transcode keeps for itself
// is refused, with or without a stream specifier, in any of the lists,
// and that other options pass.
func TestCheckProfileArgs(t *testing.T) {
	if err := ffmpeg.CheckProfileArgs([]string{"-crf", "30", "-b:v", "0"}, []string{"-b:a", "64000"}); err != nil {
		t.Fatalf("plain args: %v", err)
	}
	for _, bad := range [][]string{{"-i", "x"}, {"-filter:v:0", "scale=1:1"}, {"-t", "9"}, {"-map", "0:v"}, {"-y"}} {
		if err := ffmpeg.CheckProfileArgs([]string{"-crf", "30"}, bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%v: %v", bad, err)
		}
	}
}
