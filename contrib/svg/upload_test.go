package svg_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/svg"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

const (
	dirty = `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(2)</script><rect width="4" height="4"/></svg>`
	clean = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><rect width="4" height="4"></rect></svg>`
	html  = `<!doctype html><html><body><script>alert(1)</script></body></html>`
)

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// base is an application's image policy that also wants SVG.
var base = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*", "text/html"}}

func newPolicy(t *testing.T) upload.Policy {
	t.Helper()
	p, err := svg.Policy(base, svg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPolicy_StoresSVGClean(t *testing.T) {
	d := newDisk(t)
	res, err := upload.Receive(t.Context(), d, strings.NewReader(dirty), "logo.svg", newPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadAll(t.Context(), res.Key)
	if err != nil || string(got) != clean || res.ContentType != svg.ContentType {
		t.Fatalf("stored %q (%s), %v", got, res.ContentType, err)
	}
}

func TestPolicy_RefusesOtherActiveContentAndNames(t *testing.T) {
	d := newDisk(t)
	p := newPolicy(t)
	// The base allowed text/html by mistake; svg.Policy must not let
	// lifting the active-content default open that door.
	if _, err := upload.Receive(t.Context(), d, strings.NewReader(html), "page.html", p); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("html: %v", err)
	}
	if _, err := upload.Receive(t.Context(), d, strings.NewReader(clean), "shell.php.svg", p); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("executable name: %v", err)
	}
	if _, err := svg.Policy(upload.Policy{MaxSize: 1, Allowed: []string{"image/png"}}, svg.Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a policy without SVG: %v", err)
	}
}

// TestPolicy_LetsOnlySVGThrough: a base that allows a whole family must not
// let any other active type in with SVG, including the +xml types no fixed
// list names (RSS, Atom, XHTML: script runs in all of them).
func TestPolicy_LetsOnlySVGThrough(t *testing.T) {
	p, err := svg.Policy(upload.Policy{MaxSize: 1, Allowed: []string{"image/*", "application/*"}}, svg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Accepts(svg.ContentType); err != nil {
		t.Errorf("svg refused: %v", err)
	}
	for _, typ := range []string{"application/rss+xml", "application/atom+xml", "application/xhtml+xml", "application/javascript"} {
		if err := p.Accepts(typ); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("Accepts(%q) = %v, want rejected", typ, err)
		}
	}
	if p.AllowActiveContent {
		t.Error("svg.Policy lifted every active-content rule")
	}
}

// TestPolicy_ChecksUploadsThatSkipProcess covers presigned and Uppy
// uploads: the object is already in the bucket, and Confirm runs Inspect.
func TestPolicy_ChecksUploadsThatSkipProcess(t *testing.T) {
	d := newDisk(t)
	p := newPolicy(t)
	for name, tc := range map[string]struct {
		key, filename, body string
		ok                  bool
	}{
		"dirty":           {"in/a.svg", "a.svg", dirty, false},
		"clean":           {"in/b.svg", "b.svg", clean, true},
		"executable name": {"in/c.svg", "c.php.svg", clean, false},
	} {
		if _, err := d.WriteBytes(t.Context(), tc.key, []byte(tc.body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := upload.Confirm(t.Context(), d, tc.key, tc.filename, p)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", name, err)
		}
		exists, _ := d.Exists(t.Context(), tc.key)
		if exists != tc.ok {
			t.Errorf("%s: object exists = %v", name, exists)
		}
	}
}

func TestPolicy_RunsTheBaseHooks(t *testing.T) {
	var processed, inspected bool
	b := base
	b.Process = func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		processed = true
		return body, in, nil
	}
	b.Inspect = func(context.Context, *filesystem.Disk, upload.Result) error { inspected = true; return nil }
	p, err := svg.Policy(b, svg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Receive(t.Context(), newDisk(t), strings.NewReader(dirty), "logo.svg", p); err != nil || !processed || !inspected {
		t.Fatalf("err %v, processed %v, inspected %v", err, processed, inspected)
	}
}

func TestProcessorAndInspect_PassOtherTypes(t *testing.T) {
	d := newDisk(t)
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Process: svg.Processor(svg.Options{}), Inspect: svg.Inspect(svg.Options{})}
	res, err := upload.Receive(t.Context(), d, strings.NewReader("quarterly numbers\n"), "notes.txt", p)
	if err != nil || res.Size != 18 {
		t.Fatalf("%+v, %v", res, err)
	}
	broken := upload.Policy{MaxSize: 1 << 20, Allowed: []string{svg.ContentType}, AllowActiveContent: true, Process: svg.Processor(svg.Options{})}
	if _, err := upload.Receive(t.Context(), d, strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"><rect>`), "x.svg", broken); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a broken SVG: %v", err)
	}
}

func TestInspect_Failures(t *testing.T) {
	d := newDisk(t)
	inspect := svg.Inspect(svg.Options{})
	if err := inspect(t.Context(), d, upload.Result{Key: "missing.svg", ContentType: svg.ContentType}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing object: %v", err)
	}
	if _, err := d.WriteBytes(t.Context(), "bad.svg", []byte(`<svg><rect>`), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := inspect(t.Context(), d, upload.Result{Key: "bad.svg", ContentType: svg.ContentType}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("malformed object: %v", err)
	}
}

func TestPolicy_BaseProcessErrorRefuses(t *testing.T) {
	errBase := filesystem.Errorf(filesystem.CodeRejected, "the app's own check")
	b := base
	b.Process = func(context.Context, upload.Info, io.Reader) (io.Reader, upload.Info, error) {
		return nil, upload.Info{}, errBase
	}
	p, err := svg.Policy(b, svg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d := newDisk(t)
	if _, err := upload.Receive(t.Context(), d, strings.NewReader(clean), "logo.svg", p); err == nil || !strings.Contains(err.Error(), "the app's own check") {
		t.Fatalf("err = %v", err)
	}
}
