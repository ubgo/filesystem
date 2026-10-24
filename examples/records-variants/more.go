package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/focal"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// Names of the second half's variants.
const (
	nameBanner = "banner"
	nameAvatar = "avatar"
)

// moreTransforms crops a wide banner around the file's focal point, and
// declares a 48-pixel avatar whose 2x and 3x versions WithDensities adds.
// ds:def id=moretransforms-c5w9k2nq
var moreTransforms = map[string]imaging.Transform{
	nameBanner: {Mode: imaging.ModeFill, Width: 120, Height: 40, Format: imaging.FormatJPEG, Gravity: imaging.GravityFocal},
	nameAvatar: {Mode: imaging.ModeFill, Width: 48, Height: 48, Format: imaging.FormatJPEG},
}

// variantEvents records variants.ready and variants.failed, as a plugin
// that notifies the client (a websocket push) would.
type variantEvents struct{ lines []string }

func (v *variantEvents) Name() string { return "variant_events" }

func (v *variantEvents) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	if e.Kind == variants.EventReady || e.Kind == variants.EventFailed {
		v.lines = append(v.lines, fmt.Sprintf("%s [%s]", e.Kind, e.Detail[variants.DetailVariants]))
	}
	return nil
}

// newMorePlugins is the focal plugin and a variants plugin that reads its
// points, with densities for the avatar. WithDensities comes before any
// option that names a density version. Nothing is eager: a job makes
// the variants (Generate).
// ds:def id=newmoreplugins-h8t3v6mc
func newMorePlugins() (*focal.Plugin, *variants.Plugin, error) {
	fp := focal.New()
	vp, err := variants.New(moreTransforms, imaging.Options{},
		variants.WithFocal(fp),
		variants.WithDensities(nameAvatar),
	)
	return fp, vp, err
}

// variantJob is the worker of a per-upload job (River, Asynq, the app's
// queue): it makes every declared variant of one file now, with the same
// processing status and events as eager generation. A retry finishes what
// a failed run left.
// ds:def id=variantjob-k2n7w4tr
func variantJob(ctx context.Context, s *records.Store, vp *variants.Plugin, scope, fileID string) error {
	return vp.Generate(ctx, s, scope, fileID)
}

// catchUp is the scheduled companion: it makes the variants missing from
// a scope's images, newest first, at most maxFiles per run, resumable
// through NextCursor.
// ds:def id=catchup-p4c8m3vh
func catchUp(ctx context.Context, s *records.Store, vp *variants.Plugin, maxFiles int) (variants.MissingResult, error) {
	return vp.GenerateMissing(ctx, s, variants.MissingOptions{Scope: tenant, MaxFiles: maxFiles})
}

// newNegotiatingHandler serves variants in a format the request's Accept
// names explicitly (here PNG instead of the declared JPEG; in production
// AVIF and WebP with their codec modules in the processor), and a density
// version by the request's device pixel ratio.
// ds:def id=newnegotiatinghandler-w6r2t9qk
func newNegotiatingHandler(s *records.Store, vp *variants.Plugin) http.Handler {
	return variants.Handler(s, vp, scopeOf, variants.HandlerOptions{
		LinkLifetime: linkTTL,
		Negotiate:    []imaging.Format{imaging.FormatPNG},
		Density:      true,
	})
}

// moreSteps runs focal points, jobs, catch-up, densities and negotiation
// on a database of its own.
func moreSteps(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "more.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	fp, vp, err := newMorePlugins()
	if err != nil {
		return err
	}
	events := &variantEvents{}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(fp, vp, events))
	if err != nil {
		return err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return err
	}
	photo, err := uploadPhoto(ctx, s, 400, 200)
	if err != nil {
		return err
	}
	if err := variantJob(ctx, s, vp, tenant, photo.ID); err != nil {
		return err
	}
	first, err := vp.Get(ctx, s, tenant, photo.ID, nameBanner)
	if err != nil {
		return err
	}
	if _, err := fp.Set(ctx, s, tenant, photo.ID, &imaging.Focal{X: 0.9, Y: 0.5}); err != nil {
		return err
	}
	moved, err := vp.Get(ctx, s, tenant, photo.ID, nameBanner)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "focal point set: banner remade %t\n", moved.Key != first.Key)
	if _, err := uploadPhoto(ctx, s, 300, 300); err != nil {
		return err
	}
	res, err := catchUp(ctx, s, vp, 10)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "catch-up: generated %d, complete %d, failed %d\n", res.Generated, res.Complete, len(res.Failed))
	fmt.Fprintf(w, "events: %s\n", strings.Join(events.lines, ", "))
	srcset, _, err := vp.DensitySrcSet(ctx, s, tenant, photo.ID, nameAvatar, linkTTL)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "density srcset descriptors: %s\n", descriptors(srcset))
	h := newNegotiatingHandler(s, vp)
	for _, c := range []struct{ path, accept string }{
		{photo.ID + "/" + nameBanner, "image/png,image/*;q=0.8"},
		{photo.ID + "/" + nameBanner, "image/*"},
		{photo.ID + "/" + nameAvatar + "?dpr=2.5", ""},
	} {
		req := httptest.NewRequest(http.MethodGet, "/"+c.path, nil)
		if c.accept != "" {
			req.Header.Set("Accept", c.accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		loc := rec.Header().Get("Location")
		fmt.Fprintf(w, "GET %s (Accept %q): %d %s, Vary %q\n", c.path[strings.IndexByte(c.path, '/')+1:], c.accept, rec.Code, variantFile(loc), strings.Join(rec.Header().Values("Vary"), ", "))
	}
	return nil
}

// descriptors keeps only the x descriptors of a density srcset.
func descriptors(srcset string) string {
	var out []string
	for _, c := range strings.Split(srcset, ", ") {
		out = append(out, c[strings.LastIndexByte(c, ' ')+1:])
	}
	return strings.Join(out, " ")
}

// variantFile is the variant's name and extension in a redirect location:
// "avatar-3x….jpg".
func variantFile(loc string) string {
	if i := strings.IndexByte(loc, '?'); i >= 0 {
		loc = loc[:i]
	}
	base := loc[strings.LastIndexByte(loc, '/')+1:]
	name := base[:strings.IndexByte(base, '-')]
	if strings.HasPrefix(base, nameAvatar+"-") && len(base) > len(nameAvatar)+3 && base[len(nameAvatar)+2] == 'x' {
		name = base[:len(nameAvatar)+3]
	}
	return name + "…" + filepath.Ext(base)
}
