package video

import (
	"context"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
	"github.com/ubgo/filesystem/upload"
)

// Defaults of Transcode, used when a field is zero. They suit a web
// player on a phone or a laptop; an app with other needs sets the fields.
const (
	// DefaultMaxWidth and DefaultMaxHeight: 1080p.
	DefaultMaxWidth  = 1920
	DefaultMaxHeight = 1080
	// DefaultMaxDuration refuses longer inputs.
	DefaultMaxDuration = 30 * time.Minute
	// DefaultMaxFrameRate caps the output's frames per second.
	DefaultMaxFrameRate = 60
	// DefaultMaxOutput bounds the encoded bytes.
	DefaultMaxOutput int64 = 2 << 30
	// DefaultTranscodeTimeout bounds the encode: an encode runs for a
	// fraction of the video's length on a server core, far more than
	// Tool.Timeout's metadata reads.
	DefaultTranscodeTimeout = 30 * time.Minute
	// DefaultCRF is libx264's default quality.
	DefaultCRF = 23
	// DefaultPreset trades some size for speed, as upload pipelines do.
	DefaultPreset = "veryfast"
	// DefaultAudioBitRate for the AAC track, in bits per second.
	DefaultAudioBitRate = 128_000
)

// defaultPixelFormat is the pixel format every browser plays.
const defaultPixelFormat = "yuv420p"

// maxCRF is the worst quality libx264 accepts.
const maxCRF = 51

// Presets are libx264's presets, fastest first: the closed set
// Transcode.Preset takes.
var Presets = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"}

// Web MP4 output: what Transcode writes.
const (
	MP4ContentType = "video/mp4"
	mp4Extension   = ".mp4"
)

// Transcode configures Tool.Transcode. The zero value uses the defaults.
type Transcode struct {
	// MaxWidth and MaxHeight bound the display size: a larger video is
	// scaled down to fit, keeping its aspect ratio; a smaller one is never
	// scaled up. 0 means DefaultMaxWidth and DefaultMaxHeight.
	MaxWidth, MaxHeight int
	// MaxDuration refuses a longer input (CodeRejected), and the output is
	// cut there whatever the container claimed, so a file lying about its
	// length cannot run longer. 0 means DefaultMaxDuration.
	MaxDuration time.Duration
	// MaxFrameRate caps the output's frames per second; with MaxDuration
	// it bounds the frame count. 0 means DefaultMaxFrameRate.
	MaxFrameRate int
	// MaxOutput bounds the encoded bytes (CodeTooLarge beyond); 0 means
	// DefaultMaxOutput.
	MaxOutput int64
	// Timeout bounds the encode (CodeUnavailable beyond); 0 means
	// DefaultTranscodeTimeout. The Tool's Timeout still bounds the probe.
	Timeout time.Duration
	// CRF is libx264's quality, 1 (best) to 51; 0 means DefaultCRF.
	CRF int
	// Preset is one of Presets; "" means DefaultPreset.
	Preset string
	// AudioBitRate in bits per second; 0 means DefaultAudioBitRate.
	AudioBitRate int
	// Profile, when set, replaces the H.264 and AAC in MP4 output with
	// other codecs and another container: VP9 or AV1 in WebM, HEVC, a
	// hardware encoder. CRF, Preset and AudioBitRate are libx264 and AAC
	// settings and must then be zero (the profile's args carry its own).
	// Nil writes the web MP4 described on Tool.Transcode.
	Profile *Profile
}

