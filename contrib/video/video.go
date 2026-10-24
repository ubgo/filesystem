// Package video reads what applications need from uploaded videos with
// FFmpeg's programs: ffprobe for the metadata (duration, display size,
// codecs, frame rate, bit rate) and ffmpeg for a poster frame and for
// removing the metadata a phone writes into a video (its location among
// it). It is a module of its own because it runs external programs; the
// core never does.
//
// Everything goes through a Tool, whose zero value is usable: the programs
// are looked up on PATH as "ffprobe" and "ffmpeg", and every limit has a
// default. Inputs are bounded three ways, because a video is untrusted
// input to a large C program: at most MaxInput bytes are copied to a work
// file (CodeTooLarge beyond), each program run is killed after Timeout,
// and its output is capped. Programs run without a shell, with the
// demuxers limited to Formats and the protocols to "file", so a playlist
// disguised as a video cannot make FFmpeg read other files or URLs.
//
// On top of the Tool: Inspect is an upload.Inspector that refuses what is
// not a readable video or is over Limits; Sanitize is an upload.Processor
// that strips metadata by remuxing; Plugin (plugin.go) is a records plugin
// that stores the metadata, and optionally a poster, with every video file.
package video

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// Default program names, looked up on PATH when Tool leaves them empty.
const (
	DefaultFFprobe = ffmpeg.FFprobe
	DefaultFFmpeg  = ffmpeg.FFmpeg
)

// Default limits, used when Tool leaves a field zero.
const (
	// DefaultTimeout bounds one program run.
	DefaultTimeout = ffmpeg.DefaultTimeout
	// DefaultMaxInput bounds the bytes copied to the work file.
	DefaultMaxInput = ffmpeg.DefaultMaxInput
	// DefaultMaxPoster bounds a poster's encoded size.
	DefaultMaxPoster int64 = 10 << 20
)

// maxProbeOutput bounds ffprobe's JSON; a real report is a few KiB.
const maxProbeOutput = 1 << 20

// DefaultFormats are the demuxers the programs may use (FFmpeg's
// -format_whitelist): the containers phones, cameras and browsers record.
// Playlists (HLS, concat) are deliberately absent: they can name other
// files and URLs.
var DefaultFormats = []string{"mov", "mp4", "m4a", "3gp", "3g2", "mj2", "matroska", "webm", "avi", "mpegts", "mpeg", "flv", "ogg"}

// ErrNoProgram is in the chain of the error of a run whose program is not
// installed (or not at the configured path), classified CodeUnavailable:
// a deployment problem, not the client's file.
var ErrNoProgram = ffmpeg.ErrNoProgram

// Tool runs ffprobe and ffmpeg. The zero value uses the defaults.
type Tool struct {
	// FFprobe and FFmpeg are the programs' paths or names; empty means
	// DefaultFFprobe and DefaultFFmpeg, looked up on PATH.
	FFprobe string
	FFmpeg  string
	// Timeout bounds each program run; 0 means DefaultTimeout. A run cut
	// short is CodeUnavailable.
	Timeout time.Duration
	// MaxInput bounds the bytes of a video copied to the work file; 0
	// means DefaultMaxInput. A longer input is CodeTooLarge.
	MaxInput int64
	// MaxPoster bounds a poster's encoded size; 0 means DefaultMaxPoster.
	MaxPoster int64
	// Formats are the demuxers the programs may use; nil means
	// DefaultFormats.
	Formats []string
	// TempDir is where work files go; empty means os.TempDir(). Work files
	// are removed before each call returns.
	TempDir string
	// Env adds variables to FFmpeg's environment, as "KEY=value"
	// (FONTCONFIG_FILE, a library path), forwarded to ffmpeg.Runner.Env.
	// FFmpeg otherwise sees only PATH, HOME, TMPDIR, TMP, LANG and LC_ALL
	// of this process, so the application's secrets never reach it.
	Env []string
}

