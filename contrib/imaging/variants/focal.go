package variants

import (
	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/records"
)

// FocalSource tells the plugin where a file's focal point is, for
// variants declared with imaging.GravityFocal, and which event says it
// changed. The focal plugin (contrib/imaging/focal) is one; an app that
// detects faces can be another.
type FocalSource interface {
	// Focal returns f's focal point; ok is false when it has none (the
	// crop is then centred).
	Focal(f records.File) (pt imaging.Focal, ok bool)
	// FocalChanged is the event kind emitted, inside the transaction that
	// changes a focal point, with the file's ID: the plugin drops the
	// file's variants on it, so they are made again around the new point.
	FocalChanged() records.EventKind
}

// WithFocal crops GravityFocal variants around src's focal point. Without
// it, GravityFocal crops from the centre. A nil src is
// CodeInvalidArgument.
func WithFocal(src FocalSource) Option {
	return func(p *Plugin) error {
		if src == nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: nil focal source")
		}
		p.focal = src
		return nil
	}
}

// focalPoint is a file's focal point as the plugin uses it; comparable,
// so a change between two reads of a file is one comparison.
type focalPoint struct {
	pt imaging.Focal
	ok bool
}

// focalOf is f's focal point: none without a source or a point.
func (p *Plugin) focalOf(f records.File) focalPoint {
	if p.focal == nil {
		return focalPoint{}
	}
	pt, ok := p.focal.Focal(f)
	return focalPoint{pt: pt, ok: ok}
}

// withFocal sets t's focal point to fp when t crops by GravityFocal.
func withFocal(fp focalPoint, t imaging.Transform) imaging.Transform {
	if fp.ok && t.Gravity == imaging.GravityFocal {
		t.Focal = &fp.pt
	}
	return t
}
