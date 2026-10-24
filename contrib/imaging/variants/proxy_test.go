package variants_test

import (
	"bytes"
	"errors"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// TestHandlerModes pins the handler's mode and cache seams: ModeProxy
// streams the variant with the safety headers and the chosen
// Cache-Control, reports the read first and serves nothing a refusing
// hook did not see; an unknown mode fails closed; and the default stays a
// 302 with DefaultCacheControl.
func TestHandlerModes(t *testing.T) {
	db, _ := recordstest.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	p, err := variants.New(transforms, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := &meter{}
	s, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p, m))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, disk: disk, store: s, plugin: p}
	f := e.image(t, "a.png", pngBytes(t, 600, 400), filesystem.VisibilityPrivate)
	scope := func(*http.Request) (string, error) { return "t", nil }
	serveWith := func(opts variants.HandlerOptions, method string, header http.Header) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), method, "/"+f.ID+"/thumb", nil)
		for k, v := range header {
			req.Header[k] = v
		}
		variants.Handler(s, p, scope, opts).ServeHTTP(rec, req)
		return rec
	}
	serve := func(opts variants.HandlerOptions, method string) *httptest.ResponseRecorder {
		return serveWith(opts, method, nil)
	}

	proxy := variants.HandlerOptions{Mode: variants.ModeProxy, CacheControl: "public, max-age=86400"}
	rec := serve(proxy, http.MethodGet)
	h := rec.Header()
	if rec.Code != http.StatusOK || h.Get("Location") != "" || h.Get("Content-Type") != "image/jpeg" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Content-Security-Policy") == "" || h.Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("proxy = %d %v", rec.Code, h)
	}
	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || img.Bounds().Dx() != 32 || h.Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("proxied body: %v, length %s of %d", err, h.Get("Content-Length"), rec.Body.Len())
	}
	if len(m.served) != 1 || m.served[0].Kind != records.ServeVariant || m.served[0].Bytes != int64(rec.Body.Len()) {
		t.Fatalf("served = %+v", m.served)
	}
	if head := serve(proxy, http.MethodHead); head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d with %d bytes", head.Code, head.Body.Len())
	}

	// Proxy mode answers ranges and conditional requests through fshttp's
	// serve path (I49), keeping the safety headers and the Cache-Control.
	full := rec.Body.Bytes()
	part := serveWith(proxy, http.MethodGet, http.Header{"Range": {"bytes=0-9"}})
	if part.Code != http.StatusPartialContent || !bytes.Equal(part.Body.Bytes(), full[:10]) || part.Header().Get("Content-Range") != "bytes 0-9/"+strconv.Itoa(len(full)) ||
		part.Header().Get("X-Content-Type-Options") != "nosniff" || part.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Fatalf("range = %d %v (%d bytes)", part.Code, part.Header(), part.Body.Len())
	}
	if bad := serveWith(proxy, http.MethodGet, http.Header{"Range": {"bytes=999999-"}}); bad.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range = %d", bad.Code)
	}
	etag := h.Get("ETag")
	if etag == "" {
		t.Fatal("a proxied variant has no ETag")
	}
	if nm := serveWith(proxy, http.MethodGet, http.Header{"If-None-Match": {etag}}); nm.Code != http.StatusNotModified || nm.Body.Len() != 0 {
		t.Fatalf("If-None-Match = %d with %d bytes", nm.Code, nm.Body.Len())
	}

	m.err = errors.New("meter down")
	if rec := serve(proxy, http.MethodGet); rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") == "image/jpeg" {
		t.Fatalf("a refused read answered %d %v", rec.Code, rec.Header())
	}
	m.err = nil

	if rec := serve(variants.HandlerOptions{Mode: "stream"}, http.MethodGet); rec.Code != http.StatusInternalServerError {
		t.Fatalf("an unknown mode answered %d", rec.Code)
	}

	def := serve(variants.HandlerOptions{}, http.MethodGet)
	if def.Code != http.StatusFound || def.Header().Get("Cache-Control") != variants.DefaultCacheControl || variants.DefaultCacheControl != "private, max-age=60" {
		t.Fatalf("default = %d %q", def.Code, def.Header().Get("Cache-Control"))
	}
	if custom := serve(variants.HandlerOptions{CacheControl: "no-store"}, http.MethodGet); custom.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("redirect cache control %q", custom.Header().Get("Cache-Control"))
	}

	// An undeclared variant is 404 in proxy mode too.
	rec = httptest.NewRecorder()
	variants.Handler(s, p, scope, proxy).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/"+f.ID+"/poster", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("undeclared variant = %d", rec.Code)
	}
	// A variant whose object went missing behind the row: the read's
	// error, not an empty 200.
	for e, err := range disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Key != f.Key {
			if err := disk.Delete(t.Context(), e.Key); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rec := serve(proxy, http.MethodGet); rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") == "image/jpeg" {
		t.Fatalf("a missing variant object answered %d %v", rec.Code, rec.Header())
	}
}