// Info is what ffprobe reports about a video.
type Info struct {
	// Format is the container as ffprobe names it ("mov,mp4,m4a,3gp,3g2,mj2").
	Format string
	// Duration the container claims, which a file can lie about
	// (Inspect's Limits measure the packets instead); 0 when unknown, and
	// at most the longest time.Duration.
	Duration time.Duration
	// Width and Height are the display size of the first video stream:
	// a phone video recorded upright but stored sideways with a rotation
	// reports its upright size.
	Width, Height int
	// Rotation is the stream's display rotation in degrees (0, 90, 180,
	// 270), clockwise.
	Rotation int
	// VideoCodec and AudioCodec are the first video and audio streams'
	// codecs ("h264", "aac"); AudioCodec is "" for a silent video.
	VideoCodec, AudioCodec string
	// FrameRate of the first video stream in frames per second, 0 when
	// unknown.
	FrameRate float64
	// BitRate of the container in bits per second, 0 when unknown.
	BitRate int64
}

// Probe reports what r holds. A file ffprobe cannot read, or one without
// a video stream, is CodeRejected.
func (t Tool) Probe(ctx context.Context, r io.Reader) (Info, error) {
	var info Info
	err := t.withWorkFile(r, func(path string) error {
		var err error
		info, err = t.probePath(ctx, path)
		return err
	})
	return info, err
}

// Poster returns the frame at offset at as a JPEG, scaled to the video's
// display size. An offset past the end falls back to the first frame, so
// a short clip still gets a poster.
func (t Tool) Poster(ctx context.Context, r io.Reader, at time.Duration) ([]byte, error) {
	var out []byte
	err := t.withWorkFile(r, func(path string) error {
		var err error
		out, err = t.posterPath(ctx, path, at)
		return err
	})
	return out, err
}

// withWorkFile copies at most MaxInput bytes of r to a work file, runs fn
// on its path and removes it.
func (t Tool) withWorkFile(r io.Reader, fn func(path string) error) error {
	return t.runner().WorkFile(r, fn)
}

// runner is the bounded program runner t's fields configure.
func (t Tool) runner() ffmpeg.Runner {
	formats := t.Formats
	if formats == nil {
		formats = DefaultFormats
	}
	return ffmpeg.Runner{Package: packageName, Formats: formats, Timeout: t.Timeout, MaxInput: t.MaxInput, TempDir: t.TempDir, Env: t.Env}
}

// probePath runs ffprobe on path.
func (t Tool) probePath(ctx context.Context, path string) (Info, error) {
	args := append(t.inputOptions(), "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := t.run(ctx, t.program(t.FFprobe, DefaultFFprobe), args, maxProbeOutput)
	if err != nil {
		return Info{}, err
	}
	return parseProbe(out)
}

// posterPath runs ffmpeg on path for one JPEG frame at at, falling back to
// the first frame when there is none at at (an offset past the end makes
// ffmpeg write nothing or fail, depending on the input): a file that is
// really unreadable fails again at 0 with its own error.
func (t Tool) posterPath(ctx context.Context, path string, at time.Duration) ([]byte, error) {
	out, err := t.frame(ctx, path, at)
	if at > 0 && (len(out) == 0 || filesystem.CodeOf(err) == filesystem.CodeRejected) {
		out, err = t.frame(ctx, path, 0)
	}
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "video: no frame to make a poster from")
	}
	return out, nil
}

// frame extracts one frame at at as JPEG; an offset past the end yields
// no output and no error.
func (t Tool) frame(ctx context.Context, path string, at time.Duration) ([]byte, error) {
	args := append([]string{"-v", "error", "-nostdin", "-ss", ffmpeg.Seconds(at)}, t.inputOptions()...)
	args = append(args, "-i", path, "-frames:v", "1", "-map", "0:v:0", "-f", "image2", "-c:v", "mjpeg", "-q:v", "3", "pipe:1")
	return t.run(ctx, t.program(t.FFmpeg, DefaultFFmpeg), args, t.maxPoster())
}

// inputOptions limit what FFmpeg may open while reading the input.
func (t Tool) inputOptions() []string { return t.runner().InputOptions() }

