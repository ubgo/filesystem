package video

import (
	"context"
	"crypto/rand"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// PluginName is the records plugin's name.
const PluginName = "video"

// Metadata keys the plugin sets on video files. Width and height use the
// same keys as the dimensions plugin, so a layout reads one pair for
// images and videos alike.
const (
	MetaDuration   = "duration_ms" // whole milliseconds
	MetaWidth      = "width"
	MetaHeight     = "height"
	MetaVideoCodec = "video_codec"
	MetaAudioCodec = "audio_codec" // absent for a silent video
	MetaPosterKey  = "poster_key"  // with WithPoster: the poster's key, on MetaPosterDisk or else the file's disk
	// MetaPosterDisk names the poster's disk when WithLocation put it on
	// another disk than the file's; absent when it is on the file's own.
	MetaPosterDisk = "poster_disk"
	// MetaHLSKey is, with WithHLS, the key of the ladder's master
	// playlist (HLSMaster); every file of the ladder is under the same
	// directory, on MetaHLSDisk or else the file's disk.
	MetaHLSKey = "hls_key"
	// MetaHLSDisk names the ladder's disk when WithLocation put it on
	// another disk than the file's; absent when it is on the file's own.
	MetaHLSDisk = "hls_disk"
)

// metaKeys are every key the plugin owns. They are cleared only when the
// plugin describes a video (see describe), never on another type: width
// and height are shared with the dimensions plugin, and duration_ms and
// audio_codec with contrib/audio, so clearing them on every file would
// wipe what those plugins recorded.
var metaKeys = []string{MetaDuration, MetaWidth, MetaHeight, MetaVideoCodec, MetaAudioCodec, MetaPosterKey, MetaPosterDisk, MetaHLSKey, MetaHLSDisk}

// Names the plugin passes to a Location: DerivedPoster for a poster,
// DerivedHLS for an HLS ladder (WithHLS).
const (
	DerivedPoster = "poster"
	DerivedHLS    = "hls"
)

// Location chooses where the plugin stores a derived object of f (name is
// DerivedPoster or DerivedHLS): the name of a disk of the Store's Manager
// ("" for f's own disk) and a key prefix. The plugin appends a random
// part and the suffix (PosterSuffix for a poster, "/" and the ladder's
// file names for HLS) to the prefix, so every generation writes keys no
// other generation writes: a replacement's poster or ladder never
// overwrites the current one before the replacement commits, and the
// deferred deletion of an old one never removes a new one. An error fails
// closed: nothing is written and the upload or replacement fails with it.
type Location func(f records.File, name string) (disk, prefix string, err error)

// defaultLocation stores a poster beside its video, on the video's disk,
// as <video key><PosterInfix><random><PosterSuffix>, and an HLS ladder in
// a directory beside it, <video key><HLSInfix><random>/.
func defaultLocation(f records.File, name string) (string, string, error) {
	if name == DerivedHLS {
		return "", f.Key + HLSInfix, nil
	}
	return "", f.Key + PosterInfix, nil
}

// HLSInfix starts the name of a ladder's directory beside its video.
const HLSInfix = ".hls-"

// Poster objects are stored next to their video as
// <video key><PosterInfix><random><PosterSuffix>. The random part keeps a
// replacement's poster from overwriting the current one before the
// replacement commits.
const (
	PosterInfix       = ".poster-"
	PosterSuffix      = ".jpg"
	PosterContentType = "image/jpeg"
	// posterTokenLen is how many characters of rand.Text name a poster.
	posterTokenLen = 10
)

// Plugin is a records plugin that stores every video file's metadata
// (MetaDuration, MetaWidth, …) when it is recorded and when its content is
// replaced, and with WithPoster a poster frame beside it. Construct it
// with NewPlugin and install it with records.WithPlugins.
//
// A file ffprobe cannot read is recorded without metadata, or refused
// with WithStrict. A missing program is an error either way: every video
// upload fails with CodeUnavailable until FFmpeg is installed, rather
// than silently recording videos without metadata.
type Plugin struct {
	tool     Tool
	strict   bool
	poster   bool
	posterAt time.Duration
	// location places posters and ladders; defaultLocation unless
	// WithLocation.
	location Location
	// hls, when set (WithHLS), is the ladder written for every video.
	hls *HLS
}

// PluginOption configures a Plugin.
type PluginOption func(*Plugin)

// WithStrict refuses a video file ffprobe cannot read (CodeRejected), so
// a corrupt or disguised file is never recorded.
func WithStrict() PluginOption { return func(p *Plugin) { p.strict = true } }

// WithPoster also stores the frame at offset at (the first frame for a
// shorter video) as a JPEG next to each video, records its key in
// MetaPosterKey, and deletes it with the file (after the purge commits)
// or when the file's content is replaced.
func WithPoster(at time.Duration) PluginOption {
	return func(p *Plugin) { p.poster, p.posterAt = true, at }
}

// WithHLS also encodes every video as an HLS ladder (Tool.HLS with opts)
// into a directory of its own, records its master playlist's key in
// MetaHLSKey, and handles the ladder as it does a poster: it is deleted
// with the file (after the purge commits) or when the file's content is
// replaced, moved with the file to another disk, kept from the orphan
// sweep while its file row names it, and placed by WithLocation (name
// DerivedHLS). A ladder whose encode fails is deleted at once, and one
// written by a transaction that rolls back is deleted then; a video HLS
// refuses (longer than opts.MaxDuration) is handled like one ffprobe
// cannot read (see WithStrict). The encode runs inside the upload's
// transaction, so it suits short clips; for long videos run Tool.HLS
// from a job instead. Off by default.
func WithHLS(opts HLS) PluginOption {
	return func(p *Plugin) { p.hls = &opts }
}

// WithLocation stores posters where l says instead of beside their video
// on its disk: a public CDN disk while the videos stay private, a cheaper
// bucket, or a CDN-friendly path. The poster's disk is recorded in
// MetaPosterDisk when it is not the file's, so PosterLocation, the purge
// and a replacement find it. A nil l keeps the default.
func WithLocation(l Location) PluginOption {
	return func(p *Plugin) {
		if l != nil {
			p.location = l
		}
	}
}

// NewPlugin returns a Plugin running t.
func NewPlugin(t Tool, opts ...PluginOption) *Plugin {
	p := &Plugin{tool: t, location: defaultLocation}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// BeforeCreate implements records.FileHooks: the poster keys a caller
// put in f's metadata are dropped (clearRefs), then a video is described.
func (p *Plugin) BeforeCreate(ctx context.Context, s *records.Store, f *records.File) error {
	clearRefs(f)
	return p.describe(ctx, s, f)
}

// refKeys name the plugin's own objects: the purge deletes what they name,
// OwnedObjects keeps it from the orphan sweep and PosterLocation hands it
// out. Only the plugin may set them, so they are dropped from a new file
// of any type, while every other key of a type the plugin does not
// describe stays the application's (see describe).
var refKeys = []string{MetaPosterKey, MetaPosterDisk, MetaHLSKey, MetaHLSDisk}

// clearRefs removes refKeys from f's metadata, copying the map, which the
// caller may share.
func clearRefs(f *records.File) {
	if !slices.ContainsFunc(refKeys, func(k string) bool { _, ok := f.Metadata[k]; return ok }) {
		return
	}
	meta := maps.Clone(f.Metadata)
	for _, k := range refKeys {
		delete(meta, k)
	}
	if len(meta) == 0 {
		meta = nil
	}
	f.Metadata = meta
}

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks: the poster and the ladder go
// with the file, after the purge commits.
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	deleteDerived(ctx, s, f)
	return nil
}

// BeforeRelease implements records.ReleaseHooks: a file recorded or
// replaced while quarantined was not read then (see describe), so it is
// described when its scan passes.
func (p *Plugin) BeforeRelease(ctx context.Context, s *records.Store, f *records.File) error {
	return p.describe(ctx, s, f)
}

// BeforeReplace implements records.ReplaceHooks: the new content is
// described (and gets a new poster), and the old poster is deleted once
// the replacement commits.
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, current records.File, next *records.File) error {
	if IsVideo(current.ContentType) && !IsVideo(next.ContentType) {
		dropStale(next)
	}
	if err := p.describe(ctx, s, next); err != nil {
		return err
	}
	deleteDerived(ctx, s, current)
	return nil
}

