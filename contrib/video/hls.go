package video

import (
	"context"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/video/ffmpeg"
)

// HLS output: the names and types of what Tool.HLS writes.
const (
	// HLSMaster is the master playlist's file name: the one URL a player
	// is given. It names each rendition's playlist by a relative path,
	// and each playlist its segments, so the set plays from any prefix
	// as long as its files stay together.
	HLSMaster = "master.m3u8"
	// HLSPlaylistContentType is the type of the master and rendition
	// playlists.
	HLSPlaylistContentType = "application/vnd.apple.mpegurl"
	// HLSSegmentContentType is the type of a segment (MPEG-TS).
	HLSSegmentContentType = "video/mp2t"
	// HLSFMP4SegmentContentType is the type of a segment in fragmented
	// MP4 (HLS.SegmentType HLSSegmentFMP4), and HLSInitContentType that
	// of each rendition's initialization file (<rendition>_init.mp4).
	HLSFMP4SegmentContentType = "video/iso.segment"
	HLSInitContentType        = "video/mp4"
	hlsPlaylistExt            = ".m3u8"
	hlsSegmentExt             = ".ts"
	hlsFMP4SegmentExt         = ".m4s"
	hlsInitSuffix             = "_init.mp4"
)

// Segment types of HLS.SegmentType: the container of the media
// segments.
const (
	// HLSSegmentMPEGTS is MPEG-TS, the default: what every HLS player
	// reads, with H.264 and AAC.
	HLSSegmentMPEGTS = "mpegts"
	// HLSSegmentFMP4 is fragmented MP4 (CMAF), with one initialization
	// file per rendition: what HEVC and AV1 in HLS need, and segments a
	// DASH manifest can share.
	HLSSegmentFMP4 = "fmp4"
)

// HLS encryption bounds.
const (
	// HLSKeyBytes is the length of an AES-128 key and of its IV.
	HLSKeyBytes = 16
	// maxHLSKeyURI bounds HLSEncryption.KeyURI, which every rendition
	// playlist repeats.
	maxHLSKeyURI = 2048
)

// hlsMuxerOptions are the option prefixes an HLSProfile's args may not
// hold: the HLS muxer's own (file names, playlists, keys, segment type)
// and those that would send its output over the network. The ladder
// sets them, so a profile cannot write files outside its work directory.
var hlsMuxerOptions = []string{"-hls", "-master_pl", "-var_stream_map", "-strftime", "-method", "-http", "-headers", "-ignore_io_errors"}

// Defaults of HLS, used when a field is zero.
const (
	// DefaultHLSSegment is the target length of one segment, Apple's
	// recommendation for video on demand.
	DefaultHLSSegment = 6 * time.Second
	// DefaultHLSMaxOutput bounds the bytes of every file of one ladder
	// together.
	DefaultHLSMaxOutput int64 = 4 << 30
	// MaxRenditions caps HLS.Renditions: each one is an encode of the
	// whole video running at the same time.
	MaxRenditions = 8
	// minHLSSegment and maxHLSSegment bound HLS.SegmentDuration: shorter
	// segments multiply the files, longer ones delay the start.
	minHLSSegment = time.Second
	maxHLSSegment = time.Minute
	// hlsPoll is how often the work directory's size is measured while
	// FFmpeg writes, so a ladder over MaxOutput is stopped as it grows
	// rather than after it filled the disk.
	hlsPoll = 200 * time.Millisecond
	// hlsBufferRatio sizes each rendition's rate-control buffer from its
	// bit rate, as encoding guides do for streaming.
	hlsBufferRatio = 2
	// hlsSegmentDigits is the width of a segment's number in its name.
	hlsSegmentDigits = 5
)

