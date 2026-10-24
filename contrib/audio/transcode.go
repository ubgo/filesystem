package audio

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// Format is an output of Transcode.
type Format string

// The closed set of formats.
const (
	// Opus in an Ogg container: what WhatsApp and Telegram send voice
	// notes as; every current browser plays it.
	Opus Format = "opus"
	// AAC in an MP4 (.m4a) container with the index first: what Safari
	// before 17 and iOS's own players need.
	AAC Format = "aac"
)

// FormatValues lists every Format.
var FormatValues = []Format{Opus, AAC}

// format is how FFmpeg writes one Format.
type format struct {
	encoder, muxer, contentType, extension string
	bitRate                                int
	extra                                  []string
}

// formats maps every Format to its FFmpeg settings and default bit rate
// (speech-clear Opus, music-grade AAC).
var formats = map[Format]format{
	Opus: {encoder: "libopus", muxer: "ogg", contentType: "audio/ogg", extension: ".ogg", bitRate: 48_000},
	AAC:  {encoder: "aac", muxer: "ipod", contentType: "audio/mp4", extension: ".m4a", bitRate: 128_000, extra: []string{"-movflags", "+faststart"}},
}

// ContentType is the type of f's output ("audio/ogg"), or "" for an
// unknown format.
func (f Format) ContentType() string { return formats[f].contentType }

// Extension is the file extension of f's output (".ogg"), or "".
func (f Format) Extension() string { return formats[f].extension }

// Transcode configures Tool.Transcode.
type Transcode struct {
	// Format of the output; "" means Opus.
	Format Format
	// BitRate in bits per second; 0 means the format's default (48 kb/s
	// for Opus, 128 kb/s for AAC).
	BitRate int
	// Mono mixes down to one channel, as voice notes are.
	Mono bool
	// MaxOutput bounds the encoded bytes (CodeTooLarge beyond); 0 means
	// DefaultMaxOutput.
	MaxOutput int64
	// Profile, when set, replaces Format with another encoding: MP3 in
	// its own container, FLAC, AAC at another profile. Format must then
	// be "" (Mono and BitRate still apply; BitRate 0 is the profile's).
	// Nil encodes Format.
	Profile *Profile
}

// Profile is an output encoding for Transcode: FFmpeg's encoder and
// muxer names, their options and what the output is. A profile is the
// application's configuration, never a client's input. Transcode keeps
// its safety settings whatever it says: the duration bound, the output
// size limit, the first audio stream only, no video, metadata or
// chapters; args that would change one (-t, -map, -f and the like, see
// ffmpeg.CheckProfileArgs) are refused with CodeInvalidArgument.
type Profile struct {
	// Encoder is FFmpeg's audio encoder ("libmp3lame", "flac").
	Encoder string
	// Muxer is FFmpeg's output format ("mp3", "flac").
	Muxer string
	// BitRate in bits per second; 0 lets the encoder choose (a lossless
	// encoder takes none).
	BitRate int
	// Args are extra encoder and muxer options ("-q:a", "2").
	Args []string
	// ContentType and Extension describe the output ("audio/mpeg",
	// ".mp3").
	ContentType, Extension string
}

// Transcode re-encodes r's first audio stream in opts.Format, without
// metadata, cover art or video, and returns the output, read from a work
// file that is already unlinked; close it when done. Audio longer than
// the Tool's MaxDuration is CodeRejected, and decoding stops there
// whatever the container claims; an unknown format or a negative option
// is CodeInvalidArgument.
func (t Tool) Transcode(ctx context.Context, r io.Reader, opts Transcode) (io.ReadCloser, error) {
	f, err := opts.format()
	if err != nil {
		return nil, err
	}
	var out io.ReadCloser
	err = t.runner().WorkFile(r, func(in string) error {
		info, err := t.probePath(ctx, in)
		if err != nil {
			return err
		}
		if err := t.bounded(info); err != nil {
			return err
		}
		run := t.runner()
		file, err := run.OutputFile(opts.maxOutput(), func(dst string) error {
			_, err := run.Run(ctx, t.ffmpeg(), opts.args(f, run.InputOptions(), in, dst, t.maxDuration()), ffmpeg.MaxFileRunOutput)
			return err
		})
		out = file
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ContentType is the type of o's output: the profile's, or its
// Format's.
func (o Transcode) ContentType() string {
	if o.Profile != nil {
		return o.Profile.ContentType
	}
	return o.formatName().ContentType()
}

// Extension is the file extension of o's output, with the dot.
func (o Transcode) Extension() string {
	if o.Profile != nil {
		return o.Profile.Extension
	}
	return o.formatName().Extension()
}

// formatName is o.Format, or Opus when it is "".
func (o Transcode) formatName() Format {
	if o.Format == "" {
		return Opus
	}
	return o.Format
}

// format resolves opts.Format (or the Profile) and checks the options.
func (o Transcode) format() (format, error) {
	if p := o.Profile; p != nil {
		if o.Format != "" || o.BitRate < 0 || o.MaxOutput < 0 || p.BitRate < 0 {
			return format{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audio: a Profile with a Format, or a negative option")
		}
		if err := ffmpeg.CheckProfile([]string{p.Encoder, p.Muxer}, p.ContentType, p.Extension, "audio/"); err != nil {
			return format{}, err
		}
		if err := ffmpeg.CheckProfileArgs(p.Args); err != nil {
			return format{}, err
		}
		f := format{encoder: p.Encoder, muxer: p.Muxer, contentType: p.ContentType, extension: p.Extension, bitRate: p.BitRate, extra: p.Args}
		if o.BitRate > 0 {
			f.bitRate = o.BitRate
		}
		return f, nil
	}
	name := o.Format
	if name == "" {
		name = Opus
	}
	f, ok := formats[name]
	if !ok || o.BitRate < 0 || o.MaxOutput < 0 {
		return format{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "audio: unknown format %q or a negative option", o.Format)
	}
	if o.BitRate > 0 {
		f.bitRate = o.BitRate
	}
	return f, nil
}

func (o Transcode) maxOutput() int64 {
	if o.MaxOutput == 0 {
		return DefaultMaxOutput
	}
	return o.MaxOutput
}

// args are ffmpeg's arguments to encode in to dst as f.
func (o Transcode) args(f format, inputOptions []string, in, dst string, maxDuration time.Duration) []string {
	args := append([]string{"-v", "error", "-nostdin"}, inputOptions...)
	args = append(args, "-i", in, "-t", ffmpeg.Seconds(maxDuration), "-map", "0:a:0", "-vn", "-c:a", f.encoder)
	// A built-in format always has a bit rate; a profile's 0 leaves it to
	// the encoder.
	if f.bitRate > 0 {
		args = append(args, "-b:a", strconv.Itoa(f.bitRate))
	}
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1")
	if o.Mono {
		args = append(args, "-ac", "1")
	}
	args = append(args, f.extra...)
	args = append(args, ffmpeg.OutputLimit(o.maxOutput())...)
	return append(args, "-f", f.muxer, "-y", dst)
}
