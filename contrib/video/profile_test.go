package video_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/upload"
)

// vp9 is a WebM profile: VP9 video, Opus audio.
var vp9 = video.Profile{
	VideoEncoder: "libvpx-vp9", VideoArgs: []string{"-crf", "40", "-b:v", "0", "-deadline", "realtime", "-cpu-used", "8"},
	AudioEncoder: "libopus", AudioArgs: []string{"-b:a", "64000"},
	Muxer: "webm", ContentType: "video/webm", Extension: ".webm",
}

// TestTranscode_Profile pins the profile seam: a VP9 profile writes a
// WebM with its codecs inside the same box, and the processor renames
// the upload with the profile's type and extension; a profile that
// names a bad encoder, type or extension, mixes in the libx264 fields or
// sets a bound of its own is refused before FFmpeg runs; and without one
// the output is still H.264 in MP4.
func TestTranscode_Profile(t *testing.T) {
	opts := video.Transcode{MaxWidth: 160, MaxHeight: 160, Profile: &vp9}
	out, err := transcoded(t, video.Tool{}, clip(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(out))
	if err != nil || info.VideoCodec != "vp9" || info.AudioCodec != "opus" || !strings.Contains(info.Format, "webm") || info.Width > 160 || info.Height > 160 {
		t.Fatalf("output = %+v, %v", info, err)
	}
	_, got, err := video.Tool{}.TranscodeProcessor(opts)(t.Context(), upload.Info{Filename: "a.mov", ContentType: "video/quicktime"}, bytes.NewReader(clip(t)))
	if err != nil || got.ContentType != "video/webm" || got.Filename != "a.webm" {
		t.Fatalf("processor = %+v, %v", got, err)
	}

	bad := func(mut func(p *video.Profile)) video.Transcode {
		p := vp9
		p.VideoArgs = append([]string(nil), vp9.VideoArgs...)
		mut(&p)
		return video.Transcode{Profile: &p}
	}
	for name, o := range map[string]video.Transcode{
		"option as encoder": bad(func(p *video.Profile) { p.VideoEncoder = "-i" }),
		"html type":         bad(func(p *video.Profile) { p.ContentType = "text/html" }),
		"path extension":    bad(func(p *video.Profile) { p.Extension = "../x" }),
		"lifts the cut":     bad(func(p *video.Profile) { p.VideoArgs = append(p.VideoArgs, "-t", "99999") }),
		"own filter":        bad(func(p *video.Profile) { p.VideoArgs = append(p.VideoArgs, "-filter:v", "scale=8000:8000") }),
		"second input":      bad(func(p *video.Profile) { p.AudioArgs = []string{"-i", "/etc/passwd"} }),
		"with a CRF":        {CRF: 20, Profile: &vp9},
		"bad pixel format":  bad(func(p *video.Profile) { p.PixelFormat = "-vf" }),
	} {
		if _, err := transcoded(t, video.Tool{}, clip(t), o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A pixel format the profile names is checked and passed on.
	yuv := vp9
	yuv.PixelFormat = "yuv420p"
	if _, err := transcoded(t, video.Tool{}, clip(t), video.Transcode{MaxWidth: 160, MaxHeight: 160, Profile: &yuv}); err != nil {
		t.Fatalf("profile with a pixel format: %v", err)
	}
	if d := (video.Transcode{}); d.ContentType() != video.MP4ContentType || d.Extension() != ".mp4" {
		t.Fatalf("default output %q %q", d.ContentType(), d.Extension())
	}
}