// Rendition is one rung of an HLS ladder: a size box and a bit rate.
type Rendition struct {
	// Name names the rendition's files (<Name>.m3u8, <Name>_00000.ts):
	// 1 to 32 of a-z, 0-9, "_" and "-", starting with a letter or digit,
	// unique in the ladder, and not "master".
	Name string
	// MaxWidth and MaxHeight bound the display size, as in Transcode: a
	// larger video is scaled down to fit, never up. Both must be set.
	MaxWidth, MaxHeight int
	// VideoBitRate is the H.264 target and peak rate, in bits per second;
	// with the duration it bounds the rendition's size.
	VideoBitRate int
}

// DefaultRenditions is the ladder HLS encodes without one: 1080p, 720p,
// 480p and 360p at common streaming bit rates.
var DefaultRenditions = []Rendition{
	{Name: "1080p", MaxWidth: 1920, MaxHeight: 1080, VideoBitRate: 5_000_000},
	{Name: "720p", MaxWidth: 1280, MaxHeight: 720, VideoBitRate: 2_800_000},
	{Name: "480p", MaxWidth: 854, MaxHeight: 480, VideoBitRate: 1_400_000},
	{Name: "360p", MaxWidth: 640, MaxHeight: 360, VideoBitRate: 800_000},
}

// renditionName is the closed shape of Rendition.Name: it ends up in file
// names and in FFmpeg's -var_stream_map, so nothing else may reach them.
var renditionName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// HLSProfile is the encoding of every rendition of an HLS ladder in
// place of H.264 and AAC: FFmpeg's encoder names and options. Like a
// Transcode Profile it is the application's configuration, never a
// client's input, and it cannot lift a bound: args that hold one of the
// options ffmpeg.CheckProfileArgs refuses, or one of the HLS muxer's own
// (hlsMuxerOptions: "-hls_*", "-master_pl_name", "-var_stream_map", the
// network options), are CodeInvalidArgument. Each rendition's
// VideoBitRate is still set after VideoArgs (-b:v, -maxrate and -bufsize
// per stream), so the ladder's rates hold for any encoder that honours
// them.
//
// HEVC needs SegmentType HLSSegmentFMP4 and, for Apple players, the
// "-tag:v", "hvc1" VideoArgs.
type HLSProfile struct {
	// VideoEncoder is FFmpeg's video encoder: "libx265", "libsvtav1",
	// "h264_videotoolbox", "h264_nvenc".
	VideoEncoder string
	// VideoArgs are its options ("-preset", "fast", "-tag:v", "hvc1").
	VideoArgs []string
	// PixelFormat is the output's pixel format; "" means yuv420p.
	PixelFormat string
	// AudioEncoder is FFmpeg's audio encoder ("aac", "libopus").
	AudioEncoder string
	// AudioArgs are its options ("-b:a", "128000").
	AudioArgs []string
}

// HLSEncryption encrypts every segment of a ladder with AES-128
// (EXT-X-KEY METHOD=AES-128). The key is never handed to the HLSWriter:
// FFmpeg reads it from a file in a work directory of its own, removed
// before HLS returns, and the playlists name only KeyURI. Storing the
// key and serving KeyURI to entitled viewers only is the application's
// part; a key served beside the segments protects nothing.
type HLSEncryption struct {
	// Key is the AES-128 key, HLSKeyBytes long.
	Key []byte
	// KeyURI is where a player fetches the key, as the playlists name it
	// (an absolute URL, or a path relative to the playlist): at most 2048
	// bytes, no control character or double quote.
	KeyURI string
	// IV is the initialization vector, HLSKeyBytes long; nil uses each
	// segment's sequence number, as the HLS specification allows.
	IV []byte
}

// String keeps the key out of logs and error messages.
func (e HLSEncryption) String() string {
	return "HLSEncryption{KeyURI: " + strconv.Quote(e.KeyURI) + ", Key: [redacted]}"
}

// GoString keeps the key out of %#v.
func (e HLSEncryption) GoString() string { return e.String() }

// LogValue keeps the key out of structured logs.
func (e HLSEncryption) LogValue() slog.Value {
	return slog.GroupValue(slog.String("key_uri", e.KeyURI), slog.String("key", "[redacted]"))
}

