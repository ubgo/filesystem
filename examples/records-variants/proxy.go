package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"

	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/records"
)

// Decisions of the proxied handler: the cache header of every response
// (public, since the demo's CDN serves only public files) and the slice
// of the variant a resumed download asks for.
const (
	proxyCacheControl = "public, max-age=86400"
	proxyRange        = "bytes=0-99"
)

// newProxyHandler serves GET /variants/{fileID}/{name} by streaming the
// variant itself, so the bucket stays unreachable and a CDN sees one URL.
// ds:def id=newproxyhandler-z6k2w8mr
func newProxyHandler(s *records.Store, p *variants.Plugin) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/variants/", http.StripPrefix("/variants/",
		variants.Handler(s, p, scopeOf, variants.HandlerOptions{
			Mode:         variants.ModeProxy,
			CacheControl: proxyCacheControl,
		})))
	return mux
}

// get calls h with GET path and the given request headers.
func get(h http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	h.ServeHTTP(rec, r)
	return rec
}

// proxied asks the proxy handler for a thumbnail three ways, as a page,
// a resumed download and a revalidating cache would, and prints what each
// got; then a handler with an unknown mode.
// ds:def id=proxied-j3t9n5xa
func proxied(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "proxied.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disks, err := newDisks()
	if err != nil {
		return err
	}
	p, err := newPlugin(transforms, logEager)
	if err != nil {
		return err
	}
	s, err := newStore(ctx, db, disks, p)
	if err != nil {
		return err
	}
	f, err := uploadPhoto(ctx, s, photoW, photoH)
	if err != nil {
		return err
	}
	h := newProxyHandler(s, p)
	path := "/variants/" + f.ID + "/" + nameThumb

	full := get(h, path, nil)
	fmt.Fprintf(w, "proxy GET thumb: %d %s, %d bytes, Location %q, nosniff %t, CSP set %t, Cache-Control %q\n",
		full.Code, full.Header().Get("Content-Type"), full.Body.Len(), full.Header().Get("Location"),
		full.Header().Get("X-Content-Type-Options") == "nosniff", full.Header().Get("Content-Security-Policy") != "",
		full.Header().Get("Cache-Control"))

	part := get(h, path, map[string]string{"Range": proxyRange})
	fmt.Fprintf(w, "proxy GET thumb (Range %s): %d, %d bytes, Content-Range %s, same bytes as the full body %t\n",
		proxyRange, part.Code, part.Body.Len(), part.Header().Get("Content-Range"),
		full.Body.Len() >= part.Body.Len() && string(full.Body.Bytes()[:part.Body.Len()]) == part.Body.String())

	etag := full.Header().Get("ETag")
	again := get(h, path, map[string]string{"If-None-Match": etag})
	fmt.Fprintf(w, "proxy GET thumb (If-None-Match its ETag): %d, %d bytes\n", again.Code, again.Body.Len())

	bad := httptest.NewRecorder()
	variants.Handler(s, p, scopeOf, variants.HandlerOptions{Mode: "stream"}).
		ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/"+f.ID+"/"+nameThumb, nil))
	fmt.Fprintf(w, "handler with mode \"stream\": %d\n", bad.Code)
	return nil
}
