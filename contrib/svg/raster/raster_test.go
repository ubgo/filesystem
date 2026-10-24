package raster_test

import (
	"bytes"
	"errors"
	"flag"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/imagingtest"
	"github.com/ubgo/filesystem/contrib/svg"
	"github.com/ubgo/filesystem/contrib/svg/raster"
)

var _ = flag.Bool("update", false, "rewrite golden images")

// badge is a 200 × 100 drawing: a blue rounded rectangle, a red circle and
// a gradient bar, the shapes a logo or an icon is made of.
const badge = `<?xml version="1.0" encoding="UTF-8"?>
<!-- a test badge -->
<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="0 0 200 100">
  <defs><linearGradient id="g"><stop offset="0" stop-color="#ff0"/><stop offset="1" stop-color="#0a0"/></linearGradient></defs>
  <rect x="5" y="5" width="190" height="90" rx="15" fill="#2050c0"/>
  <circle cx="60" cy="50" r="30" fill="#e02020"/>
  <rect x="110" y="35" width="70" height="30" fill="url(#g)"/>
</svg>`

func pipeline(o raster.Options) *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithDecoder(raster.Decoder(o)))
}

// TestRenders pins IM-24 with a perceptual golden: the badge drawn at a
// 400-pixel side through an ordinary Pipeline, as a variant would be.
func TestRenders(t *testing.T) {
	p := pipeline(raster.Options{Side: 400})
	info, err := p.Probe(strings.NewReader(badge))
	if err != nil || info != (imaging.Info{Format: "svg", Width: 400, Height: 200}) {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	res, err := p.Process(strings.NewReader(badge), imaging.Transform{Mode: imaging.ModeFit, Width: 400, Height: 400, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(res.Data))
	imagingtest.Golden(t, img, "testdata/badge.golden.png")
}

// TestSanitisesFirst pins that the renderer only ever sees a sanitised
// document: a script and an external reference are gone before it runs.
func TestSanitisesFirst(t *testing.T) {
	var seen string
	spy := func(doc io.Reader, w, h int) (*image.RGBA, error) {
		b, _ := io.ReadAll(doc)
		seen = string(b)
		return image.NewRGBA(image.Rect(0, 0, w, h)), nil
	}
	evil := `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="10" height="10"><script>alert(1)</script><image xlink:href="https://evil.example/track.png" width="10" height="10"/><rect width="10" height="10" onload="steal()"/></svg>`
	if _, err := raster.Decoder(raster.Options{Render: spy}).Decode(strings.NewReader(evil)); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"script", "evil.example", "onload"} {
		if strings.Contains(seen, gone) {
			t.Errorf("the renderer saw %q:\n%s", gone, seen)
		}
	}
	// The sanitiser's limits refuse before rendering.
	big := `<svg xmlns="http://www.w3.org/2000/svg">` + strings.Repeat("<g/>", 50) + `</svg>`
	if _, err := raster.Decoder(raster.Options{Render: spy, Sanitize: svg.Options{MaxElements: 10}}).Decode(strings.NewReader(big)); err == nil {
		t.Error("an SVG over the element limit was rendered")
	}
}

func TestSizes(t *testing.T) {
	d := raster.Decoder(raster.Options{Side: 100})
	for doc, want := range map[string][2]int{
		`<svg width="300" height="150"/>`:                           {100, 50},
		`<svg width="40px" height="80px"/>`:                         {50, 100},
		`<svg viewBox="0 0 10 20"/>`:                                {50, 100},
		`<svg viewBox="0,0,30,10" width="50%"/>`:                    {100, 33},
		`<svg width="2em" height="1em"/>`:                           {100, 50}, // browser default 300 × 150
		"\xef\xbb\xbf  <!DOCTYPE svg>\n<svg width='1' height='1'/>": {100, 100},
	} {
		cfg, err := d.DecodeConfig(strings.NewReader(doc))
		if err != nil || [2]int{cfg.Width, cfg.Height} != want {
			t.Errorf("%s = %dx%d, %v; want %v", doc, cfg.Width, cfg.Height, err, want)
		}
	}
	// Side defaults and caps.
	if cfg, _ := raster.Decoder(raster.Options{}).DecodeConfig(strings.NewReader(badge)); cfg.Width != raster.DefaultSide {
		t.Errorf("default side = %d", cfg.Width)
	}
	if cfg, _ := raster.Decoder(raster.Options{Side: 1 << 20}).DecodeConfig(strings.NewReader(badge)); cfg.Width != raster.MaxSide {
		t.Errorf("capped side = %d", cfg.Width)
	}
	for _, notSVG := range []string{`<html><svg/></html>`, `hello <svg/>`, "\x89PNG", ``} {
		if d.Match([]byte(notSVG)) {
			t.Errorf("Match(%q)", notSVG)
		}
		if _, err := d.DecodeConfig(strings.NewReader(notSVG)); err == nil {
			t.Errorf("DecodeConfig(%q) accepted", notSVG)
		}
	}
	if !d.Match([]byte(badge)) || d.Name() != "svg" {
		t.Error("Match or Name")
	}
	if _, err := d.DecodeConfig(failing{}); err == nil {
		t.Error("a read error was ignored")
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestTimeout pins the time cap: a renderer that runs too long fails the
// decode with CodeRejected instead of holding the request.
func TestTimeout(t *testing.T) {
	slow := func(io.Reader, int, int) (*image.RGBA, error) {
		time.Sleep(time.Second)
		return nil, nil
	}
	_, err := raster.Decoder(raster.Options{Render: slow, Timeout: 20 * time.Millisecond}).Decode(strings.NewReader(badge))
	if filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("slow render = %v", err)
	}
}

// TestRendersInFlightAreBounded pins that renderings abandoned by the
// timeout still count until they end: with one slot, a second Decode is
// refused while the first, timed out, still runs, and accepted after.
func TestRendersInFlightAreBounded(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	blocked := func(_ io.Reader, w, h int) (*image.RGBA, error) {
		started <- struct{}{}
		<-release
		return image.NewRGBA(image.Rect(0, 0, w, h)), nil
	}
	d := raster.Decoder(raster.Options{Render: blocked, Timeout: 10 * time.Millisecond, MaxRenders: 1})
	if _, err := d.Decode(strings.NewReader(badge)); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("first = %v, want the timeout", err)
	}
	<-started
	if _, err := d.Decode(strings.NewReader(badge)); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("second while the first still runs = %v, want unavailable", err)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := d.Decode(strings.NewReader(badge)); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the slot was never freed")
}

// TestRendererPanicIsContained pins that a panicking renderer fails the
// decode with CodeUnknown instead of crashing the process, and frees its
// slot for the next one.
func TestRendererPanicIsContained(t *testing.T) {
	calls := 0
	flaky := func(_ io.Reader, w, h int) (*image.RGBA, error) {
		calls++
		if calls == 1 {
			panic("renderer bug")
		}
		return image.NewRGBA(image.Rect(0, 0, w, h)), nil
	}
	d := raster.Decoder(raster.Options{Render: flaky, MaxRenders: 1})
	if _, err := d.Decode(strings.NewReader(badge)); filesystem.CodeOf(err) != filesystem.CodeUnknown || !strings.Contains(err.Error(), "renderer bug") {
		t.Fatalf("panicking render = %v", err)
	}
	if _, err := d.Decode(strings.NewReader(badge)); err != nil {
		t.Errorf("the slot was not freed after the panic: %v", err)
	}
}