// HLS configures Tool.HLS: an adaptive-bit-rate ladder of H.264 and AAC
// renditions in MPEG-TS segments, with a master playlist (another codec
// with Profile, fragmented MP4 with SegmentType, AES-128 with
// Encryption). The zero value
// encodes DefaultRenditions with the defaults. Every bound of Transcode
// holds: the duration, frame rate and frame count, the first video and
// audio stream only, no metadata or chapters, and an output limit, here
// on all the files together.
type HLS struct {
	// Renditions is the ladder, at most MaxRenditions; nil means
	// DefaultRenditions. A rung whose box is larger than the video is
	// dropped, except the smallest such rung, which encodes the video at
	// its own size: a 720p upload gets 720p and below, never an upscaled
	// 1080p.
	Renditions []Rendition
	// SegmentDuration is each segment's target length, 1s to 1m; 0
	// means DefaultHLSSegment.
	SegmentDuration time.Duration
	// MaxDuration refuses a longer input (CodeRejected) and cuts the
	// output there; 0 means DefaultMaxDuration.
	MaxDuration time.Duration
	// MaxFrameRate caps every rendition's frames per second; 0 means
	// DefaultMaxFrameRate.
	MaxFrameRate int
	// MaxOutput bounds the bytes of all the files together; FFmpeg is
	// stopped as soon as its work directory grows past it (CodeTooLarge).
	// 0 means DefaultHLSMaxOutput.
	MaxOutput int64
	// Timeout bounds the encode (CodeUnavailable beyond); 0 means
	// DefaultTranscodeTimeout.
	Timeout time.Duration
	// Preset is one of Presets; "" means DefaultPreset.
	Preset string
	// AudioBitRate of every rendition's AAC track, in bits per second; 0
	// means DefaultAudioBitRate.
	AudioBitRate int
	// Profile, when set, replaces libx264 and AAC with other encoders
	// (see HLSProfile); Preset and AudioBitRate must then be zero, the
	// profile's args carrying its own. Nil encodes H.264 and AAC.
	Profile *HLSProfile
	// SegmentType is HLSSegmentMPEGTS ("" means it) or HLSSegmentFMP4.
	SegmentType string
	// Encryption, when set, encrypts every segment with AES-128 (see
	// HLSEncryption). Nil writes clear segments. FFmpeg encrypts MPEG-TS
	// segments only: with HLSSegmentFMP4 it is CodeUnsupported.
	Encryption *HLSEncryption
}

// HLSWriter receives one file of a ladder: its name (HLSMaster, a
// rendition playlist or a segment, never with a directory), its type
// and its bytes. Tool.HLS calls it for every segment first, then the
// rendition playlists, then HLSMaster, so a player given the master
// never finds a file missing. An error stops the ladder and is returned.
type HLSWriter func(ctx context.Context, name, contentType string, body io.Reader) error

// HLS encodes r as an HLS ladder (see HLS) and hands every file to
// write. FFmpeg writes into a work directory that is removed before HLS
// returns. A file that is not a readable video, or longer than
// MaxDuration, is CodeRejected; options outside their range are
// CodeInvalidArgument. Encoding several renditions is slow: run it from
// a job for anything longer than a short clip.
func (t Tool) HLS(ctx context.Context, r io.Reader, opts HLS, write HLSWriter) error {
	if err := opts.check(); err != nil {
		return err
	}
	if write == nil {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: HLS needs a writer")
	}
	return t.withWorkFile(r, func(in string) error {
		info, err := t.probePath(ctx, in)
		if err != nil {
			return err
		}
		if info.Duration > opts.maxDuration() {
			return filesystem.Errorf(filesystem.CodeRejected, "video is longer than %s", opts.maxDuration())
		}
		return t.hlsPath(ctx, in, info, opts, write)
	})
}

