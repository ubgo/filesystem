package fshttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// gzipHello is "hello" compressed with gzip.
const gzipHello = "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff\xcaH\xcd\xc9\xc9\x07\x04\x00\x00\xff\xff\x86\xa6\x106\x05\x00\x00\x00"

func TestStoredHeadersAreServed(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("pub", memory.New())
	h := filesystem.HTTPHeaders{CacheControl: "public, max-age=60", ContentDisposition: `attachment; filename="hello.txt"`, ContentEncoding: "gzip"}
	_, _ = d.WriteBytes(ctx, "hello.txt", []byte(gzipHello), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic, ContentType: "text/plain", Headers: h})
	srv := httptest.NewServer(fshttp.Public(d))
	defer srv.Close()
	// Asking for gzip explicitly stops the client from decompressing.
	resp, body := do(t, http.MethodGet, srv.URL+"/hello.txt", nil, http.Header{"Accept-Encoding": {"gzip"}})
	if body != gzipHello || resp.Header.Get("Cache-Control") != h.CacheControl ||
		resp.Header.Get("Content-Disposition") != h.ContentDisposition || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("served %q with %v", body, resp.Header)
	}
	// A plain client gets the text, decompressed by the client as usual.
	if _, body := do(t, http.MethodGet, srv.URL+"/hello.txt", nil, nil); body != "hello" {
		t.Fatalf("decompressed body = %q", body)
	}
}

// TestActiveContentIsDownloaded serves HTML, SVG and XML as attachments,
// even when an inline disposition was stored, and images inline.
func TestActiveContentIsDownloaded(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("pub", memory.New())
	pub := filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}
	inline := pub
	inline.Headers = filesystem.HTTPHeaders{ContentDisposition: "inline"}
	_, _ = d.WriteBytes(ctx, "x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), inline)
	_, _ = d.WriteBytes(ctx, "x.html", []byte("<!DOCTYPE html><script>alert(1)</script>"), pub)
	_, _ = d.WriteBytes(ctx, "x.xml", []byte(`<?xml version="1.0"?><a/>`), pub)
	_, _ = d.WriteBytes(ctx, "keep.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic, Headers: filesystem.HTTPHeaders{ContentDisposition: `attachment; filename="logo.svg"`}})
	_, _ = d.WriteBytes(ctx, "x.png", []byte("\x89PNG\r\n\x1a\nrest"), pub)
	srv := httptest.NewServer(fshttp.Public(d))
	defer srv.Close()
	for key, want := range map[string]string{"x.svg": "attachment", "x.html": "attachment", "x.xml": "attachment", "keep.svg": `attachment; filename="logo.svg"`, "x.png": ""} {
		resp, _ := do(t, http.MethodGet, srv.URL+"/"+key, nil, nil)
		if got := resp.Header.Get("Content-Disposition"); got != want {
			t.Errorf("%s: Content-Disposition %q, want %q", key, got, want)
		}
	}
}
