package variants

import (
	"context"
	"image"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/records"
)

// Device-density versions of a variant: the same picture at 2x or 3x the
// pixels, for the screens iOS calls @2x and @3x, Android calls xhdpi and
// xxhdpi, and a browser reports as its devicePixelRatio (Cloudinary's
// dpr). A variant is declared once at its base (1x) size, and
// WithDensities derives the others from it.

// MaxDensity bounds a declared density factor: 4 is Android's xxxhdpi,
// the densest screen class any platform names, so a larger factor only
// spends storage on pixels no device shows.
const MaxDensity = 4.0

// densitySuffix ends every density name: "avatar-2x" is avatar at 2x.
const densitySuffix = "x"

// densityDecimal replaces the decimal point of a fractional factor in a
// name, because names appear in storage keys and URLs and their pattern
// has no dot: 1.5 is "avatar-1_5x" (Android's hdpi).
const densityDecimal = "_"

// descriptorPrecision is how many decimals an x descriptor keeps: a
// density the source could not fully reach (1.6666…) is advertised as
// 1.67x, which a browser displays less than a CSS pixel off.
const descriptorPrecision = 100

// defaultDensities are the factors WithDensities declares when given none:
// iOS's @2x and @3x, which are also Android's xhdpi and xxhdpi.
func defaultDensities() []float64 { return []float64{2, 3} }

// density says that a declared name is a density version of base.
type density struct {
	base   string
	factor float64
}

// DensityName is the declared name of base at factor: base itself at 1,
// otherwise "<base>-<factor>x" with a fractional factor's point written
// as an underscore ("avatar-2x", "avatar-1_5x"). Use it to build the
// names an iOS or Android app requests, or to pass to Get, URL or
// WithEager.
func DensityName(base string, factor float64) string {
	if factor == 1 {
		return base
	}
	f := strings.ReplaceAll(strconv.FormatFloat(factor, 'f', -1, 64), ".", densityDecimal)
	return base + "-" + f + densitySuffix
}

// WithDensities declares density versions of the declared variant base:
// for each factor, a variant named DensityName(base, factor) whose
// transform is base's with Width and Height multiplied by the factor
// (rounded down). No factors means 2 and 3; base itself is the 1x. Each
// version is an ordinary variant (generated on first request, served by
// Get, URL and Handler, kept by Prune, linked by SrcSet), and
// DensitySrcSet links them all with x descriptors.
//
// A density version never enlarges its source. When the original (or the
// transform's Crop) has fewer pixels than the factor needs, the version is
// made at the largest factor the source supports, never below 1x: a
// 900-pixel photo of a 400-pixel ModeFill variant gives a 2x of 800 pixels
// and a "3x" of 900 (2.25x), and a 300-pixel photo gives 2x and 3x versions
// the size of its 1x. Those versions still exist, so an app that requests
// "avatar-3x" by name always gets an image, and DensitySrcSet advertises
// their real density and leaves out any that adds no pixels. ModeFit and
// ModePad are capped the same way (ModePad would otherwise pad a small
// image onto a canvas the screen then shrinks).
//
// A factor must be within 1..MaxDensity (1 is base itself); base must be
// declared, not itself a density version, and given densities once; a
// derived name must not already be declared and must fit the name pattern; a scaled size must pass imaging.Transform.Validate.
// Each failure is CodeInvalidArgument. List WithDensities before
// WithEager and any WithHeaders that names a density version, since those
// options only see variants declared before them. A density version
// without headers of its own takes base's (WithHeaders).
func WithDensities(base string, factors ...float64) Option {
	return func(p *Plugin) error {
		t, ok := p.transforms[base]
		switch {
		case !ok:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: densities of undeclared variant %q", base)
		case p.densities[base] != nil:
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: densities of %q declared twice", base)
		}
		if _, ok := p.densityOf[base]; ok {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: %q is a density version; declare densities of its base", base)
		}
		if len(factors) == 0 {
			factors = defaultDensities()
		}
		list := []float64{1}
		for _, k := range factors {
			if !(k >= 1 && k <= MaxDensity) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: density %v of %q is not within 1..%v", k, base, MaxDensity)
			}
			if slices.Contains(list, k) {
				if k == 1 {
					continue
				}
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: density %v of %q listed twice", k, base)
			}
			list = append(list, k)
		}
		slices.Sort(list)
		added := map[string]imaging.Transform{}
		of := map[string]density{}
		for _, k := range list[1:] {
			name := DensityName(base, k)
			if _, ok := p.transforms[name]; ok {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: density name %q is already declared", name)
			}
			if !namePattern.MatchString(name) {
				return filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: density name %q is not a valid name", name)
			}
			// dt is checked with every transform once New knows the
			// processor (ValidateFor), so a too-large density fails New.
			dt := scaled(t, k)
			added[name], of[name] = dt, density{base: base, factor: k}
		}
		// Nothing is declared until every version is named.
		maps.Copy(p.transforms, added)
		maps.Copy(p.densityOf, of)
		p.densities[base] = list
		return nil
	}
}

// scaled is t at factor k: Width and Height multiplied and rounded down,
// so rounding never asks for a pixel more than k does.
func scaled(t imaging.Transform, k float64) imaging.Transform {
	t.Width = max(1, int(float64(t.Width)*k))
	t.Height = max(1, int(float64(t.Height)*k))
	return t
}

// inheritHeaders gives each density version without headers of its own
// the headers of its base, once every option has run, so the order of
// WithHeaders and WithDensities does not matter for them.
func (p *Plugin) inheritHeaders() {
	for name, d := range p.densityOf {
		if _, ok := p.headers[name]; ok {
			continue
		}
		if h, ok := p.headers[d.base]; ok {
			p.headers[name] = h
		}
	}
}

