package variants_test

import (
	"image"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
)

// avatar is a 40-pixel square base variant, declared once.
var avatar = map[string]imaging.Transform{
	"avatar": {Mode: imaging.ModeFill, Width: 40, Height: 40, Format: imaging.FormatJPEG},
}

// densityEnv is a store whose plugin declares tr with options.
func densityEnv(t *testing.T, tr map[string]imaging.Transform, options ...variants.Option) *env {
	t.Helper()
	p, err := variants.New(tr, imaging.Options{}, options...)
	if err != nil {
		t.Fatal(err)
	}
	return newEnvWithPlugin(t, memory.New(), p, linked...)
}

// size gets the named variant and returns its dimensions.
func (e *env) size(t *testing.T, f records.File, name string) (int64, int64) {
	t.Helper()
	v, err := e.plugin.Get(t.Context(), e.store, "t", f.ID, name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v.Width, v.Height
}

func TestDensityName(t *testing.T) {
	for k, want := range map[float64]string{1: "avatar", 2: "avatar-2x", 3: "avatar-3x", 1.5: "avatar-1_5x", 2.625: "avatar-2_625x"} {
		if got := variants.DensityName("avatar", k); got != want {
			t.Errorf("DensityName(%v) = %q, want %q", k, got, want)
		}
	}
}

// TestDensitiesAreDeclaredVariants pins that WithDensities declares 2x and
// 3x by default at the scaled size, generated and stored like any variant,
// that a configured list (Android's 1.5x) gets its own names, and that the
// caller's map is left as it was.
func TestDensitiesAreDeclaredVariants(t *testing.T) {
	e := densityEnv(t, avatar, variants.WithDensities("avatar"))
	if len(avatar) != 1 {
		t.Fatalf("New changed the caller's map: %v", avatar)
	}
	f := e.image(t, "a.png", pngBytes(t, 300, 300), filesystem.VisibilityPrivate)
	for name, want := range map[string]int64{"avatar": 40, "avatar-2x": 80, "avatar-3x": 120} {
		if w, h := e.size(t, f, name); w != want || h != want {
			t.Errorf("%s = %dx%d, want %d", name, w, h, want)
		}
	}
	v, _ := e.plugin.Get(t.Context(), e.store, "t", f.ID, "avatar-2x")
	if !strings.HasPrefix(v.Key, variants.KeyDir+"/"+f.ID+"/avatar-2x-") {
		t.Errorf("key = %q", v.Key)
	}
	if _, err := e.plugin.Get(t.Context(), e.store, "t", f.ID, "avatar-4x"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("undeclared density = %v", err)
	}
	if n, err := e.plugin.Prune(t.Context(), e.store); err != nil || n != 0 {
		t.Errorf("Prune removed %d density versions (%v)", n, err)
	}

	android := densityEnv(t, avatar, variants.WithDensities("avatar", 1, 1.5, 2, 3, 4))
	g := android.image(t, "a.png", pngBytes(t, 300, 300), filesystem.VisibilityPrivate)
	for name, want := range map[string]int64{"avatar-1_5x": 60, "avatar-4x": 160} {
		if w, _ := android.size(t, g, name); w != want {
			t.Errorf("%s = %d, want %d", name, w, want)
		}
	}
	// Generate with no names makes every declared variant, densities too.
	if err := android.plugin.Generate(t.Context(), android.store, "t", g.ID); err != nil {
		t.Fatal(err)
	}
}

// TestDensityNeverUpscales pins the cap: a version is made at the largest
// factor its source fills, never below 1x, for each mode and for a crop.
func TestDensityNeverUpscales(t *testing.T) {
	cases := []struct {
		name   string
		base   imaging.Transform
		src    [2]int
		want2x [2]int64
		want3x [2]int64
	}{
		// 100 pixels fill 2x (80) but only 2.5x: the "3x" is 100.
		{"fill short of 3x", avatar["avatar"], [2]int{100, 100}, [2]int64{80, 80}, [2]int64{100, 100}},
		// A wide source: fill is limited by its height, 60/40 = 1.5x.
		{"fill limited by height", avatar["avatar"], [2]int{400, 60}, [2]int64{60, 60}, [2]int64{60, 60}},
		// Smaller than the 1x box: both versions are the 1x size.
		{"fill below 1x", avatar["avatar"], [2]int{30, 30}, [2]int64{40, 40}, [2]int64{40, 40}},
		// Fit stops growing at the source's own size.
		{"fit", imaging.Transform{Mode: imaging.ModeFit, Width: 40, Height: 40, Format: imaging.FormatPNG}, [2]int{100, 50}, [2]int64{80, 40}, [2]int64{100, 50}},
		// Pad would otherwise centre the source on a larger canvas.
		{"pad", imaging.Transform{Mode: imaging.ModePad, Width: 40, Height: 40, Format: imaging.FormatPNG}, [2]int{60, 60}, [2]int64{60, 60}, [2]int64{60, 60}},
		// The crop, not the whole image, is the source: 50/40 = 1.25x.
		{"crop", imaging.Transform{Mode: imaging.ModeFill, Width: 40, Height: 40, Format: imaging.FormatJPEG, Crop: image.Rect(0, 0, 50, 50)}, [2]int{300, 300}, [2]int64{50, 50}, [2]int64{50, 50}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := densityEnv(t, map[string]imaging.Transform{"avatar": c.base}, variants.WithDensities("avatar"))
			f := e.image(t, "a.png", pngBytes(t, c.src[0], c.src[1]), filesystem.VisibilityPrivate)
			if w, h := e.size(t, f, "avatar-2x"); w != c.want2x[0] || h != c.want2x[1] {
				t.Errorf("2x = %dx%d, want %v", w, h, c.want2x)
			}
			if w, h := e.size(t, f, "avatar-3x"); w != c.want3x[0] || h != c.want3x[1] {
				t.Errorf("3x = %dx%d, want %v", w, h, c.want3x)
			}
		})
	}
}

