package imaging

import (
	"context"
	"math"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// DimensionLimits bound an image's displayed size (after EXIF rotation).
// Zero fields are not checked.
type DimensionLimits struct {
	MinWidth, MinHeight int
	MaxWidth, MaxHeight int
	// MaxPixels bounds Width × Height: a 10,000 × 10,000 image passes
	// both side limits of 10,000 yet costs 400 MB to decode.
	MaxPixels int64
	// Aspect is the required width ÷ height (16.0/9 for a video poster,
	// 1 for an avatar), within AspectTolerance; 0 is not checked.
	Aspect float64
	// AspectTolerance is how far the ratio may be from Aspect, as a
	// fraction of Aspect; 0 means DefaultAspectTolerance. Phones and
	// editors round sizes, so an exact ratio would refuse 1080 × 1921.
	AspectTolerance float64
}

// DefaultAspectTolerance is the AspectTolerance when it is 0: 1%.
const DefaultAspectTolerance = 0.01

// svgType is not checked: a vector image has no pixel size to bound.
const svgType = "image/svg+xml"

// DimensionsOption configures CheckDimensions.
type DimensionsOption func(*dimensionsConfig)

type dimensionsConfig struct{ proc Processor }

// WithProber reads headers with proc (a Pipeline with codec modules, for
// HEIC or AVIF uploads) instead of Default().
func WithProber(proc Processor) DimensionsOption {
	return func(c *dimensionsConfig) { c.proc = proc }
}

// CheckDimensions returns an upload.Inspector (for upload.Policy.Inspect)
// that refuses an image outside l with CodeRejected and a message saying
// which bound it broke, such as an avatar under 200 × 200 or a banner wider
// than 4,000 pixels. An Inspector runs on every upload path, the presigned
// one included, so the check cannot be skipped by uploading straight to the
// bucket.
//
// Only the header is read (ProbeBytes through Disk.ReadRange), so a
// decompression bomb costs nothing. Files whose content type is not an
// image, and SVG (which has no pixel size), pass; an image whose header
// cannot be read is refused, since its size cannot be verified. A limit
// that is negative, or a minimum above its maximum, is CodeInvalidArgument
// here, at construction.
func CheckDimensions(l DimensionLimits, options ...DimensionsOption) (upload.Inspector, error) {
	if l.MinWidth < 0 || l.MinHeight < 0 || l.MaxWidth < 0 || l.MaxHeight < 0 || l.MaxPixels < 0 || l.Aspect < 0 || l.AspectTolerance < 0 ||
		(l.MaxWidth > 0 && l.MinWidth > l.MaxWidth) || (l.MaxHeight > 0 && l.MinHeight > l.MaxHeight) {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: dimension limits %+v are inconsistent", l)
	}
	c := dimensionsConfig{proc: defaultPipeline}
	for _, o := range options {
		o(&c)
	}
	return func(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
		if !strings.HasPrefix(r.ContentType, "image/") || r.ContentType == svgType {
			return nil
		}
		rc, err := disk.ReadRange(ctx, r.Key, filesystem.ByteRange{Length: ProbeBytes})
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
		info, err := c.proc.Probe(rc)
		if err != nil {
			return err
		}
		return l.check(info.Width, info.Height)
	}, nil
}

// check reports the first bound w × h breaks.
func (l DimensionLimits) check(w, h int) error {
	refuse := func(why string, args ...any) error {
		return filesystem.Errorf(filesystem.CodeRejected, "image is %dx%d; "+why, append([]any{w, h}, args...)...)
	}
	switch {
	case w < l.MinWidth:
		return refuse("it must be at least %d pixels wide", l.MinWidth)
	case h < l.MinHeight:
		return refuse("it must be at least %d pixels high", l.MinHeight)
	case l.MaxWidth > 0 && w > l.MaxWidth:
		return refuse("it must be at most %d pixels wide", l.MaxWidth)
	case l.MaxHeight > 0 && h > l.MaxHeight:
		return refuse("it must be at most %d pixels high", l.MaxHeight)
	case l.MaxPixels > 0 && int64(w)*int64(h) > l.MaxPixels:
		return refuse("it must have at most %d pixels", l.MaxPixels)
	case l.Aspect > 0 && !l.aspectOK(w, h):
		return refuse("its shape must be %.2f:1 (within %g%%), not %.2f:1", l.Aspect, l.tolerance()*100, float64(w)/float64(h))
	}
	return nil
}

func (l DimensionLimits) tolerance() float64 {
	if l.AspectTolerance == 0 {
		return DefaultAspectTolerance
	}
	return l.AspectTolerance
}

// aspectOK reports whether w ÷ h is within the tolerance of Aspect. A
// zero height gives an infinite or NaN ratio, which is never within it.
func (l DimensionLimits) aspectOK(w, h int) bool {
	return math.Abs(float64(w)/float64(h)-l.Aspect) <= l.Aspect*l.tolerance()
}
