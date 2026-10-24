// Package dimensions is a records plugin that stores the width and height
// of every raster image in its file's metadata, when the file is recorded
// and when its content is replaced. Layouts need them before an image
// loads (aspect-ratio boxes, masonry grids, srcset choices), and reading
// them later would mean fetching every object again.
//
// Only the header is read (imaging.Probe), so a pixel bomb costs nothing.
// Dimensions are as displayed: a phone's portrait photo stored sideways
// with an EXIF rotation reports its portrait size.
package dimensions

import (
	"context"
	"encoding/base64"
	"image"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/records"
)

// PluginName is the plugin's name.
const PluginName = "dimensions"

// Metadata keys the plugin sets: width and height as decimal strings, and
// with WithThumbHash the ThumbHash in standard base64.
const (
	MetaWidth     = "width"
	MetaHeight    = "height"
	MetaThumbHash = "thumbhash"
	// MetaColour is the dominant colour as "#rrggbb" (WithDominantColour).
	MetaColour = "colour"
)

// metaKeys are every key the plugin owns. They are cleared only when the
// plugin measures a file of a type it reads (see measure), never on
// another type: width and height are shared with contrib/video, so
// clearing them on every file would wipe what that plugin recorded for a
// video.
var metaKeys = []string{MetaWidth, MetaHeight, MetaThumbHash, MetaColour}

// siblingTypes maps each key the plugin shares with another plugin to
// the types that plugin describes: width and height to contrib/video's
// videos. On a replacement by such a type the key is that plugin's to
// set, so dropStale leaves it, whichever order the hooks run in.
var siblingTypes = map[string]string{MetaWidth: "video/", MetaHeight: "video/"}

// RasterTypes are the content types the plugin reads: the ones imaging
// decodes. Other files, SVG included, are left alone.
var RasterTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/tiff"}

// Plugin records dimensions. Construct it with New and install it with
// records.WithPlugins.
type Plugin struct {
	strict bool
	// processor reads headers; nil means imaging.Default().
	processor imaging.Processor
	// types are the content types measured; nil means RasterTypes.
	types []string
	// thumbHash also stores a ThumbHash (WithThumbHash), within limits.
	thumbHash bool
	colour    bool
	// hashLimits and colourLimits bound the full decode each needs; with
	// equal limits the two share one decode.
	hashLimits, colourLimits imaging.Options
}

// WithDominantColour also stores each image's dominant colour
// (MetaColour, read back with ColourOf), for a placeholder background
// while it loads. Like WithThumbHash it needs the whole image decoded,
// within its own limits (an image over them gets no colour); when
// WithThumbHash has the same limits, the two share one decode.
func WithDominantColour(limits imaging.Options) Option {
	return func(p *Plugin) { p.colour, p.colourLimits = true, limits }
}

// ColourOf returns the dominant colour recorded for f
// (WithDominantColour), as "#rrggbb"; ok is false when there is none.
func ColourOf(f records.File) (hex string, ok bool) {
	hex, ok = f.Metadata[MetaColour]
	return hex, ok && hex != ""
}

// WithThumbHash also stores each image's ThumbHash (MetaThumbHash, read
// back with ThumbHashOf): a placeholder of about 25 bytes that a page
// draws, blurred, while the image loads. Unlike the dimensions it needs
// the whole image decoded, so it reads the full object, within limits
// (zero fields: imaging's defaults, 50 megapixels and 50 MiB). An image
// over the limits or that does not decode gets no hash, or is refused
// with WithStrict.
func WithThumbHash(limits imaging.Options) Option {
	return func(p *Plugin) { p.thumbHash, p.hashLimits = true, limits }
}

// Option configures a Plugin.
type Option func(*Plugin)

// WithStrict refuses a raster image whose header cannot be read
// (CodeRejected), so a corrupt image is never recorded. Without it such a
// file is recorded without dimensions.
func WithStrict() Option { return func(p *Plugin) { p.strict = true } }

// WithProcessor reads headers with proc instead of imaging.Default(), and
// also measures the given content types: a Pipeline with the HEIC codec
// module and "image/heic", for example. A nil proc keeps the default.
func WithProcessor(proc imaging.Processor, types ...string) Option {
	return func(p *Plugin) {
		p.processor = proc
		p.types = append(slices.Clone(RasterTypes), types...)
	}
}

