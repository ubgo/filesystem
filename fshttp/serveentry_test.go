package fshttp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// serveEntry stats key on d and serves it through ServeEntry with the
// request's headers.
func serveEntry(t *testing.T, d *filesystem.Disk, key string, header http.Header, opts fshttp.ServeOptions) *httptest.ResponseRecorder {
	t.Helper()
	e, err := d.Stat(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	fshttp.ServeEntry(rec, req, d, e, opts)
	return rec
}

// TestServeEntry proves the exported serve path answers ranges and
// conditional requests and keeps the security headers.
func TestServeEntry(t *testing.T) {
	d, err := filesystem.NewDisk("m", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "a.png", []byte("0123456789"), filesystem.WriteOptions{ContentType: "image/png", Headers: filesystem.HTTPHeaders{CacheControl: "stored"}}); err != nil {
		t.Fatal(err)
	}
	rec := serveEntry(t, d, "a.png", nil, fshttp.ServeOptions{})
	if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" || rec.Header().Get("Cache-Control") != "stored" || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("full: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if rec.Header().Get(fshttp.HeaderContentTypeOptions) != "nosniff" || rec.Header().Get(fshttp.HeaderCSP) == "" {
		t.Errorf("security headers missing: %v", rec.Header())
	}
	etag := rec.Header().Get("ETag")
	rec = serveEntry(t, d, "a.png", http.Header{"Range": {"bytes=2-4"}}, fshttp.ServeOptions{CacheControl: "private, max-age=60"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "234" || rec.Header().Get("Cache-Control") != "private, max-age=60" {
		t.Errorf("range: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec = serveEntry(t, d, "a.png", http.Header{"If-None-Match": {etag}}, fshttp.ServeOptions{})
	if etag == "" || rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match %q: %d %q", etag, rec.Code, rec.Body)
	}
	// Options cannot loosen the floor: active content is an attachment.
	if _, err := d.WriteBytes(t.Context(), "x.html", []byte("<script>"), filesystem.WriteOptions{ContentType: "text/html"}); err != nil {
		t.Fatal(err)
	}
	rec = serveEntry(t, d, "x.html", nil, fshttp.ServeOptions{ContentDisposition: "inline"})
	if rec.Header().Get("Content-Disposition") != "attachment" || rec.Header().Get(fshttp.HeaderCSP) == "" {
		t.Errorf("active content: %v", rec.Header())
	}
}

// TestServeEntrySendfileAndInlinePDF proves ServeOptions carries the two
// opt-ins Public and Signed accept: Sendfile hands the body to the front
// proxy, InlinePDF swaps the CSP for a PDF only, and the zero value keeps
// both off.
func TestServeEntrySendfileAndInlinePDF(t *testing.T) {
	d, err := filesystem.NewDisk("m", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	for key, ct := range map[string]string{"doc.pdf": "application/pdf", "a.png": "image/png"} {
		if _, err := d.WriteBytes(t.Context(), key, []byte("%PDF-1.4 body"), filesystem.WriteOptions{ContentType: ct}); err != nil {
			t.Fatal(err)
		}
	}
	sf := &fshttp.Sendfile{Path: func(disk, key string) (string, bool) { return "/internal/" + disk + "/" + key, true }}
	rec := serveEntry(t, d, "a.png", nil, fshttp.ServeOptions{Sendfile: sf})
	if got := rec.Header().Get(fshttp.HeaderAccelRedirect); got != "/internal/m/a.png" || rec.Body.Len() != 0 {
		t.Errorf("sendfile: header %q, body %q", got, rec.Body)
	}
	if rec.Header().Get(fshttp.HeaderContentTypeOptions) != "nosniff" {
		t.Errorf("sendfile lost nosniff: %v", rec.Header())
	}
	rec = serveEntry(t, d, "a.png", nil, fshttp.ServeOptions{})
	if rec.Header().Get(fshttp.HeaderAccelRedirect) != "" || rec.Body.Len() == 0 {
		t.Errorf("default must stream: %v %q", rec.Header(), rec.Body)
	}
	if got := serveEntry(t, d, "doc.pdf", nil, fshttp.ServeOptions{InlinePDF: true}).Header().Get(fshttp.HeaderCSP); got != fshttp.InlinePDFCSP {
		t.Errorf("InlinePDF: CSP %q", got)
	}
	if got := serveEntry(t, d, "a.png", nil, fshttp.ServeOptions{InlinePDF: true}).Header().Get(fshttp.HeaderCSP); got == fshttp.InlinePDFCSP {
		t.Error("InlinePDF loosened a PNG's CSP")
	}
	if got := serveEntry(t, d, "doc.pdf", nil, fshttp.ServeOptions{}).Header().Get(fshttp.HeaderCSP); got == fshttp.InlinePDFCSP {
		t.Error("default served a PDF with the inline CSP")
	}
}

// TestServeEntrySendfileRefusesBadKey proves an entry whose key the disk
// refuses (traversal) is answered 400 before any header is set: it never
// reaches Sendfile's mapping, sends no proxy header and no byte, and the
// refusal goes through ServeOptions.Errors when set.
func TestServeEntrySendfileRefusesBadKey(t *testing.T) {
	d, err := filesystem.NewDisk("m", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	e, err := d.WriteBytes(t.Context(), "a.png", []byte("png"), filesystem.WriteOptions{ContentType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	mapped := false
	sf := &fshttp.Sendfile{Path: func(disk, key string) (string, bool) { mapped = true; return "/internal/" + key, true }}
	e.Key = "../a.png"
	rec := httptest.NewRecorder()
	fshttp.ServeEntry(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), d, e, fshttp.ServeOptions{Sendfile: sf})
	if mapped || rec.Header().Get(fshttp.HeaderAccelRedirect) != "" {
		t.Errorf("a traversal key reached Sendfile: mapped %v, header %q", mapped, rec.Header().Get(fshttp.HeaderAccelRedirect))
	}
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "png") || rec.Header().Get("ETag") != "" {
		t.Errorf("a traversal key = %d %v %q, want a bare 400", rec.Code, rec.Header(), rec.Body)
	}
	var seen int
	custom := fshttp.ServeOptions{Errors: func(w http.ResponseWriter, _ *http.Request, status int, _ error) {
		seen = status
		w.WriteHeader(status)
	}}
	rec = httptest.NewRecorder()
	fshttp.ServeEntry(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), d, e, custom)
	if seen != http.StatusBadRequest || rec.Code != http.StatusBadRequest {
		t.Errorf("Errors saw %d, response %d", seen, rec.Code)
	}
}
