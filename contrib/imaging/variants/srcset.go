package variants

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// Candidate is one image of a srcset.
type Candidate struct {
	// Name is the declared variant.
	Name string
	// URL is its link: public for a public file on a disk that has public
	// URLs, otherwise signed for the requested lifetime.
	URL string
	// Width is the variant's real width in pixels, which can be less
	// than its transform asks for (ModeFit never enlarges).
	Width int64
	// Density is the x descriptor DensitySrcSet gave it: its width over
	// the 1x version's. SrcSet, which uses w descriptors, leaves it 0.
	Density float64
}

// SrcSet returns the HTML srcset of a file's named variants, such as
// "https://cdn/…/w320.jpg 320w, https://cdn/…/w640.jpg 640w", and the
// candidates it was built from. Each variant is generated if needed, as
// Get does. Name variants of one aspect ratio and growing widths (w320,
// w640, w1280): the browser picks by width alone.
//
// Width descriptors are the variants' real widths, smallest first. Two
// variants that came out the same width (a small original that no larger
// variant can enlarge) give one candidate, the first named, so the
// browser never downloads a larger file for the same pixels. No names is
// CodeInvalidArgument; any variant's failure fails the call, so a page
// never gets a srcset that silently lacks a size. A view-once or expired
// file has none (CodeNotFound).
func (p *Plugin) SrcSet(ctx context.Context, s *records.Store, scope, fileID string, ttl time.Duration, names ...string) (string, []Candidate, error) {
	if len(names) == 0 {
		return "", nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: srcset needs at least one variant name")
	}
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return "", nil, err
	}
	if err := linkable(s, f); err != nil {
		return "", nil, err
	}
	var out []Candidate
	for _, name := range names {
		v, err := p.variant(ctx, s, f, name)
		if err != nil {
			return "", nil, err
		}
		if slices.ContainsFunc(out, func(c Candidate) bool { return c.Width == v.Width }) {
			continue
		}
		u, err := variantURL(ctx, s, f, v, ttl)
		if err != nil {
			return "", nil, err
		}
		out = append(out, Candidate{Name: name, URL: u, Width: v.Width})
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return int(a.Width - b.Width) })
	parts := make([]string, len(out))
	for i, c := range out {
		parts[i] = c.URL + " " + strconv.FormatInt(c.Width, 10) + "w"
	}
	return strings.Join(parts, ", "), out, nil
}
