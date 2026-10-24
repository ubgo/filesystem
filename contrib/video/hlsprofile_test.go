package video_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
)

// hlsArgs runs Tool.HLS with fake programs and opts, and returns
// FFmpeg's arguments, one per line between newlines.
func hlsArgs(t *testing.T, opts video.HLS, silent bool) string {
	t.Helper()
	args, err := hlsArgsErr(t, opts, silent)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

// hlsArgsErr is hlsArgs returning Tool.HLS's error: the fake writes an
// MPEG-TS ladder, which an fMP4 ladder refuses as unexpected output.
func hlsArgsErr(t *testing.T, opts video.HLS, silent bool) (string, error) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "args")
	tool := video.Tool{FFprobe: hlsProbe(t, silent), FFmpeg: hlsFFmpeg(t, log, []string{"720p", "480p", "360p"}, "")}
	var w written
	hlsErr := tool.HLS(t.Context(), strings.NewReader("video"), opts, w.write)
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return "\n" + string(b), hlsErr
}

// TestHLS_Profile pins the HLS codec seam (R-I17): a Profile's encoders
// and args reach FFmpeg in place of libx264 and AAC, each rendition's
// bit rate still follows them, and without a Profile the arguments are
// libx264, AAC and MPEG-TS as before, with no key file.
func TestHLS_Profile(t *testing.T) {
	args, err := hlsArgsErr(t, video.HLS{Profile: &video.HLSProfile{
		VideoEncoder: "libx265", VideoArgs: []string{"-tag:v", "hvc1", "-x265-params", "log-level=error"}, PixelFormat: "yuv420p10le",
		AudioEncoder: "libopus", AudioArgs: []string{"-b:a", "96000"},
	}, SegmentType: video.HLSSegmentFMP4}, false)
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("an fMP4 ladder accepted MPEG-TS output: %v", err)
	}
	for _, want := range []string{
		"\n-c:v\nlibx265\n-tag:v\nhvc1\n-x265-params\nlog-level=error\n-pix_fmt\nyuv420p10le\n",
		"\n-c:a\nlibopus\n-b:a\n96000\n", "\n-b:v:0\n2800000\n", "\n-hls_segment_type\nfmp4\n", "\n-hls_fmp4_init_filename\n%v_init.mp4\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("profile arguments lack %q:%s", want, args)
		}
	}
	for _, not := range []string{"libx264", "\naac\n", "-preset"} {
		if strings.Contains(args, not) {
			t.Errorf("profile arguments hold %q:%s", not, args)
		}
	}

	plain := hlsArgs(t, video.HLS{}, false)
	for _, want := range []string{"\n-c:v\nlibx264\n-preset\n", "\n-c:a\naac\n", "\n-hls_segment_type\nmpegts\n", "_%05d.ts\n"} {
		if !strings.Contains(plain, want) {
			t.Errorf("default arguments lack %q:%s", want, plain)
		}
	}
	for _, not := range []string{"-hls_key_info_file", "-hls_fmp4_init_filename", ".m4s"} {
		if strings.Contains(plain, not) {
			t.Errorf("default arguments hold %q", not)
		}
	}
	if silent := hlsArgs(t, video.HLS{Profile: &video.HLSProfile{VideoEncoder: "libx265", AudioEncoder: "libopus"}}, true); strings.Contains(silent, "libopus") {
		t.Errorf("a silent video got an audio encoder:%s", silent)
	}
}