// hlsPath encodes the work file in, already probed as info. It checks
// opts itself, since the plugin (WithHLS) calls it without Tool.HLS.
func (t Tool) hlsPath(ctx context.Context, in string, info Info, opts HLS, write HLSWriter) error {
	if err := opts.check(); err != nil {
		return err
	}
	ladder := opts.ladder(info)
	dir, err := os.MkdirTemp(t.TempDir, packageName+"-hls-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	keyInfo, err := opts.writeKeyInfo(t.TempDir)
	if err != nil {
		return err
	}
	if keyInfo != "" {
		defer func() { _ = os.RemoveAll(filepath.Dir(keyInfo)) }()
	}

	run := t.runner()
	run.Timeout = opts.timeout()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var over atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(hlsPoll)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				if dirSize(dir) > opts.maxOutput() {
					over.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	_, err = run.Run(runCtx, t.program(t.FFmpeg, DefaultFFmpeg), opts.args(t.inputOptions(), in, dir, keyInfo, ladder, info.AudioCodec != ""), ffmpeg.MaxFileRunOutput)
	cancel()
	<-done
	if over.Load() || dirSize(dir) > opts.maxOutput() {
		return filesystem.Errorf(filesystem.CodeTooLarge, "video: HLS output larger than %d bytes", opts.maxOutput())
	}
	if err != nil {
		return err
	}
	files, err := opts.files(dir, ladder)
	if err != nil {
		return err
	}
	for _, name := range files {
		if err := writeHLSFile(ctx, filepath.Join(dir, name), name, write); err != nil {
			return err
		}
	}
	return nil
}

// writeHLSFile hands one work file to write.
func writeHLSFile(ctx context.Context, path, name string, write HLSWriter) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var contentType string
	switch {
	case strings.HasSuffix(name, hlsPlaylistExt):
		contentType = HLSPlaylistContentType
	case strings.HasSuffix(name, hlsInitSuffix):
		contentType = HLSInitContentType
	case strings.HasSuffix(name, hlsFMP4SegmentExt):
		contentType = HLSFMP4SegmentContentType
	default:
		contentType = HLSSegmentContentType
	}
	return write(ctx, name, contentType, f)
}

// writeKeyInfo writes o.Encryption's key and FFmpeg's key info file
// (the key URI, the key file's path, the IV in hex) into a work directory
// of their own under tmp, never the ladder's, so the key is never handed
// to the writer. It returns the info file's path, or "" without
// encryption; the caller removes its directory.
func (o HLS) writeKeyInfo(tmp string) (string, error) {
	e := o.Encryption
	if e == nil {
		return "", nil
	}
	dir, err := os.MkdirTemp(tmp, packageName+"-hlskey-*")
	if err != nil {
		return "", err
	}
	keyFile := filepath.Join(dir, "key")
	info := e.KeyURI + "\n" + keyFile + "\n"
	if e.IV != nil {
		info += hex.EncodeToString(e.IV) + "\n"
	}
	infoFile := filepath.Join(dir, "keyinfo")
	if err := os.WriteFile(keyFile, e.Key, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(infoFile, []byte(info), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return infoFile, nil
}

// files lists the work directory in writing order (segments and, in
// fMP4, initialization files, then the rendition playlists, then the
// master) and checks it holds what the ladder should and nothing else:
// every name is one this package chose, every entry a regular file, and
// the master, every playlist and, in fMP4, every initialization file
// present. Anything else is a failure, not something to upload.
func (o HLS) files(dir string, ladder []Rendition) ([]string, error) {
	fmp4 := o.SegmentType == HLSSegmentFMP4
	ext := hlsSegmentExt
	if fmp4 {
		ext = hlsFMP4SegmentExt
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	unexpected := func(why string) ([]string, error) {
		return nil, filesystem.Errorf(filesystem.CodeUnavailable, "video: unexpected HLS output: %s", why)
	}
	var segments, playlists []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			return unexpected(name + " is not a regular file")
		}
		seen[name] = true
		switch {
		case name == HLSMaster:
		case slices.ContainsFunc(ladder, func(r Rendition) bool { return name == r.Name+hlsPlaylistExt }):
			playlists = append(playlists, name)
		case slices.ContainsFunc(ladder, func(r Rendition) bool { return isSegmentOf(name, r.Name, ext) }),
			fmp4 && slices.ContainsFunc(ladder, func(r Rendition) bool { return name == r.Name+hlsInitSuffix }):
			segments = append(segments, name)
		default:
			return unexpected(name)
		}
	}
	if !seen[HLSMaster] {
		return unexpected("no master playlist")
	}
	for _, r := range ladder {
		if !seen[r.Name+hlsPlaylistExt] {
			return unexpected("no playlist for " + r.Name)
		}
		if fmp4 && !seen[r.Name+hlsInitSuffix] {
			return unexpected("no initialization file for " + r.Name)
		}
	}
	slices.Sort(segments)
	slices.Sort(playlists)
	return append(append(segments, playlists...), HLSMaster), nil
}

// isSegmentOf reports whether name is <rendition>_<digits><ext>.
func isSegmentOf(name, rendition, ext string) bool {
	num, ok := strings.CutPrefix(name, rendition+"_")
	if !ok {
		return false
	}
	num, ok = strings.CutSuffix(num, ext)
	if !ok || len(num) < hlsSegmentDigits {
		return false
	}
	_, err := strconv.ParseUint(num, 10, 32)
	return err == nil
}

// dirSize sums the sizes of the regular files directly in dir; an entry
// that vanished mid-walk counts as nothing.
func dirSize(dir string) int64 {
	var n int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		var fi fs.FileInfo
		if fi, _ = e.Info(); fi != nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
	}
	return n
}

// check refuses options outside their range.
func (o HLS) check() error {
	switch {
	case o.SegmentDuration < 0, o.MaxDuration < 0, o.MaxFrameRate < 0, o.MaxOutput < 0, o.Timeout < 0, o.AudioBitRate < 0:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: negative HLS option")
	case o.SegmentDuration != 0 && (o.SegmentDuration < minHLSSegment || o.SegmentDuration > maxHLSSegment):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: HLS segment duration %s is not within %s..%s", o.SegmentDuration, minHLSSegment, maxHLSSegment)
	case o.Preset != "" && !slices.Contains(Presets, o.Preset):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: unknown preset %q", o.Preset)
	case o.Renditions != nil && (len(o.Renditions) == 0 || len(o.Renditions) > MaxRenditions):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: an HLS ladder has 1 to %d renditions, not %d", MaxRenditions, len(o.Renditions))
	case o.SegmentType != "" && o.SegmentType != HLSSegmentMPEGTS && o.SegmentType != HLSSegmentFMP4:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: unknown HLS segment type %q", o.SegmentType)
	}
	if err := o.checkProfile(); err != nil {
		return err
	}
	if err := o.Encryption.check(); err != nil {
		return err
	}
	if o.Encryption != nil && o.SegmentType == HLSSegmentFMP4 {
		// FFmpeg's HLS muxer refuses to encrypt fMP4 ("not yet
		// supported"); say so before an encode rather than after.
		return filesystem.Errorf(filesystem.CodeUnsupported, "video: FFmpeg cannot encrypt fMP4 HLS segments; use MPEG-TS segments with Encryption")
	}
	seen := map[string]bool{}
	for _, r := range o.renditions() {
		switch {
		case !renditionName.MatchString(r.Name) || r.Name+hlsPlaylistExt == HLSMaster:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: rendition name %q is not allowed", r.Name)
		case seen[r.Name]:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: rendition %q appears twice", r.Name)
		case r.MaxWidth <= 0 || r.MaxHeight <= 0 || r.VideoBitRate <= 0:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: rendition %q needs a size box and a bit rate", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// checkProfile refuses a Profile that names something other than an
// FFmpeg encoder or pixel format, holds an option the ladder keeps for
// itself, or comes with the libx264 and AAC settings it replaces.
func (o HLS) checkProfile() error {
	p := o.Profile
	if p == nil {
		return nil
	}
	if o.Preset != "" || o.AudioBitRate != 0 {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: Preset and AudioBitRate are not used with an HLS Profile; put them in its args")
	}
	names := []string{p.VideoEncoder, p.AudioEncoder}
	if p.PixelFormat != "" {
		names = append(names, p.PixelFormat)
	}
	if err := ffmpeg.CheckNames(names...); err != nil {
		return err
	}
	if err := ffmpeg.CheckProfileArgs(p.VideoArgs, p.AudioArgs); err != nil {
		return err
	}
	for _, a := range slices.Concat(p.VideoArgs, p.AudioArgs) {
		if slices.ContainsFunc(hlsMuxerOptions, func(prefix string) bool { return strings.HasPrefix(a, prefix) }) {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: an HLS profile may not set %s; the ladder sets it", a)
		}
	}
	return nil
}

// check refuses a key or IV of the wrong length, and a KeyURI that is
// empty, too long, or could break the key info file's lines or the
// playlist's quoted attribute. A nil e (no encryption) passes.
func (e *HLSEncryption) check() error {
	switch {
	case e == nil:
		return nil
	case len(e.Key) != HLSKeyBytes:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: an HLS key is %d bytes, not %d", HLSKeyBytes, len(e.Key))
	case e.IV != nil && len(e.IV) != HLSKeyBytes:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: an HLS IV is %d bytes, not %d", HLSKeyBytes, len(e.IV))
	case e.KeyURI == "" || len(e.KeyURI) > maxHLSKeyURI || strings.ContainsFunc(e.KeyURI, func(r rune) bool { return r < ' ' || r == 0x7f || r == '"' }):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "video: HLS key URI must be 1 to %d bytes with no control character or double quote", maxHLSKeyURI)
	}
	return nil
}

func (o HLS) renditions() []Rendition {
	if o.Renditions == nil {
		return DefaultRenditions
	}
	return o.Renditions
}

// ladder is the renditions encoded for a video of info's display size:
// every rung that scales the video down, in the configured order, plus
// the smallest rung that does not, which encodes it at its own size.
// A video of unknown size (0) gets every rung.
func (o HLS) ladder(info Info) []Rendition {
	all := o.renditions()
	if info.Width <= 0 || info.Height <= 0 {
		return all
	}
	top := -1
	for i, r := range all {
		if info.Width <= r.MaxWidth && info.Height <= r.MaxHeight {
			if top < 0 || r.MaxWidth*r.MaxHeight < all[top].MaxWidth*all[top].MaxHeight {
				top = i
			}
		}
	}
	var out []Rendition
	for i, r := range all {
		if i == top || info.Width > r.MaxWidth || info.Height > r.MaxHeight {
			out = append(out, r)
		}
	}
	return out
}

// args are ffmpeg's arguments to encode in as ladder into dir; audio says
// whether the input has an audio stream to map into every rendition.
// keyInfo is FFmpeg's key info file (writeKeyInfo), "" without
// encryption.
func (o HLS) args(inputOptions []string, in, dir, keyInfo string, ladder []Rendition, audio bool) []string {
	fps := orDefault(o.MaxFrameRate, DefaultMaxFrameRate)
	frames := int64(o.maxDuration().Seconds()*float64(fps)) + 1
	seg := o.segment()
	// One decode, split into a scaled stream per rendition.
	var graph strings.Builder
	graph.WriteString("[0:v:0]split=" + strconv.Itoa(len(ladder)))
	for i := range ladder {
		graph.WriteString("[s" + strconv.Itoa(i) + "]")
	}
	for i, r := range ladder {
		graph.WriteString(";[s" + strconv.Itoa(i) + "]scale=w='min(" + strconv.Itoa(r.MaxWidth) + ",iw)':h='min(" + strconv.Itoa(r.MaxHeight) +
			",ih)':force_original_aspect_ratio=decrease:force_divisible_by=2[v" + strconv.Itoa(i) + "]")
	}
	args := append([]string{"-v", "error", "-nostdin"}, inputOptions...)
	args = append(args, "-i", in, "-t", ffmpeg.Seconds(o.maxDuration()), "-filter_complex", graph.String())
	var streams []string
	for i := range ladder {
		args = append(args, "-map", "[v"+strconv.Itoa(i)+"]")
		stream := "v:" + strconv.Itoa(i)
		if audio {
			args = append(args, "-map", "0:a:0")
			stream += ",a:" + strconv.Itoa(i)
		}
		streams = append(streams, stream+",name:"+ladder[i].Name)
	}
	if p := o.Profile; p != nil {
		pixFmt := p.PixelFormat
		if pixFmt == "" {
			pixFmt = defaultPixelFormat
		}
		args = append(args, "-c:v", p.VideoEncoder)
		args = append(args, p.VideoArgs...)
		args = append(args, "-pix_fmt", pixFmt)
	} else {
		args = append(args, "-c:v", "libx264", "-preset", o.preset(), "-pix_fmt", defaultPixelFormat)
	}
	for i, r := range ladder {
		n := strconv.Itoa(i)
		rate := strconv.Itoa(r.VideoBitRate)
		args = append(args, "-b:v:"+n, rate, "-maxrate:v:"+n, rate, "-bufsize:v:"+n, strconv.Itoa(r.VideoBitRate*hlsBufferRatio))
	}
	// Key frames on segment boundaries, so every rendition switches at
	// the same instants.
	args = append(args, "-fpsmax", strconv.Itoa(fps), "-frames:v", strconv.FormatInt(frames, 10),
		"-force_key_frames", "expr:gte(t,n_forced*"+ffmpeg.Seconds(seg)+")", "-sc_threshold", "0")
	switch {
	case !audio:
	case o.Profile != nil:
		args = append(args, "-c:a", o.Profile.AudioEncoder)
		args = append(args, o.Profile.AudioArgs...)
	default:
		args = append(args, "-c:a", "aac", "-b:a", strconv.Itoa(orDefault(o.AudioBitRate, DefaultAudioBitRate)), "-ac", "2")
	}
	segType, segExt := HLSSegmentMPEGTS, hlsSegmentExt
	if o.SegmentType == HLSSegmentFMP4 {
		segType, segExt = HLSSegmentFMP4, hlsFMP4SegmentExt
	}
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1",
		"-f", "hls", "-hls_time", ffmpeg.Seconds(seg), "-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments", "-hls_segment_type", segType)
	if segType == HLSSegmentFMP4 {
		// FFmpeg expands %v in the init file's name only when there are
		// several variant streams, so a one-rung ladder names it.
		init := "%v" + hlsInitSuffix
		if len(ladder) == 1 {
			init = ladder[0].Name + hlsInitSuffix
		}
		args = append(args, "-hls_fmp4_init_filename", init)
	}
	if keyInfo != "" {
		args = append(args, "-hls_key_info_file", keyInfo)
	}
	args = append(args,
		"-hls_segment_filename", filepath.Join(dir, "%v_%0"+strconv.Itoa(hlsSegmentDigits)+"d"+segExt),
		"-master_pl_name", HLSMaster,
		"-var_stream_map", strings.Join(streams, " "),
		"-y", filepath.Join(dir, "%v"+hlsPlaylistExt))
	return args
}

func (o HLS) segment() time.Duration {
	if o.SegmentDuration == 0 {
		return DefaultHLSSegment
	}
	return o.SegmentDuration
}

func (o HLS) maxDuration() time.Duration {
	if o.MaxDuration == 0 {
		return DefaultMaxDuration
	}
	return o.MaxDuration
}

func (o HLS) maxOutput() int64 {
	if o.MaxOutput == 0 {
		return DefaultHLSMaxOutput
	}
	return o.MaxOutput
}

func (o HLS) timeout() time.Duration {
	if o.Timeout == 0 {
		return DefaultTranscodeTimeout
	}
	return o.Timeout
}

func (o HLS) preset() string {
	if o.Preset == "" {
		return DefaultPreset
	}
	return o.Preset
}