// deleteDerived deletes f's poster and every file of its ladder after the
// transaction in ctx commits.
func deleteDerived(ctx context.Context, s *records.Store, f records.File) {
	if disk, key, ok := PosterLocation(f); ok {
		releaseAfterCommit(ctx, s, records.File{Disk: disk, Key: key})
	}
	if disk, key, ok := HLSLocation(f); ok {
		releaseDirAfterCommit(ctx, s, disk, hlsDir(key))
	}
}

// siblingTypes maps each key the plugin shares with another plugin to
// the types that plugin describes: width and height to the dimensions
// plugin's images, duration_ms and audio_codec to contrib/audio's audio
// files. On a replacement by such a type the key is that plugin's to
// set, so dropStale leaves it, whichever order the hooks run in.
var siblingTypes = map[string]string{
	MetaWidth: "image/", MetaHeight: "image/",
	MetaDuration: "audio/", MetaAudioCodec: "audio/",
}

// dropStale removes from next the keys the plugin wrote for current, a
// video whose content next (no longer a video) replaces, so they do not
// outlive it; a shared key is left when next is its sibling's type (see
// siblingTypes). Without that sibling installed, such a key keeps the
// video's value. It copies the metadata map, which the caller may share.
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

// describe sets a video's metadata from its object, replacing whatever
// the plugin set before (only clearing it for a quarantined video). Any
// other type is left untouched, keys and all: they may be another
// plugin's or the application's (except the poster keys, which
// BeforeCreate drops from every new file; see clearRefs). It copies the metadata map before
// changing it, because the caller's map may be shared.
func (p *Plugin) describe(ctx context.Context, s *records.Store, f *records.File) error {
	if !IsVideo(f.ContentType) {
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
	// described (with its poster) on release (BeforeRelease).
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
	var poster []byte
	var ladder hlsWrite
	err = p.tool.withWorkFile(rc, func(path string) error {
		var err error
		if info, err = p.tool.probePath(ctx, path); err != nil {
			return err
		}
		if p.poster {
			if poster, err = p.tool.posterPath(ctx, path, p.posterAt); err != nil {
				return err
			}
		}
		if p.hls == nil {
			return nil
		}
		if info.Duration > p.hls.maxDuration() {
			return filesystem.Errorf(filesystem.CodeRejected, "video is longer than %s", p.hls.maxDuration())
		}
		if ladder, err = p.hlsTarget(s, *f); err != nil {
			return err
		}
		return p.tool.hlsPath(ctx, path, info, *p.hls, ladder.write)
	})
	if err != nil {
		// A ladder cut short is deleted at once: the create may still
		// commit (a rejected video without WithStrict).
		ladder.discard(context.WithoutCancel(ctx))
		if !p.strict && filesystem.CodeOf(err) == filesystem.CodeRejected {
			return nil
		}
		return err
	}
	meta[MetaDuration] = strconv.FormatInt(info.Duration.Milliseconds(), 10)
	meta[MetaWidth] = strconv.Itoa(info.Width)
	meta[MetaHeight] = strconv.Itoa(info.Height)
	meta[MetaVideoCodec] = info.VideoCodec
	if info.AudioCodec != "" {
		meta[MetaAudioCodec] = info.AudioCodec
	}
	if ladder.dst != nil {
		// Object first, row second: a rollback must not leave the ladder.
		s.OnRollback(ctx, ladder.discard)
		meta[MetaHLSKey] = ladder.dir + HLSMaster
		if ladder.disk != f.Disk {
			meta[MetaHLSDisk] = ladder.disk
		}
	}
	if poster == nil {
		return nil
	}
	diskName, prefix, err := p.location(*f, DerivedPoster)
	if err != nil {
		return err
	}
	if diskName == "" {
		diskName = f.Disk
	}
	dst, err := s.Disks().Disk(diskName)
	if err != nil {
		return err
	}
	key := prefix + strings.ToLower(rand.Text()[:posterTokenLen]) + PosterSuffix
	if _, err := dst.WriteBytes(ctx, key, poster, filesystem.WriteOptions{ContentType: PosterContentType}); err != nil {
		return err
	}
	// Object first, row second: a rollback must not leave the poster.
	// The Store runs rollback work without the request's cancellation.
	s.OnRollback(ctx, func(ctx context.Context) { _ = dst.Delete(ctx, key) })
	meta[MetaPosterKey] = key
	if diskName != f.Disk {
		meta[MetaPosterDisk] = diskName
	}
	return nil
}

// Meta is the metadata the plugin recorded for a video file.
type Meta struct {
	Duration               time.Duration
	Width, Height          int
	VideoCodec, AudioCodec string
}

// Of returns the metadata recorded for f; ok is false when there is none
// (not a video, unreadable, or recorded before the plugin was installed).
func Of(f records.File) (m Meta, ok bool) {
	ms, errD := strconv.ParseInt(f.Metadata[MetaDuration], 10, 64)
	w, errW := strconv.Atoi(f.Metadata[MetaWidth])
	h, errH := strconv.Atoi(f.Metadata[MetaHeight])
	if errD != nil || errW != nil || errH != nil {
		return Meta{}, false
	}
	return Meta{
		Duration: time.Duration(ms) * time.Millisecond, Width: w, Height: h,
		VideoCodec: f.Metadata[MetaVideoCodec], AudioCodec: f.Metadata[MetaAudioCodec],
	}, true
}

// PosterKey returns the key of f's poster (WithPoster); ok is false when
// there is none. The poster is on f's disk unless WithLocation chose
// another: PosterLocation returns both.
func PosterKey(f records.File) (string, bool) {
	key := f.Metadata[MetaPosterKey]
	return key, key != ""
}

// HLSLocation returns the disk and the master playlist's key of f's HLS
// ladder (WithHLS): the disk is MetaHLSDisk when WithLocation put it
// elsewhere, else f's own. Every file of the ladder is in the master's
// directory. ok is false when there is no ladder.
func HLSLocation(f records.File) (disk, key string, ok bool) {
	return refLocation(f, hlsRef)
}

// hlsWrite is one ladder being written by describe: where it goes and
// every key written so far, so a failure or a rollback deletes exactly
// those.
type hlsWrite struct {
	dst       *filesystem.Disk
	disk, dir string
	written   []string
}

// hlsTarget places f's ladder: a fresh directory under the Location's
// prefix, on its disk.
func (p *Plugin) hlsTarget(s *records.Store, f records.File) (hlsWrite, error) {
	diskName, prefix, err := p.location(f, DerivedHLS)
	if err != nil {
		return hlsWrite{}, err
	}
	if diskName == "" {
		diskName = f.Disk
	}
	dst, err := s.Disks().Disk(diskName)
	if err != nil {
		return hlsWrite{}, err
	}
	return hlsWrite{dst: dst, disk: diskName, dir: prefix + strings.ToLower(rand.Text()[:posterTokenLen]) + "/"}, nil
}

// write is the HLSWriter storing each file under w's directory.
func (w *hlsWrite) write(ctx context.Context, name, contentType string, body io.Reader) error {
	key := w.dir + name
	if _, err := w.dst.Write(ctx, key, body, filesystem.WriteOptions{ContentType: contentType}); err != nil {
		return err
	}
	w.written = append(w.written, key)
	return nil
}

// discard deletes every file w wrote, best effort: what fails to go is
// an orphan no row names, which SweepOrphans removes.
func (w *hlsWrite) discard(ctx context.Context) {
	if w.dst == nil || len(w.written) == 0 {
		return
	}
	_ = w.dst.DeleteKeys(ctx, w.written)
	w.written = nil
}

// PosterLocation returns the disk and key of f's poster (WithPoster): the
// disk is MetaPosterDisk when WithLocation put it elsewhere, else f's
// own. ok is false when there is no poster.
func PosterLocation(f records.File) (disk, key string, ok bool) {
	key, ok = PosterKey(f)
	if !ok {
		return "", "", false
	}
	disk = f.Metadata[MetaPosterDisk]
	if disk == "" {
		disk = f.Disk
	}
	return disk, key, true
}
