package video_test

import (
	"bytes"
	"image/jpeg"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video"
	"github.com/ubgo/filesystem/upload"
)

// transcoded runs Transcode and reads its whole output.
func transcoded(t *testing.T, tool video.Tool, body []byte, opts video.Transcode) ([]byte, error) {
	t.Helper()
	rc, err := tool.Transcode(t.Context(), bytes.NewReader(body), opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func TestTranscode_WritesAWebMP4WithinTheBox(t *testing.T) {
	out, err := transcoded(t, video.Tool{}, rotated(t), video.Transcode{MaxWidth: 120, MaxHeight: 120})
	if err != nil {
		t.Fatal(err)
	}
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	// The upright 240x320 source fits 120x120 as 90x120, rotation applied
	// to the pixels.
	if info.VideoCodec != "h264" || info.AudioCodec != "aac" || info.Width != 90 || info.Height != 120 || info.Rotation != 0 {
		t.Fatalf("output = %+v", info)
	}
	if moov, mdat := bytes.Index(out, []byte("moov")), bytes.Index(out, []byte("mdat")); moov < 0 || moov > mdat {
		t.Fatalf("the index is not before the media (moov %d, mdat %d): +faststart missing", moov, mdat)
	}
	if strings.Contains(tags(t, out), "holiday") {
		t.Fatal("metadata survived the transcode")
	}
}

func TestTranscode_NeverUpscalesAndCapsTheFrameRate(t *testing.T) {
	out, err := transcoded(t, video.Tool{}, clip(t), video.Transcode{MaxFrameRate: 10, Preset: "ultrafast"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(out))
	if err != nil || info.Width != 320 || info.Height != 240 || info.FrameRate > 10 {
		t.Fatalf("output = %+v, %v", info, err)
	}
}

func TestTranscode_Bounds(t *testing.T) {
	body := clip(t)
	for name, tc := range map[string]struct {
		opts video.Transcode
		code filesystem.Code
	}{
		"longer than MaxDuration": {video.Transcode{MaxDuration: time.Second}, filesystem.CodeRejected},
		"over MaxOutput":          {video.Transcode{MaxOutput: 1000}, filesystem.CodeTooLarge},
		"over Timeout":            {video.Transcode{Timeout: time.Nanosecond}, filesystem.CodeUnavailable},
		"negative":                {video.Transcode{MaxWidth: -1}, filesystem.CodeInvalidArgument},
		"CRF out of range":        {video.Transcode{CRF: 52}, filesystem.CodeInvalidArgument},
		"unknown preset":          {video.Transcode{Preset: "placebo; rm"}, filesystem.CodeInvalidArgument},
	} {
		if _, err := transcoded(t, video.Tool{}, body, tc.opts); filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
	if _, err := transcoded(t, video.Tool{}, []byte("not a video"), video.Transcode{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("not a video: %v", err)
	}
}

// TestTranscode_CutsALyingContainer pins the -t cut: a container that
// claims to be short cannot make the encode run longer than MaxDuration.
func TestTranscode_CutsALyingContainer(t *testing.T) {
	short := fakeProgram(t, "ffprobe", `echo '{"format":{"duration":"0.5"},"streams":[{"codec_type":"video","codec_name":"h264","width":320,"height":240}]}'`)
	out, err := transcoded(t, video.Tool{FFprobe: short}, clip(t), video.Transcode{MaxDuration: time.Second, Preset: "ultrafast"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := video.Tool{}.Probe(t.Context(), bytes.NewReader(out))
	if err != nil || info.Duration > 1100*time.Millisecond {
		t.Fatalf("output runs %s, %v; want at most MaxDuration", info.Duration, err)
	}
}

func TestTranscodeProcessor(t *testing.T) {
	d := newDisk(t)
	p := upload.Policy{MaxSize: 50 << 20, Allowed: []string{"video/*", "text/plain"}, Process: video.Tool{}.TranscodeProcessor(video.Transcode{Preset: "ultrafast"})}
	res, err := receive(t, d, "clip.mov", rotated(t), p)
	if err != nil || res.ContentType != video.MP4ContentType || !strings.HasSuffix(res.Filename, ".mp4") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if res, err := receive(t, d, "notes.txt", []byte("quarterly numbers\n"), p); err != nil || res.Filename != "notes.txt" {
		t.Fatalf("a text file must pass untouched: %+v %v", res, err)
	}
	_, info, err := video.Tool{}.TranscodeProcessor(video.Transcode{Preset: "ultrafast"})(t.Context(), upload.Info{Filename: "clip.mov", Path: "trip/day1/clip.mov", ContentType: "video/quicktime"}, bytes.NewReader(clip(t)))
	if err != nil || info.Path != "trip/day1/clip.mp4" {
		t.Fatalf("folder upload path = %q, %v", info.Path, err)
	}
	if _, err := receive(t, d, "bad.mp4", []byte("\x00\x00\x00\x18ftypmp42 not really"), p); err == nil {
		t.Fatal("an unreadable video was stored")
	}
}

func TestStoryboard(t *testing.T) {
	body := clip(t)
	s, err := video.Tool{}.Storyboard(t.Context(), bytes.NewReader(body), video.Storyboard{Interval: 500 * time.Millisecond, Columns: 3, TileSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tiles) != 4 || s.TileWidth != 64 || s.TileHeight != 48 || s.Columns != 3 {
		t.Fatalf("layout = %d tiles %dx%d, %d columns", len(s.Tiles), s.TileWidth, s.TileHeight, s.Columns)
	}
	if last := s.Tiles[3]; last.X != 0 || last.Y != 48 || last.Start != 1500*time.Millisecond || last.End != 2*time.Second {
		t.Fatalf("last tile = %+v", last)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(s.JPEG))
	if err != nil || cfg.Width != 3*64 || cfg.Height != 2*48 {
		t.Fatalf("sprite = %+v, %v", cfg, err)
	}
	vtt, err := s.WebVTT("/media/clip/storyboard.jpg")
	if err != nil || !strings.HasPrefix(vtt, "WEBVTT\n") || !strings.Contains(vtt, "00:00:01.500 --> 00:00:02.000\n/media/clip/storyboard.jpg#xywh=0,48,64,48\n") {
		t.Fatalf("WebVTT = %q, %v", vtt, err)
	}
	if _, err := s.WebVTT("/x.jpg\n\n00:00:00.000 --> 99:00:00.000"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a URL with a line break: %v", err)
	}
}

// TestStoryboard_MaxTilesWidensTheInterval pins the bound on a sprite's
// size: a long video gets fewer tiles, never a bigger image.
func TestStoryboard_MaxTilesWidensTheInterval(t *testing.T) {
	s, err := video.Tool{}.Storyboard(t.Context(), bytes.NewReader(clip(t)), video.Storyboard{Interval: 100 * time.Millisecond, MaxTiles: 4, TileSize: 32})
	if err != nil || len(s.Tiles) != 4 || s.Tiles[1].Start != 500*time.Millisecond {
		t.Fatalf("tiles = %+v, %v", s.Tiles, err)
	}
	// A portrait video fits its height to the tile size.
	s, err = video.Tool{}.Storyboard(t.Context(), bytes.NewReader(rotated(t)), video.Storyboard{TileSize: 64})
	if err != nil || s.TileWidth != 48 || s.TileHeight != 64 || len(s.Tiles) != 1 {
		t.Fatalf("portrait = %dx%d, %d tiles, %v", s.TileWidth, s.TileHeight, len(s.Tiles), err)
	}
}

func TestStoryboard_Refuses(t *testing.T) {
	body := clip(t)
	for name, opts := range map[string]video.Storyboard{
		"tile too large": {TileSize: video.MaxTileSize + 1},
		"too many tiles": {MaxTiles: video.MaxTiles + 1},
		"negative":       {Interval: -time.Second},
	} {
		if _, err := (video.Tool{}).Storyboard(t.Context(), bytes.NewReader(body), opts); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := (video.Tool{MaxPoster: 100}).Storyboard(t.Context(), bytes.NewReader(body), video.Storyboard{}); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("over MaxPoster: %v", err)
	}
	if _, err := (video.Tool{}).Storyboard(t.Context(), strings.NewReader("not a video"), video.Storyboard{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("not a video: %v", err)
	}
	noLength := fakeProgram(t, "ffprobe", `echo '{"format":{},"streams":[{"codec_type":"video","width":2,"height":2}]}'`)
	if _, err := (video.Tool{FFprobe: noLength}).Storyboard(t.Context(), bytes.NewReader(body), video.Storyboard{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("no duration: %v", err)
	}
	silent := fakeProgram(t, "ffmpeg", "exit 0")
	if _, err := (video.Tool{FFmpeg: silent}).Storyboard(t.Context(), bytes.NewReader(body), video.Storyboard{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("no frames: %v", err)
	}
}

// TestTranscode_BoundsReachFFmpeg pins the arguments that stop FFmpeg
// itself at the bounds, rather than only checking its output afterwards:
// the duration cut, the frame cap and the output size cap.
func TestTranscode_BoundsReachFFmpeg(t *testing.T) {
	echo := fakeProgram(t, "ffmpeg", `echo "$@" > "$last"`)
	out, err := transcoded(t, video.Tool{FFmpeg: echo}, clip(t), video.Transcode{MaxDuration: 4 * time.Second, MaxFrameRate: 30, MaxOutput: 5000})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-t 4.000", "-fpsmax 30", "-frames:v 121", "-fs 5000", "-format_whitelist mov,mp4", "-protocol_whitelist file"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("ffmpeg ran without %q: %s", want, out)
		}
	}
}
