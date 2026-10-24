package fshttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

var longLived = filesystem.HTTPHeaders{CacheControl: "public, max-age=31536000, immutable"}

// TestPublicCacheControl pins Public's order: the object's stored value,
// then the configured one, then the safe default.
func TestPublicCacheControl(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("pub", memory.New())
	pub := filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}
	_, _ = d.WriteBytes(ctx, "plain.txt", []byte("x"), pub)
	stored := pub
	stored.Headers = longLived
	_, _ = d.WriteBytes(ctx, "stored.txt", []byte("x"), stored)
	cases := []struct {
		h    http.Handler
		key  string
		want string
	}{
		{fshttp.Public(d), "plain.txt", fshttp.DefaultPublicCacheControl},
		{fshttp.Public(d), "stored.txt", longLived.CacheControl},
		{fshttp.Public(d, fshttp.PublicOptions{CacheControl: "public, max-age=600"}), "plain.txt", "public, max-age=600"},
		{fshttp.Public(d, fshttp.PublicOptions{CacheControl: "public, max-age=600"}), "stored.txt", longLived.CacheControl},
		{fshttp.Public(d, fshttp.PublicOptions{CacheControl: "public, max-age=600", ForceCacheControl: true}), "stored.txt", "public, max-age=600"},
		{fshttp.Public(d, fshttp.PublicOptions{ForceCacheControl: true}), "stored.txt", fshttp.DefaultPublicCacheControl},
	}
	for _, c := range cases {
		srv := httptest.NewServer(c.h)
		resp, _ := do(t, http.MethodGet, srv.URL+"/"+c.key, nil, nil)
		srv.Close()
		if got := resp.Header.Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control %q, want %q", c.key, got, c.want)
		}
	}
}

// TestSignedCacheControl pins that a signed download sends the configured
// value, else one no cache stores, whatever the object stored, and the
// stored value only when StoredCacheControl asks for it.
func TestSignedCacheControl(t *testing.T) {
	for _, c := range []struct {
		opts fshttp.SignedOptions
		key  string
		want string
	}{
		{fshttp.SignedOptions{}, "stored.txt", fshttp.DefaultSignedCacheControl},
		{fshttp.SignedOptions{}, "plain.txt", fshttp.DefaultSignedCacheControl},
		{fshttp.SignedOptions{CacheControl: "private, max-age=60"}, "stored.txt", "private, max-age=60"},
		{fshttp.SignedOptions{CacheControl: "private, max-age=60"}, "plain.txt", "private, max-age=60"},
		{fshttp.SignedOptions{StoredCacheControl: true}, "stored.txt", longLived.CacheControl},
		{fshttp.SignedOptions{StoredCacheControl: true}, "plain.txt", fshttp.DefaultSignedCacheControl},
		{fshttp.SignedOptions{StoredCacheControl: true, CacheControl: "private, max-age=60"}, "plain.txt", "private, max-age=60"},
	} {
		e := setup(t, c.opts)
		_, _ = e.disk.WriteBytes(t.Context(), "plain.txt", []byte("x"), filesystem.WriteOptions{})
		_, _ = e.disk.WriteBytes(t.Context(), "stored.txt", []byte("x"), filesystem.WriteOptions{Headers: longLived})
		resp, _ := do(t, http.MethodGet, e.sign(t, e.disk, c.key, get), nil, nil)
		if got := resp.Header.Get("Cache-Control"); got != c.want {
			t.Errorf("%+v %s: Cache-Control %q, want %q", c.opts, c.key, got, c.want)
		}
	}
}