// TestHLS_ProfileRefusals: a profile cannot set what the ladder keeps
// (its bounds, the HLS muxer's file names, the network), cannot name
// something other than an encoder, and does not mix with the libx264
// settings; an unknown segment type and malformed encryption are
// refused too. Each is CodeInvalidArgument before anything runs.
func TestHLS_ProfileRefusals(t *testing.T) {
	key := make([]byte, video.HLSKeyBytes)
	p := func(args ...string) *video.HLSProfile {
		return &video.HLSProfile{VideoEncoder: "libx265", AudioEncoder: "aac", VideoArgs: args}
	}
	for name, opts := range map[string]video.HLS{
		"segment file name": {Profile: p("-hls_segment_filename", "/tmp/x_%d.ts")},
		"key info":          {Profile: p("-hls_key_info_file", "/etc/passwd")},
		"master":            {Profile: p("-master_pl_name", "../m.m3u8")},
		"upload":            {Profile: p("-method", "PUT")},
		"bound":             {Profile: p("-t", "99999")},
		"filter":            {Profile: p("-vf", "scale=8000:8000")},
		"encoder name":      {Profile: &video.HLSProfile{VideoEncoder: "-y", AudioEncoder: "aac"}},
		"pixel format":      {Profile: &video.HLSProfile{VideoEncoder: "libx265", AudioEncoder: "aac", PixelFormat: "../x"}},
		"preset too":        {Profile: p(), Preset: "fast"},
		"audio rate too":    {Profile: p(), AudioBitRate: 64000},
		"segment type":      {SegmentType: "webm"},
		"short key":         {Encryption: &video.HLSEncryption{Key: key[:15], KeyURI: "k"}},
		"short IV":          {Encryption: &video.HLSEncryption{Key: key, KeyURI: "k", IV: key[:8]}},
		"no key URI":        {Encryption: &video.HLSEncryption{Key: key}},
		"newline in URI":    {Encryption: &video.HLSEncryption{Key: key, KeyURI: "k\n/etc/passwd"}},
		"quote in URI":      {Encryption: &video.HLSEncryption{Key: key, KeyURI: `k",X="y`}},
		"long URI":          {Encryption: &video.HLSEncryption{Key: key, KeyURI: strings.Repeat("k", 2049)}},
	} {
		err := video.Tool{FFprobe: "/nonexistent", FFmpeg: "/nonexistent"}.HLS(t.Context(), strings.NewReader("v"), opts, (&written{}).write)
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestHLS_EncryptionKeyStaysOut pins that the key never leaves its work
// directory: %v, %#v and %+v of the options print no key byte.
func TestHLS_EncryptionKeyStaysOut(t *testing.T) {
	key := []byte("0123456789abcdef")
	opts := video.HLS{Encryption: &video.HLSEncryption{Key: key, KeyURI: "https://keys.example/k"}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if s := fmt.Sprintf(verb, opts.Encryption); strings.Contains(s, "0123456789") || strings.Contains(s, "48 49 50") {
			t.Errorf("%s printed the key: %s", verb, s)
		}
	}
}

// TestHLS_RealFFmpegFMP4 pins fMP4 segments with the real programs, on
// a one-rung ladder (where FFmpeg does not expand %v in the init file's
// name): the rendition gets its initialization file, segments are .m4s
// typed video/iso.segment, and the playlist names only files that were
// written.
func TestHLS_RealFFmpegFMP4(t *testing.T) {
	body := clip(t)
	opts := video.HLS{SegmentDuration: time.Second, Preset: "ultrafast", SegmentType: video.HLSSegmentFMP4,
		Renditions: []video.Rendition{{Name: "120p", MaxWidth: 160, MaxHeight: 120, VideoBitRate: 100_000}}}
	var w written
	if err := (video.Tool{}).HLS(t.Context(), bytes.NewReader(body), opts, w.write); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.bodies["120p_init.mp4"]; !ok {
		t.Fatalf("no initialization file: %v", w.names)
	}
	for i, name := range w.names {
		want := ""
		switch {
		case name == "120p_init.mp4":
			want = video.HLSInitContentType
		case strings.HasSuffix(name, ".m4s"):
			want = video.HLSFMP4SegmentContentType
		}
		if want != "" && w.types[i] != want {
			t.Errorf("%s typed %q, want %q", name, w.types[i], want)
		}
	}
	pl := w.bodies["120p.m3u8"]
	if !strings.Contains(pl, `#EXT-X-MAP:URI="120p_init.mp4"`) {
		t.Fatalf("playlist:\n%s", pl)
	}
	for _, line := range strings.Split(pl, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			if _, ok := w.bodies[line]; !ok || !strings.HasSuffix(line, ".m4s") {
				t.Errorf("playlist names %q", line)
			}
		}
	}
	if err := (video.Tool{}).HLS(t.Context(), bytes.NewReader(body), video.HLS{SegmentType: video.HLSSegmentFMP4,
		Encryption: &video.HLSEncryption{Key: make([]byte, video.HLSKeyBytes), KeyURI: "k"}}, w.write); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("encrypted fMP4: %v", err)
	}
}

// TestHLS_RealFFmpegAES pins AES-128 with the real programs: the
// playlists carry the key URI, the segments are not clear MPEG-TS, the
// writer never receives the key, and the key's work directory is gone
// afterwards.
func TestHLS_RealFFmpegAES(t *testing.T) {
	body := clip(t)
	key := make([]byte, video.HLSKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	opts := video.HLS{SegmentDuration: time.Second, Preset: "ultrafast",
		Encryption: &video.HLSEncryption{Key: key, KeyURI: "https://keys.example/v1/key"},
		Renditions: []video.Rendition{{Name: "120p", MaxWidth: 160, MaxHeight: 120, VideoBitRate: 100_000}}}
	var w written
	if err := (video.Tool{TempDir: tmp}).HLS(t.Context(), bytes.NewReader(body), opts, w.write); err != nil {
		t.Fatal(err)
	}
	for _, name := range w.names {
		if strings.Contains(name, "key") || bytes.Contains([]byte(w.bodies[name]), key) {
			t.Errorf("the writer received the key in %s", name)
		}
	}
	if pl := w.bodies["120p.m3u8"]; !strings.Contains(pl, `METHOD=AES-128,URI="https://keys.example/v1/key"`) {
		t.Fatalf("playlist:\n%s", pl)
	}
	if _, err := (video.Tool{Formats: []string{"mpegts"}}).Probe(t.Context(), strings.NewReader(w.bodies["120p_00000.ts"])); err == nil {
		t.Error("an encrypted segment probed as clear MPEG-TS")
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("work directories left behind: %v", left)
	}
}
