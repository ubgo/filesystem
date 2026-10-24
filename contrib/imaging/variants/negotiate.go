package variants

import (
	"mime"
	"slices"
	"strconv"
	"strings"

	"github.com/ubgo/filesystem/contrib/imaging"
)

// negotiable keeps the formats of want the processor can be shown to
// write, in order. A processor that cannot say (no Encodes method) gets
// none: a format it could not write would fail every request that asked
// for it, while the declared format always works.
func (p *Plugin) negotiable(want []imaging.Format) []imaging.Format {
	enc, ok := p.processor.(interface{ Encodes(imaging.Format) bool })
	if !ok {
		return nil
	}
	var out []imaging.Format
	for _, f := range want {
		if enc.Encodes(f) {
			out = append(out, f)
		}
	}
	return out
}

// negotiate returns the first of formats the Accept header names
// explicitly with a non-zero quality, or "" for the declared format.
// Wildcards ("image/*", "*/*") do not count: every browser sends them,
// and only one that names image/webp or image/avif can display it.
func (p *Plugin) negotiate(accept string, formats []imaging.Format) imaging.Format {
	accepted := map[string]bool{}
	for _, part := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if v, ok := params["q"]; ok {
			if q, err = strconv.ParseFloat(v, 64); err != nil {
				continue
			}
		}
		if q > 0 {
			accepted[mediaType] = true
		}
	}
	for _, f := range formats {
		if ct, _ := p.formatInfo(f); accepted[ct] {
			return f
		}
	}
	return ""
}

// sibling reports whether digest names transform t in some output format:
// the declared variant or one of its negotiated siblings, which Prune
// keeps. The formats tried are FormatValues, t's own and every format the
// processor says it encodes (a Formats method, as *imaging.Pipeline has),
// so a variant in a custom format is not pruned as stale.
func (p *Plugin) sibling(t imaging.Transform, digest string) bool {
	formats := append([]imaging.Format{t.Format}, imaging.FormatValues...)
	if lister, ok := p.processor.(interface{ Formats() []imaging.Format }); ok {
		formats = append(formats, lister.Formats()...)
	}
	return slices.ContainsFunc(formats, func(f imaging.Format) bool {
		t.Format = f
		return t.Digest() == digest
	})
}