// New returns a Plugin.
func New(opts ...Option) *Plugin {
	p := &Plugin{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name implements records.Plugin.
func (p *Plugin) Name() string { return PluginName }

// BeforeCreate implements records.FileHooks.
func (p *Plugin) BeforeCreate(ctx context.Context, s *records.Store, f *records.File) error {
	return p.measure(ctx, s, f)
}

// AfterCreate implements records.FileHooks.
func (p *Plugin) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks.
func (p *Plugin) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// BeforeRelease implements records.ReleaseHooks: a file recorded or
// replaced while quarantined was not read then (see measure), so it is
// measured when its scan passes.
func (p *Plugin) BeforeRelease(ctx context.Context, s *records.Store, f *records.File) error {
	return p.measure(ctx, s, f)
}

// BeforeReplace implements records.ReplaceHooks: the new content is
// measured, and the old dimensions never outlive it: when it is no
// longer a type the plugin reads, the keys it wrote for the old content
// are dropped (dropStale).
func (p *Plugin) BeforeReplace(ctx context.Context, s *records.Store, current records.File, next *records.File) error {
	if p.reads(current) && !p.reads(*next) {
		dropStale(next)
	}
	return p.measure(ctx, s, next)
}

// reads reports whether f is a type the plugin measures.
func (p *Plugin) reads(f records.File) bool { return slices.Contains(p.measured(), f.ContentType) }

// dropStale removes from next the keys the plugin wrote for an image
// whose content next (no longer a type the plugin reads) replaces, so
// they do not outlive it; a shared key is left when next is its
// sibling's type (see siblingTypes). Without that sibling installed,
// such a key keeps the image's value. It copies the metadata map, which
// the caller may share.
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

// measure sets f's dimensions from its object's header when f is a type
// the plugin reads, replacing whatever it set before (only clearing them
// for a quarantined file). It copies the metadata map before changing
// it, because the caller's map may be shared.
func (p *Plugin) measure(ctx context.Context, s *records.Store, f *records.File) error {
	// Another type is left untouched, keys and all: they may be another
	// plugin's (a video's width and height) or the application's.
	if !p.reads(*f) {
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
	// A quarantined file's bytes are unscanned and are not read: its
	// dimensions are measured on release (BeforeRelease).
	if f.Quarantined() {
		return nil
	}
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return err
	}
	rc, err := disk.ReadRange(ctx, f.Key, filesystem.ByteRange{Length: imaging.ProbeBytes})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	info, err := p.prober().Probe(rc)
	if err != nil {
		if !p.strict && filesystem.CodeOf(err) == filesystem.CodeRejected {
			return nil
		}
		return err
	}
	meta[MetaWidth] = strconv.Itoa(info.Width)
	meta[MetaHeight] = strconv.Itoa(info.Height)
	if !p.thumbHash && !p.colour {
		return nil
	}
	var hashed image.Image
	if p.thumbHash {
		if hashed, err = p.preview(ctx, disk, f.Key, p.hashLimits); err != nil {
			return err
		}
		if hashed != nil {
			meta[MetaThumbHash] = base64.StdEncoding.EncodeToString(imaging.ThumbHash(hashed))
		}
	}
	if p.colour {
		small := hashed
		if !p.thumbHash || p.colourLimits != p.hashLimits {
			if small, err = p.preview(ctx, disk, f.Key, p.colourLimits); err != nil {
				return err
			}
		}
		if small == nil {
			return nil
		}
		if c, ok := imaging.DominantColour(small); ok {
			meta[MetaColour] = imaging.Hex(c)
		}
	}
	return nil
}

// preview reads the whole object and decodes it within limits into a
// small copy. An image over the limits, or that does not decode, gives
// nil (or an error with WithStrict); a read failure is always an error.
func (p *Plugin) preview(ctx context.Context, disk *filesystem.Disk, key string, limits imaging.Options) (image.Image, error) {
	full, err := disk.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = full.Close() }()
	small, err := imaging.Preview(p.prober(), full, limits)
	if err != nil {
		if !p.strict && slices.Contains([]filesystem.Code{filesystem.CodeRejected, filesystem.CodeTooLarge}, filesystem.CodeOf(err)) {
			return nil, nil
		}
		return nil, err
	}
	return small, nil
}

// ThumbHashOf returns the ThumbHash recorded for f (WithThumbHash); ok is
// false when there is none.
func ThumbHashOf(f records.File) (hash []byte, ok bool) {
	hash, err := base64.StdEncoding.DecodeString(f.Metadata[MetaThumbHash])
	if err != nil || len(hash) == 0 {
		return nil, false
	}
	return hash, true
}

// Of returns the dimensions recorded for f; ok is false when there are
// none (not a raster image, unreadable, or recorded before the plugin was
// installed).
func Of(f records.File) (width, height int, ok bool) {
	w, errW := strconv.Atoi(f.Metadata[MetaWidth])
	h, errH := strconv.Atoi(f.Metadata[MetaHeight])
	if errW != nil || errH != nil {
		return 0, 0, false
	}
	return w, h, true
}

// measured is the content types the plugin reads.
func (p *Plugin) measured() []string {
	if p.types == nil {
		return RasterTypes
	}
	return p.types
}

func (p *Plugin) prober() imaging.Processor {
	if p.processor == nil {
		return imaging.Default()
	}
	return p.processor
}