// TestDensityCapFailures pins that a source the cap cannot read fails the
// version (rather than generating it uncapped), and that a crop outside
// the image is left for Process to refuse.
func TestDensityCapFailures(t *testing.T) {
	ctx := t.Context()
	e := densityEnv(t, avatar, variants.WithDensities("avatar"))
	// PNG magic with a broken header: recorded as an image, unreadable.
	bad := e.image(t, "bad.png", []byte("\x89PNG\r\n\x1a\nnot a header at all"), filesystem.VisibilityPrivate)
	_, err := e.plugin.Get(ctx, e.store, "t", bad.ID, "avatar-2x")
	wantCode(t, err, filesystem.CodeRejected)
	gone := e.image(t, "gone.png", pngBytes(t, 100, 100), filesystem.VisibilityPrivate)
	if err := e.disk.Delete(ctx, "gone.png"); err != nil {
		t.Fatal(err)
	}
	_, err = e.plugin.Get(ctx, e.store, "t", gone.ID, "avatar-2x")
	wantCode(t, err, filesystem.CodeNotFound)

	outside := map[string]imaging.Transform{"avatar": {Mode: imaging.ModeFill, Width: 40, Height: 40, Format: imaging.FormatJPEG, Crop: image.Rect(500, 500, 600, 600)}}
	o := densityEnv(t, outside, variants.WithDensities("avatar"))
	f := o.image(t, "a.png", pngBytes(t, 100, 100), filesystem.VisibilityPrivate)
	if _, err := o.plugin.Get(ctx, o.store, "t", f.ID, "avatar-2x"); err == nil {
		t.Error("a crop outside the image made a variant")
	}
}

func TestWithDensitiesRefusals(t *testing.T) {
	long := strings.Repeat("a", 62)
	cases := map[string]struct {
		tr   map[string]imaging.Transform
		opts []variants.Option
	}{
		"undeclared base": {avatar, []variants.Option{variants.WithDensities("missing")}},
		"declared twice":  {avatar, []variants.Option{variants.WithDensities("avatar"), variants.WithDensities("avatar", 4)}},
		"of a density":    {avatar, []variants.Option{variants.WithDensities("avatar"), variants.WithDensities("avatar-2x")}},
		"below 1":         {avatar, []variants.Option{variants.WithDensities("avatar", 0.5)}},
		"above max":       {avatar, []variants.Option{variants.WithDensities("avatar", variants.MaxDensity+1)}},
		"not a number":    {avatar, []variants.Option{variants.WithDensities("avatar", math.NaN())}},
		"listed twice":    {avatar, []variants.Option{variants.WithDensities("avatar", 2, 2)}},
		"name taken": {map[string]imaging.Transform{"avatar": avatar["avatar"], "avatar-2x": avatar["avatar"]},
			[]variants.Option{variants.WithDensities("avatar")}},
		"name too long": {map[string]imaging.Transform{long: avatar["avatar"]}, []variants.Option{variants.WithDensities(long)}},
		"size too large": {map[string]imaging.Transform{"big": {Mode: imaging.ModeFit, Width: 4000, Height: 4000, Format: imaging.FormatJPEG}},
			[]variants.Option{variants.WithDensities("big")}},
		"eager before densities": {avatar, []variants.Option{variants.WithEager(nil, "avatar-2x"), variants.WithDensities("avatar")}},
	}
	for name, c := range cases {
		if _, err := variants.New(c.tr, imaging.Options{}, c.opts...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s = %v, want CodeInvalidArgument", name, err)
		}
	}
	// 1 is base itself, so listing it is allowed; eager after densities
	// sees the versions.
	if _, err := variants.New(avatar, imaging.Options{}, variants.WithDensities("avatar", 1, 3), variants.WithEager(nil, "avatar-3x")); err != nil {
		t.Errorf("1 listed, eager after = %v", err)
	}
}

