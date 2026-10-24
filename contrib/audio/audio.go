// Package audio reads and converts uploaded audio with FFmpeg's programs:
// ffprobe for the duration, codec, bit rate, sample rate and channels;
// ffmpeg for waveform peaks (the bars a voice-note or podcast player
// draws) and for transcoding to Opus or AAC, as chat apps do with voice
// notes. On top of that: an upload.Inspector that refuses unreadable or
// over-long audio, an upload.Processor that transcodes on upload, and a
// records plugin that stores the metadata, and optionally the waveform,
// with every audio file.
//
// Everything goes through a Tool, whose zero value is usable. Every run
// is bounded by contrib/video/ffmpeg's Runner: the input copied to a work
// file is capped (MaxInput), each run is killed after Timeout, output is
// capped, and FFmpeg may open only the demuxers in Formats and the "file"
// protocol. Decoding is also bounded by MaxDuration, whatever the
// container claims.
package audio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// Defaults used when a Tool leaves a field zero.
const (
	// DefaultTimeout bounds one program run; a transcode or waveform of
	// a long recording needs more than a probe, so it is generous.
	DefaultTimeout = 5 * time.Minute
	// DefaultMaxInput bounds the bytes copied to the work file.
	DefaultMaxInput int64 = 500 << 20
	// DefaultMaxDuration refuses longer audio for Waveform and Transcode
	// (CodeRejected), and cuts decoding there.
	DefaultMaxDuration = 3 * time.Hour
	// DefaultMaxOutput bounds a transcode's encoded bytes.
	DefaultMaxOutput int64 = 500 << 20
)

// DefaultFormats are the demuxers the programs may use: the containers
// browsers, phones and voice recorders write. Playlists (HLS, concat) are
// absent, since they can name other files and URLs.
var DefaultFormats = []string{"mp3", "ogg", "wav", "flac", "mov", "mp4", "m4a", "3gp", "3g2", "mj2", "matroska", "webm", "aac", "caf", "aiff", "amr"}

// ErrNoProgram is in the chain of the CodeUnavailable error of a run
// whose program is not installed.
var ErrNoProgram = ffmpeg.ErrNoProgram

// ProgramError is the cause of a CodeRejected run: its text ("audio:
// ffprobe could not read the file") is safe for clients, Detail is the
// program's output for logs.
type ProgramError = ffmpeg.ProgramError

// packageName prefixes the messages of the package's errors.
const packageName = "audio"

// maxProbeOutput bounds ffprobe's JSON; a real report is a few KiB.
const maxProbeOutput = 1 << 20

// Tool runs ffprobe and ffmpeg on audio. The zero value uses the
// defaults.
type Tool struct {
	// FFprobe and FFmpeg are the programs' paths or names; empty means
	// "ffprobe" and "ffmpeg" on PATH.
	FFprobe, FFmpeg string
	// Timeout bounds each program run; 0 means DefaultTimeout.
	Timeout time.Duration
	// MaxInput bounds the bytes copied to the work file; 0 means
	// DefaultMaxInput. More is CodeTooLarge.
	MaxInput int64
	// MaxDuration refuses longer audio in Waveform and Transcode; 0
	// means DefaultMaxDuration.
	MaxDuration time.Duration
	// Formats are the demuxers FFmpeg may use; nil means DefaultFormats.
	Formats []string
	// TempDir is where work files go; empty means os.TempDir().
	TempDir string
	// Env adds variables to FFmpeg's environment, as "KEY=value"
	// (FONTCONFIG_FILE, a library path), forwarded to ffmpeg.Runner.Env.
	// FFmpeg otherwise sees only PATH, HOME, TMPDIR, TMP, LANG and LC_ALL
	// of this process, so the application's secrets never reach it.
	Env []string
}

// Info is what ffprobe reports about audio.
type Info struct {
	// Format is the container as ffprobe names it ("ogg", "mp3").
	Format string
	// Codec of the first audio stream ("opus", "aac", "mp3").
	Codec string
	// Duration the container claims, which a file can lie about
	// (Inspect's Limits measure the packets too); 0 when unknown, and at
	// most the longest time.Duration.
	Duration time.Duration
	// BitRate in bits per second, 0 when unknown.
	BitRate int64
	// SampleRate in Hz and Channels of the first audio stream.
	SampleRate, Channels int
}

// Probe reports what r holds. A file ffprobe cannot read, or one without
// an audio stream, is CodeRejected.
func (t Tool) Probe(ctx context.Context, r io.Reader) (Info, error) {
	var info Info
	err := t.runner().WorkFile(r, func(path string) error {
		var err error
		info, err = t.probePath(ctx, path)
		return err
	})
	return info, err
}

// runner is the bounded program runner t's fields configure.
func (t Tool) runner() ffmpeg.Runner {
	formats := t.Formats
	if formats == nil {
		formats = DefaultFormats
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxInput := t.MaxInput
	if maxInput <= 0 {
		maxInput = DefaultMaxInput
	}
	return ffmpeg.Runner{Package: packageName, Formats: formats, Timeout: timeout, MaxInput: maxInput, TempDir: t.TempDir, Env: t.Env}
}

func (t Tool) ffprobe() string {
	if t.FFprobe == "" {
		return ffmpeg.FFprobe
	}
	return t.FFprobe
}

func (t Tool) ffmpeg() string {
	if t.FFmpeg == "" {
		return ffmpeg.FFmpeg
	}
	return t.FFmpeg
}

func (t Tool) maxDuration() time.Duration {
	if t.MaxDuration <= 0 {
		return DefaultMaxDuration
	}
	return t.MaxDuration
}

// probePath runs ffprobe on path.
func (t Tool) probePath(ctx context.Context, path string) (Info, error) {
	args := append(t.runner().InputOptions(), "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := t.runner().Run(ctx, t.ffprobe(), args, maxProbeOutput)
	if err != nil {
		return Info{}, err
	}
	return parseProbe(out)
}

// bounded refuses audio longer than MaxDuration or without a known
// length: the work after a probe is sized by the length.
func (t Tool) bounded(info Info) error {
	switch {
	case info.Duration <= 0:
		return filesystem.Errorf(filesystem.CodeRejected, "audio: unknown length")
	case info.Duration > t.maxDuration():
		return filesystem.Errorf(filesystem.CodeRejected, "audio is longer than %s", t.maxDuration())
	}
	return nil
}

// probeReport is the part of ffprobe's JSON the package reads.
type probeReport struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
	} `json:"streams"`
}

// codecAudio is ffprobe's type of an audio stream.
const codecAudio = "audio"

// parseProbe turns ffprobe's JSON into Info. A report without an audio
// stream is CodeRejected.
func parseProbe(out []byte) (Info, error) {
	var rep probeReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return Info{}, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("audio: unreadable probe report: %w", err))
	}
	info := Info{Format: rep.Format.FormatName}
	info.Duration = ffmpeg.ParseSeconds(rep.Format.Duration)
	if b, err := strconv.ParseInt(rep.Format.BitRate, 10, 64); err == nil {
		info.BitRate = b
	}
	for _, s := range rep.Streams {
		if s.CodecType != codecAudio {
			continue
		}
		info.Codec, info.Channels = s.CodecName, s.Channels
		info.SampleRate, _ = strconv.Atoi(s.SampleRate)
		return info, nil
	}
	return Info{}, filesystem.Errorf(filesystem.CodeRejected, "audio: no audio stream")
}