// capDensity returns t, a density version's transform, made at no more
// than the factor the source's pixels support (see WithDensities). It
// probes the source's header (imaging.Processor.Probe), never its pixels,
// and changes only t's size, so a negotiated format or a focal point
// already in t is kept.
func (p *Plugin) capDensity(ctx context.Context, disk *filesystem.Disk, key string, t imaging.Transform, d density) (imaging.Transform, error) {
	rc, err := disk.Read(ctx, key)
	if err != nil {
		return t, err
	}
	info, err := p.processor.Probe(rc)
	_ = rc.Close()
	if err != nil {
		return t, err
	}
	src := image.Rect(0, 0, info.Width, info.Height)
	if t.Crop != (image.Rectangle{}) {
		// A crop outside the image is refused by Process; leave it to say so.
		if src = t.Crop.Intersect(src); src.Empty() {
			return t, nil
		}
	}
	base := p.transforms[d.base]
	most := supported(base, src.Dx(), src.Dy())
	if most >= d.factor {
		return t, nil
	}
	c := scaled(base, max(1, most))
	t.Width, t.Height = c.Width, c.Height
	return t, nil
}

// supported is the largest factor of base that a sw × sh source fills
// without enlarging: ModeFill scales by the larger of the two ratios, so
// it is limited by the smaller one; ModeFit and ModePad by the larger,
// past which their image stops growing.
func supported(base imaging.Transform, sw, sh int) float64 {
	rw, rh := float64(sw)/float64(base.Width), float64(sh)/float64(base.Height)
	if base.Mode == imaging.ModeFill {
		return min(rw, rh)
	}
	return max(rw, rh)
}

// DensitySrcSet returns the HTML srcset of base and its density versions
// (WithDensities) with x descriptors, such as "https://…/avatar.jpg 1x,
// https://…/avatar-2x.jpg 2x, https://…/avatar-3x.jpg 3x", and the
// candidates it was built from; use it with an img whose CSS size is
// base's. Each version is generated if needed, as Get does.
//
// A descriptor is the version's real density, its width over base's,
// rounded to two decimals, so a version capped by a small source is
// advertised as what it is (2.25x, not 3x); a version that comes out no
// denser than the one before it is left out, so the browser never
// downloads more bytes for the same pixels. Candidates are densest last,
// each with its Density set. An undeclared base is CodeNotFound, one
// without densities CodeInvalidArgument; any version's failure fails the
// call. A view-once or expired file has none (CodeNotFound).
func (p *Plugin) DensitySrcSet(ctx context.Context, s *records.Store, scope, fileID, base string, ttl time.Duration) (string, []Candidate, error) {
	if _, ok := p.transforms[base]; !ok {
		return "", nil, filesystem.Errorf(filesystem.CodeNotFound, "variants: no variant named %q", base)
	}
	list := p.densities[base]
	if list == nil {
		return "", nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "variants: %q has no densities (WithDensities)", base)
	}
	f, err := s.File(ctx, scope, fileID)
	if err != nil {
		return "", nil, err
	}
	if err := linkable(s, f); err != nil {
		return "", nil, err
	}
	var out []Candidate
	var oneX int64
	for _, k := range list {
		name := DensityName(base, k)
		v, err := p.variant(ctx, s, f, name)
		if err != nil {
			return "", nil, err
		}
		if k == 1 {
			oneX = v.Width
		}
		got := math.Round(float64(v.Width)/float64(oneX)*descriptorPrecision) / descriptorPrecision
		if len(out) > 0 && got <= out[len(out)-1].Density {
			continue
		}
		u, err := variantURL(ctx, s, f, v, ttl)
		if err != nil {
			return "", nil, err
		}
		out = append(out, Candidate{Name: name, URL: u, Width: v.Width, Density: got})
	}
	parts := make([]string, len(out))
	for i, c := range out {
		parts[i] = c.URL + " " + strconv.FormatFloat(c.Density, 'f', -1, 64) + densitySuffix
	}
	return strings.Join(parts, ", "), out, nil
}

// Where a request states its device pixel ratio, read in this order by a
// Handler with HandlerOptions.Density.
const (
	// DPRParam is the query parameter (?dpr=2), as Cloudinary's dpr: an
	// app or a page can set it on the URL itself.
	DPRParam = "dpr"
	// HeaderSecCHDPR is the client hint Chromium browsers send once a page
	// asks for it with "Accept-CH: Sec-CH-DPR".
	HeaderSecCHDPR = "Sec-CH-DPR"
	// HeaderDPR is the hint's older name, still sent by some browsers.
	HeaderDPR = "DPR"
)

// requestDPR is r's device pixel ratio from DPRParam, HeaderSecCHDPR or
// HeaderDPR, the first that is present; 0 when none is, or the first
// present is not a finite positive number (the base is then served).
func requestDPR(r *http.Request) float64 {
	raw := r.URL.Query().Get(DPRParam)
	for _, h := range []string{HeaderSecCHDPR, HeaderDPR} {
		if raw == "" {
			raw = r.Header.Get(h)
		}
	}
	dpr, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsInf(dpr, 0) || !(dpr > 0) {
		return 0
	}
	return dpr
}

// densityFor is the declared name a request for name at dpr is served:
// for a base with densities, the version of the smallest factor at least
// dpr (rounding up, as Cloudinary does, so the screen is never short of
// pixels), or the densest when dpr exceeds them all; any other name, or
// a dpr of 1 or less, is name itself.
func (p *Plugin) densityFor(name string, dpr float64) string {
	list := p.densities[name]
	if list == nil || dpr <= 1 {
		return name
	}
	i := slices.IndexFunc(list, func(k float64) bool { return k >= dpr })
	if i < 0 {
		i = len(list) - 1
	}
	return DensityName(name, list[i])
}
