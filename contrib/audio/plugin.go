package audio

import (
	"context"
	"encoding/base64"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// PluginName is the records plugin's name.
const PluginName = "audio"

// Metadata keys the plugin sets on audio files. MetaCodec is the same key
// contrib/video uses for a video's audio track.
const (
	MetaDuration   = "duration_ms" // whole milliseconds
	MetaCodec      = "audio_codec"
	MetaBitRate    = "bit_rate"    // bits per second; absent when unknown
	MetaSampleRate = "sample_rate" // Hz
	MetaChannels   = "channels"
	MetaWaveform   = "waveform" // with WithWaveform: base64 of one byte (0-255) per point
)

// metaKeys are every key the plugin owns. They are cleared only when the
// plugin describes an audio file (see describe), never on another type:
// duration_ms and audio_codec are shared with contrib/video, so clearing
// them on every file would wipe what that plugin recorded for a video.
var metaKeys = []string{MetaDuration, MetaCodec, MetaBitRate, MetaSampleRate, MetaChannels, MetaWaveform}

// MaxStoredWaveformPoints bounds WithWaveform: the waveform rides in
// every read of the file's row, so it stays a few KiB.
const MaxStoredWaveformPoints = 1024

// waveformLevels is the top of a stored point's byte.
const waveformLevels = 255

// Plugin is a records plugin that stores every audio file's metadata
// (MetaDuration, MetaCodec, …) when it is recorded and when its content
// is replaced, and with WithWaveform its peaks. Construct it with
// NewPlugin and install it with records.WithPlugins.
//
// A file ffprobe cannot read is recorded without metadata, or refused
// with WithStrict. A missing program is an error either way, so audio is
// never silently recorded without metadata on a host without FFmpeg.
type Plugin struct {
	tool     Tool
	strict   bool
	waveform int
}

// PluginOption configures a Plugin.
type PluginOption func(*Plugin)

// WithStrict refuses an audio file ffprobe cannot read (CodeRejected).
func WithStrict() PluginOption { return func(p *Plugin) { p.strict = true } }

// WithWaveform also stores points peaks (MetaWaveform), clamped to 1 to
// MaxStoredWaveformPoints, so a player draws the bars without fetching
// the audio. Read them with WaveformOf.
func WithWaveform(points int) PluginOption {
	return func(p *Plugin) { p.waveform = min(max(points, 1), MaxStoredWaveformPoints) }
}

// NewPlugin returns a Plugin running t.
func NewPlugin(t Tool, opts ...PluginOption) *Plugin {
	p := &Plugin{tool: t}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(ctx context.Context, s *records.Store, f *records.File) error {
	return p.describe(ctx, s, f)
}

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks; the plugin stores nothing
// outside the row.
func (p *Plugin) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// BeforeRelease implements records.ReleaseHooks: a file recorded or
// replaced while quarantined was not read then (see describe), so it is
// described when its scan passes.
func (p *Plugin) BeforeRelease(ctx context.Context, s *records.Store, f *records.File) error {
	return p.describe(ctx, s, f)
}

// BeforeReplace implements records.ReplaceHooks: the new content is
// described afresh, and when it is no longer audio the keys the plugin
// wrote for the old content are dropped (dropStale).
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, current records.File, next *records.File) error {
	if IsAudio(current.ContentType) && !IsAudio(next.ContentType) {
		dropStale(next)
	}
	return p.describe(ctx, s, next)
}

// siblingTypes maps each key the plugin shares with another plugin to
// the types that plugin describes: duration_ms and audio_codec to
// contrib/video's videos. On a replacement by such a type the key is
// that plugin's to set, so dropStale leaves it, whichever order the
// hooks run in.
var siblingTypes = map[string]string{MetaDuration: "video/", MetaCodec: "video/"}

// dropStale removes from next the keys the plugin wrote for an audio file
// whose content next (no longer audio) replaces, so they do not outlive
// it; a shared key is left when next is its sibling's type (see
// siblingTypes). Without that sibling installed, such a key keeps the
// audio file's value. It copies the metadata map, which the caller may
// share.
func dropStale(next *records.File) {
	meta := maps.Clone(next.Metadata)
	for _, k := range metaKeys {
		if prefix, shared := siblingTypes[k]; shared && strings.HasPrefix(next.ContentType, prefix) {
			continue
		}
		delete(meta, k)
	}
	if len(meta) == 0 {
		meta = nil
	}
	next.Metadata = meta
}

// describe sets an audio file's metadata from its object, replacing
// whatever the plugin set before (only clearing it for a quarantined
// file). Any other type is left untouched, keys and all: they may be
// another plugin's or the application's. It copies the metadata map,
// which the caller may share.
func (p *Plugin) describe(ctx context.Context, s *records.Store, f *records.File) error {
	if !IsAudio(f.ContentType) {
		return nil
	}
	meta := maps.Clone(f.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	for _, k := range metaKeys {
		delete(meta, k)
	}
	defer func() {
		if len(meta) == 0 {
			meta = nil
		}
		f.Metadata = meta
	}()
	// A quarantined file's bytes are unscanned and are not read: it is
	// described on release (BeforeRelease).
	if f.Quarantined() {
		return nil
	}
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	rc, err := disk.Read(ctx, f.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var info Info
	var peaks []float64
	err = p.tool.runner().WorkFile(rc, func(path string) error {
		var err error
		if info, err = p.tool.probePath(ctx, path); err != nil || p.waveform == 0 {
			return err
		}
		peaks, err = p.tool.peaks(ctx, path, info, p.waveform)
		return err
	})
	if err != nil {
		if !p.strict && filesystem.CodeOf(err) == filesystem.CodeRejected {
			return nil
		}
		return err
	}
	meta[MetaDuration] = strconv.FormatInt(info.Duration.Milliseconds(), 10)
	meta[MetaCodec] = info.Codec
	meta[MetaSampleRate] = strconv.Itoa(info.SampleRate)
	meta[MetaChannels] = strconv.Itoa(info.Channels)
	if info.BitRate > 0 {
		meta[MetaBitRate] = strconv.FormatInt(info.BitRate, 10)
	}
	if peaks != nil {
		meta[MetaWaveform] = encodeWaveform(peaks)
	}
	return nil
}

// encodeWaveform stores peaks as one byte each, base64.
func encodeWaveform(peaks []float64) string {
	b := make([]byte, len(peaks))
	for i, v := range peaks {
		b[i] = byte(math.Round(v * waveformLevels))
	}
	return base64.RawStdEncoding.EncodeToString(b)
}

// Meta is the metadata the plugin recorded for an audio file.
type Meta struct {
	Duration             time.Duration
	Codec                string
	BitRate              int64
	SampleRate, Channels int
}

// Of returns the metadata recorded for f; ok is false when there is none
// (not audio, unreadable, or recorded before the plugin was installed).
func Of(f records.File) (m Meta, ok bool) {
	ms, err := strconv.ParseInt(f.Metadata[MetaDuration], 10, 64)
	if err != nil {
		return Meta{}, false
	}
	m = Meta{Duration: time.Duration(ms) * time.Millisecond, Codec: f.Metadata[MetaCodec]}
	m.BitRate, _ = strconv.ParseInt(f.Metadata[MetaBitRate], 10, 64)
	m.SampleRate, _ = strconv.Atoi(f.Metadata[MetaSampleRate])
	m.Channels, _ = strconv.Atoi(f.Metadata[MetaChannels])
	return m, true
}

// WaveformOf returns the peaks stored for f (WithWaveform) in [0, 1]; ok is
// false when there are none or they are not the plugin's encoding.
func WaveformOf(f records.File) ([]float64, bool) {
	b, err := base64.RawStdEncoding.DecodeString(f.Metadata[MetaWaveform])
	if err != nil || len(b) == 0 {
		return nil, false
	}
	peaks := make([]float64, len(b))
	for i, v := range b {
		peaks[i] = float64(v) / waveformLevels
	}
	return peaks, true
}
