package audio_test

import (
	"bytes"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/audio"
	"github.com/ubgo/filesystem/upload"
)

// mp3 is an MP3 profile, which the built-in formats lack.
var mp3 = audio.Profile{Encoder: "libmp3lame", Muxer: "mp3", BitRate: 96_000, ContentType: "audio/mpeg", Extension: ".mp3"}

// TestTranscode_Profile pins the profile seam: an MP3 profile writes MP3
// without the source's title, the processor renames the upload with the
// profile's type and extension, a bad or bound-moving profile is refused
// before FFmpeg runs, and Format still works without one.
func TestTranscode_Profile(t *testing.T) {
	body := ramp(t)
	out, err := transcode(t, audio.Tool{}, body, audio.Transcode{Profile: &mp3, Mono: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := audio.Tool{}.Probe(t.Context(), bytes.NewReader(out))
	if err != nil || info.Codec != "mp3" || info.Channels != 1 || bytes.Contains(out, []byte("voice note")) {
		t.Fatalf("output = %+v, %v", info, err)
	}
	opts := audio.Transcode{Profile: &mp3}
	_, got, err := audio.Tool{}.TranscodeProcessor(opts)(t.Context(), upload.Info{Filename: "a.wav", ContentType: "audio/wav"}, bytes.NewReader(body))
	if err != nil || got.ContentType != "audio/mpeg" || got.Filename != "a.mp3" || opts.Extension() != ".mp3" {
		t.Fatalf("processor = %+v, %v", got, err)
	}
	for name, p := range map[string]audio.Profile{
		"option as encoder": {Encoder: "-i", Muxer: "mp3", ContentType: "audio/mpeg", Extension: ".mp3"},
		"video type":        {Encoder: "libmp3lame", Muxer: "mp3", ContentType: "video/mp4", Extension: ".mp3"},
		"lifts the cut":     {Encoder: "libmp3lame", Muxer: "mp3", ContentType: "audio/mpeg", Extension: ".mp3", Args: []string{"-t", "9999"}},
		"maps video":        {Encoder: "libmp3lame", Muxer: "mp3", ContentType: "audio/mpeg", Extension: ".mp3", Args: []string{"-map", "0:v"}},
	} {
		if _, err := transcode(t, audio.Tool{}, body, audio.Transcode{Profile: &p}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := transcode(t, audio.Tool{}, body, audio.Transcode{Format: audio.AAC, Profile: &mp3}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("Format with a Profile: %v", err)
	}
	if d := (audio.Transcode{}); d.ContentType() != "audio/ogg" || d.Extension() != ".ogg" {
		t.Fatalf("default output %q %q", d.ContentType(), d.Extension())
	}
	if opts.ContentType() != "audio/mpeg" || (audio.Transcode{Format: audio.AAC}).ContentType() == "audio/ogg" {
		t.Fatalf("content types %q %q", opts.ContentType(), (audio.Transcode{Format: audio.AAC}).ContentType())
	}
	// Transcode.BitRate overrides the profile's.
	lower, err := transcode(t, audio.Tool{}, body, audio.Transcode{Profile: &mp3, BitRate: 32_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(lower) >= len(out) {
		t.Errorf("32 kb/s output (%d bytes) is not smaller than 96 kb/s mono (%d bytes)", len(lower), len(out))
	}
}
