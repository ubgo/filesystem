package fshttp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// TestSignedAllowedIPDeniesAnUnparsableClient proves an IP-restricted URL
// is refused when the connecting address cannot be parsed: a client whose
// address is unknown is never taken to be inside the range.
func TestSignedAllowedIPDeniesAnUnparsableClient(t *testing.T) {
	e := setup(t)
	if _, err := e.disk.Write(t.Context(), "a.txt", strings.NewReader("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	signed := e.sign(t, e.disk, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, AllowedIP: "0.0.0.0/0"})
	for addr, want := range map[string]int{"127.0.0.1:1234": http.StatusOK, "not-an-address": http.StatusForbidden} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, signed, nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		e.server.Config.Handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("from %q: %d, want %d", addr, rec.Code, want)
		}
	}
}

// TestSendfileIfNoneMatchWithoutAnETag proves an object without an ETag
// (the etagless driver of fshttp_test.go) never matches an If-None-Match
// tag and is offloaded in full rather than answered 304.
func TestSendfileIfNoneMatchWithoutAnETag(t *testing.T) {
	d, err := filesystem.NewDisk("assets", etagless{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(t.Context(), "a.png", strings.NewReader("png"), filesystem.WriteOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	h := fshttp.Public(d, fshttp.PublicOptions{Sendfile: (&accel{}).sendfile()})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.test/a.png", nil)
	req.Header.Set("If-None-Match", `"guess"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get(fshttp.HeaderAccelRedirect) == "" {
		t.Fatalf("got %d, offload %q; want 200 offloaded", rec.Code, rec.Header().Get(fshttp.HeaderAccelRedirect))
	}
}