// run executes program with args (no shell) under the timeout, returning
// at most maxOut bytes of its standard output; more is CodeTooLarge.
func (t Tool) run(ctx context.Context, program string, args []string, maxOut int64) ([]byte, error) {
	return t.runner().Run(ctx, program, args, maxOut)
}

// ProgramError is the cause of a run that failed on its input, inside a
// CodeRejected error: its text ("video: ffprobe could not read the file")
// is safe for clients, Detail is the program's output for logs.
type ProgramError = ffmpeg.ProgramError

// packageName prefixes the messages of the package's errors.
const packageName = "video"

func (t Tool) program(set, def string) string {
	if set == "" {
		return def
	}
	return set
}

func (t Tool) maxPoster() int64 {
	if t.MaxPoster <= 0 {
		return DefaultMaxPoster
	}
	return t.MaxPoster
}

// probeReport is the part of ffprobe's JSON the package reads.
type probeReport struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecType    string            `json:"codec_type"`
		CodecName    string            `json:"codec_name"`
		Width        int               `json:"width"`
		Height       int               `json:"height"`
		AvgFrameRate string            `json:"avg_frame_rate"`
		Tags         map[string]string `json:"tags"`
		SideData     []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
		Disposition struct {
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
	} `json:"streams"`
}

// Stream types in ffprobe's report.
const (
	codecVideo = "video"
	codecAudio = "audio"
)

// Degrees in a quarter and a full turn, for normalising rotations.
const (
	quarterTurn = 90
	fullTurn    = 360
)

// parseProbe turns ffprobe's JSON into Info. A report without a video
// stream (an audio file, a cover image only) is CodeRejected.
func parseProbe(out []byte) (Info, error) {
	var rep probeReport
	if err := json.Unmarshal(out, &rep); err != nil {
		return Info{}, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("video: unreadable probe report: %w", err))
	}
	info := Info{Format: rep.Format.FormatName}
	info.Duration = ffmpeg.ParseSeconds(rep.Format.Duration)
	if b, err := strconv.ParseInt(rep.Format.BitRate, 10, 64); err == nil {
		info.BitRate = b
	}
	found := false
	for _, s := range rep.Streams {
		switch {
		case s.CodecType == codecVideo && !found && s.Disposition.AttachedPic == 0:
			found = true
			info.VideoCodec = s.CodecName
			info.Width, info.Height = s.Width, s.Height
			info.FrameRate = rate(s.AvgFrameRate)
			info.Rotation = rotation(s.Tags["rotate"], s.SideData)
			if info.Rotation%(2*quarterTurn) != 0 {
				info.Width, info.Height = info.Height, info.Width
			}
		case s.CodecType == codecAudio && info.AudioCodec == "":
			info.AudioCodec = s.CodecName
		}
	}
	if !found {
		return Info{}, filesystem.Errorf(filesystem.CodeRejected, "video: no video stream")
	}
	return info, nil
}

// rate parses ffprobe's "30000/1001" frame rates.
func rate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	n, errN := strconv.ParseFloat(num, 64)
	if !ok || errN != nil {
		return 0
	}
	d, errD := strconv.ParseFloat(den, 64)
	if errD != nil || d == 0 {
		return 0
	}
	return n / d
}

// rotation reads the display rotation, from the display matrix (current
// FFmpeg, counter-clockwise and often negative) or the older rotate tag
// (clockwise), as clockwise degrees in [0, 360).
func rotation(tag string, side []struct {
	Rotation float64 `json:"rotation"`
}) int {
	deg := 0
	if r, err := strconv.Atoi(tag); err == nil {
		deg = r
	}
	for _, sd := range side {
		if sd.Rotation != 0 {
			// Mod first: a float outside int's range converts to an
			// arbitrary value.
			deg = -int(math.Round(math.Mod(sd.Rotation, fullTurn)))
		}
	}
	return ((deg % fullTurn) + fullTurn) % fullTurn
}

func (t Tool) maxInput() int64 {
	if t.MaxInput <= 0 {
		return DefaultMaxInput
	}
	return t.MaxInput
}