// Profile is an output encoding for Transcode: FFmpeg's encoder names
// and options, its muxer, and what the output is. A profile is the
// application's configuration, never a client's input: its args go to
// FFmpeg's command line as they are (each its own argument, no shell).
// Whatever it says, Transcode keeps the safety settings: the duration,
// size, frame-rate and frame-count bounds, the output size limit, the
// first video and audio stream only, and no metadata or chapters: args
// that would change one of them (-t, -vf, -map, -f and the like, see
// ffmpeg.CheckProfileArgs) are refused with CodeInvalidArgument.
//
// One profile writes one file, so HLS or DASH (a playlist and segments)
// is out of its reach: Tool.HLS writes an HLS ladder, and ffmpeg.Runner
// runs anything else (DASH).
type Profile struct {
	// VideoEncoder is FFmpeg's video encoder name: "libvpx-vp9",
	// "libsvtav1", "libx265", "h264_videotoolbox".
	VideoEncoder string
	// VideoArgs are the video encoder's options ("-crf", "32", "-b:v",
	// "0"), placed after it.
	VideoArgs []string
	// PixelFormat is the output's pixel format; "" means yuv420p, which
	// every browser plays.
	PixelFormat string
	// AudioEncoder is FFmpeg's audio encoder ("libopus", "aac").
	AudioEncoder string
	// AudioArgs are the audio encoder's options ("-b:a", "96000").
	AudioArgs []string
	// Muxer is FFmpeg's output format ("webm", "mp4", "mov").
	Muxer string
	// MuxerArgs are the muxer's options ("-movflags", "+faststart").
	MuxerArgs []string
	// ContentType and Extension describe the output ("video/webm",
	// ".webm"); TranscodeProcessor renames uploads with them.
	ContentType, Extension string
}

// H264Profile is the profile Transcode uses without one, for CRF, preset
// and audio bit rate: libx264 and AAC in an MP4 with the index first. It
// is exported as the starting point for a variation (a hardware H.264
// encoder with the same container).
func H264Profile(crf int, preset string, audioBitRate int) Profile {
	return Profile{
		VideoEncoder: "libx264", VideoArgs: []string{"-preset", preset, "-crf", strconv.Itoa(crf)},
		AudioEncoder: "aac", AudioArgs: []string{"-b:a", strconv.Itoa(audioBitRate), "-ac", "2"},
		Muxer: "mp4", MuxerArgs: []string{"-movflags", "+faststart"},
		ContentType: MP4ContentType, Extension: mp4Extension,
	}
}

// profile is the profile o encodes with.
func (o Transcode) profile() Profile {
	if o.Profile != nil {
		return *o.Profile
	}
	return H264Profile(orDefault(o.CRF, DefaultCRF), o.preset(), orDefault(o.AudioBitRate, DefaultAudioBitRate))
}

// ContentType is the type of o's output: the profile's, or
// MP4ContentType.
func (o Transcode) ContentType() string { return o.profile().ContentType }

// Extension is the file extension of o's output, with the dot.
func (o Transcode) Extension() string { return o.profile().Extension }