// TestDensityHeaders pins that a version without headers of its own takes
// its base's whichever option comes first, and that its own win.
func TestDensityHeaders(t *testing.T) {
	ctx := t.Context()
	own := filesystem.HTTPHeaders{CacheControl: "no-cache"}
	e := densityEnv(t, avatar, variants.WithHeaders(immutable, "avatar"), variants.WithDensities("avatar"), variants.WithHeaders(own, "avatar-3x"))
	f := e.image(t, "a.png", pngBytes(t, 300, 300), filesystem.VisibilityPrivate)
	for name, want := range map[string]filesystem.HTTPHeaders{"avatar": immutable, "avatar-2x": immutable, "avatar-3x": own} {
		v, err := e.plugin.Get(ctx, e.store, "t", f.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if st, _ := e.disk.Stat(ctx, v.Key); st.Headers != want {
			t.Errorf("%s headers = %+v, want %+v", name, st.Headers, want)
		}
	}
	plain := densityEnv(t, avatar, variants.WithDensities("avatar"))
	g := plain.image(t, "a.png", pngBytes(t, 300, 300), filesystem.VisibilityPrivate)
	v, _ := plain.plugin.Get(ctx, plain.store, "t", g.ID, "avatar-2x")
	if st, _ := plain.disk.Stat(ctx, v.Key); !st.Headers.IsZero() {
		t.Errorf("headers from nowhere: %+v", st.Headers)
	}
}

// TestDensitySrcSet pins the x descriptors: the real density of each
// version, a capped one advertised as what it is, one adding no pixels
// left out.
func TestDensitySrcSet(t *testing.T) {
	ctx := t.Context()
	e := densityEnv(t, avatar, variants.WithDensities("avatar"))
	cases := []struct {
		src   int
		names []string
		descr []string
	}{
		{300, []string{"avatar", "avatar-2x", "avatar-3x"}, []string{"1x", "2x", "3x"}},
		{100, []string{"avatar", "avatar-2x", "avatar-3x"}, []string{"1x", "2x", "2.5x"}},
		{70, []string{"avatar", "avatar-2x"}, []string{"1x", "1.75x"}},
		{30, []string{"avatar"}, []string{"1x"}},
	}
	for _, c := range cases {
		f := e.image(t, "a.png", pngBytes(t, c.src, c.src), filesystem.VisibilityPublic)
		set, list, err := e.plugin.DensitySrcSet(ctx, e.store, "t", f.ID, "avatar", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(set, ", ")
		if len(list) != len(c.names) || len(parts) != len(c.names) {
			t.Fatalf("source %d: srcset %q, candidates %+v", c.src, set, list)
		}
		for i, cand := range list {
			if cand.Name != c.names[i] || parts[i] != cand.URL+" "+c.descr[i] || !strings.HasPrefix(cand.URL, "https://cdn.test/variants/") {
				t.Errorf("source %d candidate %d = %+v / %q", c.src, i, cand, parts[i])
			}
		}
	}
	// A third of a pixel short of 3x rounds to two decimals.
	odd := densityEnv(t, map[string]imaging.Transform{"w": {Mode: imaging.ModeFit, Width: 30, Height: 30, Format: imaging.FormatPNG}}, variants.WithDensities("w"))
	f := odd.image(t, "a.png", pngBytes(t, 50, 50), filesystem.VisibilityPublic)
	set, _, err := odd.plugin.DensitySrcSet(ctx, odd.store, "t", f.ID, "w", time.Hour)
	if err != nil || !strings.HasSuffix(set, " 1.67x") {
		t.Errorf("srcset = %q, %v", set, err)
	}
}

func TestDensitySrcSetRefusals(t *testing.T) {
	ctx := t.Context()
	e := densityEnv(t, map[string]imaging.Transform{"avatar": avatar["avatar"], "thumb": transforms["thumb"]}, variants.WithDensities("avatar"))
	f := e.image(t, "a.png", pngBytes(t, 100, 100), filesystem.VisibilityPrivate)
	_, _, err := e.plugin.DensitySrcSet(ctx, e.store, "t", f.ID, "huge", time.Hour)
	wantCode(t, err, filesystem.CodeNotFound)
	_, _, err = e.plugin.DensitySrcSet(ctx, e.store, "t", f.ID, "thumb", time.Hour)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, _, err = e.plugin.DensitySrcSet(ctx, e.store, "t", "fil_missing", "avatar", time.Hour)
	wantCode(t, err, filesystem.CodeNotFound)
	if _, err := e.disk.WriteBytes(ctx, "doc.txt", []byte("text"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "doc.txt"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.plugin.DensitySrcSet(ctx, e.store, "t", doc.ID, "avatar", time.Hour)
	wantCode(t, err, filesystem.CodeRejected)
	past := time.Now().Add(-time.Minute)
	if _, err := e.disk.WriteBytes(ctx, "old.png", pngBytes(t, 100, 100), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	old, err := e.store.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "old.png", ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.plugin.DensitySrcSet(ctx, e.store, "t", old.ID, "avatar", time.Hour)
	wantCode(t, err, filesystem.CodeNotFound)
	// A disk that can neither sign nor serve publicly fails the call.
	p, _ := variants.New(avatar, imaging.Options{}, variants.WithDensities("avatar"))
	bare := newEnvWithPlugin(t, memory.New(), p)
	g := bare.image(t, "a.png", pngBytes(t, 100, 100), filesystem.VisibilityPrivate)
	_, _, err = p.DensitySrcSet(ctx, bare.store, "t", g.ID, "avatar", time.Hour)
	wantCode(t, err, filesystem.CodeUnsupported)
}

// TestHandlerDensity pins the dpr selection: the query parameter first,
// then Sec-CH-DPR, then DPR, rounded up to a declared factor; anything
// unusable serves the base; without the option, a dpr changes nothing.
func TestHandlerDensity(t *testing.T) {
	e := densityEnv(t, map[string]imaging.Transform{"avatar": avatar["avatar"], "thumb": transforms["thumb"]}, variants.WithDensities("avatar"))
	f := e.image(t, "a.png", pngBytes(t, 300, 300), filesystem.VisibilityPrivate)
	scopeOf := func(*http.Request) (string, error) { return "t", nil }
	on := httptest.NewServer(variants.Handler(e.store, e.plugin, scopeOf, variants.HandlerOptions{Density: true}))
	defer on.Close()
	off := httptest.NewServer(variants.Handler(e.store, e.plugin, scopeOf, variants.HandlerOptions{}))
	defer off.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(base, path string, headers map[string]string) *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/"+f.ID+"/"+path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	link := map[string]string{}
	for _, name := range []string{"avatar", "avatar-2x", "avatar-3x", "thumb"} {
		v, err := e.plugin.Get(t.Context(), e.store, "t", f.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		link[name] = "https://app.test/media/" + v.Key
	}
	cases := []struct {
		path    string
		headers map[string]string
		want    string
	}{
		{"avatar", nil, "avatar"},
		{"avatar?dpr=2", nil, "avatar-2x"},
		{"avatar?dpr=2.6", nil, "avatar-3x"},
		{"avatar?dpr=10", nil, "avatar-3x"},
		{"avatar?dpr=1", nil, "avatar"},
		{"avatar?dpr=1.2", nil, "avatar-2x"},
		{"avatar?dpr=abc", nil, "avatar"},
		{"avatar?dpr=-2", nil, "avatar"},
		{"avatar?dpr=Inf", nil, "avatar"},
		{"avatar?dpr=2", map[string]string{variants.HeaderSecCHDPR: "3"}, "avatar-2x"},
		{"avatar", map[string]string{variants.HeaderSecCHDPR: "3", variants.HeaderDPR: "2"}, "avatar-3x"},
		{"avatar", map[string]string{variants.HeaderDPR: " 2 "}, "avatar-2x"},
		{"avatar-3x?dpr=1", nil, "avatar-3x"},
		{"thumb?dpr=3", nil, "thumb"},
	}
	for _, c := range cases {
		r := get(on.URL, c.path, c.headers)
		if loc := r.Header.Get("Location"); r.StatusCode != http.StatusFound || loc != link[c.want] {
			t.Errorf("%s %v = %d %s, want %s", c.path, c.headers, r.StatusCode, loc, c.want)
		}
		if got := r.Header.Get("Vary"); got != variants.HeaderSecCHDPR+", "+variants.HeaderDPR {
			t.Errorf("%s Vary = %q", c.path, got)
		}
	}
	r := get(off.URL, "avatar?dpr=3", nil)
	if loc := r.Header.Get("Location"); loc != link["avatar"] || r.Header.Get("Vary") != "" {
		t.Errorf("without Density = %s, Vary %q", loc, r.Header.Get("Vary"))
	}
}
