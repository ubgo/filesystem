package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
)

// Decisions of the own-handler step: who owns the report, the header the
// demo's stand-in session puts the caller in, and the cache header.
const (
	reportOwner        = "alice"
	headerUser         = "X-Demo-User"
	ownCacheControl    = "private, max-age=300"
	ownReportKey       = "reports/q3.txt"
	ownReportBody      = "Q3 report: revenue up"
	ownPageKey         = "reports/q3.html"
	ownPageBody        = "<script>alert(1)</script>"
	ownRangeFirstBytes = "bytes=0-1"
)

// ownDownload is a download handler that authorises the request itself
// (here: only the owner may read) and then serves through
// fshttp.ServeEntry, so ranges, 304s and the security headers work as on
// fshttp's own handlers. A real app reads the caller from its session.
// ds:def id=owndownload-c7m2x9ve
func ownDownload(disk *filesystem.Disk) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerUser) != reportOwner {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		e, err := disk.Stat(r.Context(), strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
			return
		}
		fshttp.ServeEntry(w, r, disk, e, fshttp.ServeOptions{CacheControl: ownCacheControl, ContentDisposition: "inline"})
	})
}

// ownHandler stores a report and an HTML page on the private disk and
// asks ownDownload for them as another user, as the owner, as a range, as
// a revalidating cache, and for the page.
// ds:def id=ownhandler-q4n8t2wb
func ownHandler(ctx context.Context, a *app, out io.Writer) error {
	for key, body := range map[string]string{ownReportKey: ownReportBody, ownPageKey: ownPageBody} {
		ct := "text/plain; charset=utf-8"
		if strings.HasSuffix(key, ".html") {
			ct = "text/html; charset=utf-8"
		}
		if _, err := a.private.Write(ctx, key, strings.NewReader(body), filesystem.WriteOptions{ContentType: ct}); err != nil {
			return err
		}
	}
	h := ownDownload(a.private)
	get := func(key, user string, header map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/"+key, nil)
		r.Header.Set(headerUser, user)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	fmt.Fprintf(out, "own handler as bob: %d\n", get(ownReportKey, "bob", nil).Code)
	whole := get(ownReportKey, reportOwner, nil)
	fmt.Fprintf(out, "own handler as alice: %d %q | %s | %s\n", whole.Code, whole.Body.String(),
		whole.Header().Get("X-Content-Type-Options"), whole.Header().Get("Cache-Control"))
	part := get(ownReportKey, reportOwner, map[string]string{"Range": ownRangeFirstBytes})
	fmt.Fprintf(out, "own handler Range: %d %q %s\n", part.Code, part.Body.String(), part.Header().Get("Content-Range"))
	again := get(ownReportKey, reportOwner, map[string]string{"If-None-Match": whole.Header().Get("ETag")})
	fmt.Fprintf(out, "own handler If-None-Match: %d\n", again.Code)
	page := get(ownPageKey, reportOwner, nil)
	fmt.Fprintf(out, "own handler html asked inline: %s | %s\n", page.Header().Get("Content-Disposition"), page.Header().Get("Content-Security-Policy"))
	return nil
}