// Transcode re-encodes r as a web MP4 (or in opts.Profile): H.264 video (yuv420p, playable by
// every browser) and AAC audio, the index at the front so playback starts
// before the download ends (+faststart), no metadata or chapters, and the
// display rotation applied to the pixels. It returns the output, read
// from a work file that is already unlinked; close it when done.
//
// A file that is not a readable video, or longer than MaxDuration, is
// CodeRejected; options outside their range are CodeInvalidArgument.
// Encoding is slow: run it from a job (contrib/jobs or the app's queue)
// for anything longer than a short clip, or as TranscodeProcessor on an
// upload policy for short ones.
func (t Tool) Transcode(ctx context.Context, r io.Reader, opts Transcode) (io.ReadCloser, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	var out io.ReadCloser
	err := t.withWorkFile(r, func(in string) error {
		info, err := t.probePath(ctx, in)
		if err != nil {
			return err
		}
		if info.Duration > opts.maxDuration() {
			return filesystem.Errorf(filesystem.CodeRejected, "video is longer than %s", opts.maxDuration())
		}
		run := t.runner()
		run.Timeout = opts.timeout()
		f, err := run.OutputFile(opts.maxOutput(), func(dst string) error {
			_, err := run.Run(ctx, t.program(t.FFmpeg, DefaultFFmpeg), opts.args(t.inputOptions(), in, dst), ffmpeg.MaxFileRunOutput)
			return err
		})
		out = f
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TranscodeProcessor returns an upload.Processor that transcodes every
// video upload to a web MP4 (Transcode) and renames it ".mp4" (or to
// opts.Profile, renamed with its Extension and typed ContentType); other
// types pass untouched. It runs while the client waits, so it suits
// short clips (a chat's video messages); longer videos belong in a job.
func (t Tool) TranscodeProcessor(opts Transcode) upload.Processor {
	return func(ctx context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		if !IsVideo(in.ContentType) {
			return body, in, nil
		}
		rc, err := t.Transcode(ctx, body, opts)
		if err != nil {
			return nil, upload.Info{}, err
		}
		in.ContentType = opts.ContentType()
		in.Filename = withExtension(in.Filename, opts.Extension())
		if in.Path != "" {
			in.Path = withExtension(in.Path, opts.Extension())
		}
		return ffmpeg.CloseAtEOF(rc), in, nil
	}
}

// withExtension replaces name's extension with ext.
func withExtension(name, ext string) string {
	return strings.TrimSuffix(name, path.Ext(name)) + ext
}

// check refuses options outside their range.
func (o Transcode) check() error {
	switch {
	case o.MaxWidth < 0, o.MaxHeight < 0, o.MaxDuration < 0, o.MaxFrameRate < 0, o.MaxOutput < 0, o.Timeout < 0, o.AudioBitRate < 0:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: negative transcode option")
	case o.CRF < 0 || o.CRF > maxCRF:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: CRF %d is not in 1..%d", o.CRF, maxCRF)
	case o.Preset != "" && !slices.Contains(Presets, o.Preset):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: unknown preset %q", o.Preset)
	}
	if p := o.Profile; p != nil {
		if o.CRF != 0 || o.Preset != "" || o.AudioBitRate != 0 {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: CRF, Preset and AudioBitRate are not used with a Profile; put them in its args")
		}
		names := []string{p.VideoEncoder, p.AudioEncoder, p.Muxer}
		if p.PixelFormat != "" {
			names = append(names, p.PixelFormat)
		}
		if err := ffmpeg.CheckProfile(names, p.ContentType, p.Extension, "video/"); err != nil {
			return err
		}
		if err := ffmpeg.CheckProfileArgs(p.VideoArgs, p.AudioArgs, p.MuxerArgs); err != nil {
			return err
		}
	}
	return nil
}

// args are ffmpeg's arguments to encode in to dst.
func (o Transcode) args(inputOptions []string, in, dst string) []string {
	w, h := orDefault(o.MaxWidth, DefaultMaxWidth), orDefault(o.MaxHeight, DefaultMaxHeight)
	fps := orDefault(o.MaxFrameRate, DefaultMaxFrameRate)
	frames := int64(o.maxDuration().Seconds()*float64(fps)) + 1
	// Fit inside w×h without upscaling; libx264 with yuv420p needs even
	// sides.
	scale := "scale=w='min(" + strconv.Itoa(w) + ",iw)':h='min(" + strconv.Itoa(h) + ",ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
	p := o.profile()
	pixFmt := p.PixelFormat
	if pixFmt == "" {
		pixFmt = defaultPixelFormat
	}
	args := append([]string{"-v", "error", "-nostdin"}, inputOptions...)
	args = append(args, "-i", in,
		"-t", ffmpeg.Seconds(o.maxDuration()),
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", scale, "-fpsmax", strconv.Itoa(fps), "-frames:v", strconv.FormatInt(frames, 10),
		"-c:v", p.VideoEncoder)
	args = append(args, p.VideoArgs...)
	args = append(args, "-pix_fmt", pixFmt, "-c:a", p.AudioEncoder)
	args = append(args, p.AudioArgs...)
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1")
	args = append(args, p.MuxerArgs...)
	// The output limit comes after every profile arg, and check refused
	// profile args that would move the other bounds.
	args = append(args, ffmpeg.OutputLimit(o.maxOutput())...)
	return append(args, "-f", p.Muxer, "-y", dst)
}

func (o Transcode) maxDuration() time.Duration {
	if o.MaxDuration == 0 {
		return DefaultMaxDuration
	}
	return o.MaxDuration
}

func (o Transcode) preset() string {
	if o.Preset == "" {
		return DefaultPreset
	}
	return o.Preset
}

func (o Transcode) maxOutput() int64 {
	if o.MaxOutput == 0 {
		return DefaultMaxOutput
	}
	return o.MaxOutput
}

func (o Transcode) timeout() time.Duration {
	if o.Timeout == 0 {
		return DefaultTranscodeTimeout
	}
	return o.Timeout
}

// orDefault returns v, or def when v is zero.
func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
