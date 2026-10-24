package upload_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// loopback permits the test servers, which listen on 127.0.0.1.
var loopback = upload.FetchOptions{AllowAddr: func(a netip.Addr) bool { return a.IsLoopback() }}

func TestPublicAddr(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !upload.PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "fe80::1", "fd00::1",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1", "100.64.0.1", "192.0.0.1", "198.18.0.1", "240.0.0.1",
		"255.255.255.255", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1",
	} {
		if upload.PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
}

// server answers every request with handler.
func server(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func serve(body string, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		_, _ = io.WriteString(w, body)
	}
}

func TestFromURL(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, memory.New())
	cases := []struct {
		name, path string
		handler    http.HandlerFunc
		filename   string
	}{
		{"name from the path", "/img/cat.png?size=large", serve(pngData, "Content-Type", "text/html"), "cat.png"},
		{"name from Content-Disposition", "/download", serve(pngData, "Content-Disposition", `attachment; filename="../../etc/photo.png"`), "photo.png"},
		{"encoded name", "/d", serve(pngData, "Content-Disposition", `attachment; filename*=UTF-8''%C3%A9t%C3%A9.png`), "été.png"},
		{"no name at all", "/", serve(pngData), "download"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := upload.FromURL(ctx, disk, server(t, c.handler)+c.path, images, loopback)
			if err != nil {
				t.Fatal(err)
			}
			if res.Filename != c.filename || res.ContentType != "image/png" || res.Size != int64(len(pngData)) {
				t.Fatalf("result = %+v", res)
			}
			if got, _ := disk.ReadAll(ctx, res.Key); string(got) != pngData {
				t.Fatalf("stored %q", got)
			}
		})
	}
}

func TestFromURLRefusals(t *testing.T) {
	ctx := t.Context()
	disk := newDisk(t, memory.New())
	big := strings.Repeat("x", 64)
	small := upload.Policy{MaxSize: 32, Allowed: []string{"*/*"}}
	redirect := func(to string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, to, http.StatusFound) }
	}
	var hops int
	loop := func(w http.ResponseWriter, r *http.Request) { hops++; http.Redirect(w, r, "/again", http.StatusFound) }
	ok := server(t, serve(pngData))
	cases := []struct {
		name string
		url  string
		p    upload.Policy
		o    upload.FetchOptions
		code filesystem.Code
	}{
		{"loopback without AllowAddr", ok, images, upload.FetchOptions{}, filesystem.CodeInvalidArgument},
		{"redirect to a private address", server(t, redirect("http://10.0.0.1/x.png")), images, loopback, filesystem.CodeInvalidArgument},
		{"redirect to another scheme", server(t, redirect("ftp://example.com/x.png")), images, loopback, filesystem.CodeInvalidArgument},
		{"too many redirects", server(t, loop), images, loopback, filesystem.CodeInvalidArgument},
		{"no redirects allowed", server(t, redirect("/x.png")), images, upload.FetchOptions{AllowAddr: loopback.AllowAddr, MaxRedirects: -1}, filesystem.CodeInvalidArgument},
		{"not a URL", "http://[::1", images, loopback, filesystem.CodeInvalidArgument},
		{"not http", "file:///etc/passwd", images, loopback, filesystem.CodeInvalidArgument},
		{"relative", "/x.png", images, loopback, filesystem.CodeInvalidArgument},
		{"credentials", strings.Replace(ok, "http://", "http://user:pw@", 1), images, loopback, filesystem.CodeInvalidArgument},
		{"404", server(t, http.NotFound), images, loopback, filesystem.CodeNotFound},
		{"410", server(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }), images, loopback, filesystem.CodeNotFound},
		{"403", server(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }), images, loopback, filesystem.CodeInvalidArgument},
		{"429", server(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }), images, loopback, filesystem.CodeUnavailable},
		{"503", server(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }), images, loopback, filesystem.CodeUnavailable},
		{"announced too large", server(t, serve(big, "Content-Length", strconv.Itoa(len(big)))), small, loopback, filesystem.CodeTooLarge},
		{"streamed too large", server(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, big[:16])
			w.(http.Flusher).Flush() // chunked: no Content-Length
			_, _ = io.WriteString(w, big[16:])
		}), small, loopback, filesystem.CodeTooLarge},
		{"refused type", server(t, serve(htmlData, "Content-Type", "image/png")), images, loopback, filesystem.CodeRejected},
		{"invalid policy", ok, upload.Policy{}, loopback, filesystem.CodeInvalidArgument},
		{"timeout", server(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }), images,
			upload.FetchOptions{AllowAddr: loopback.AllowAddr, Timeout: 50 * time.Millisecond}, filesystem.CodeUnavailable},
		{"connection refused", closedPort(t), images, loopback, filesystem.CodeUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := upload.FromURL(ctx, disk, c.url, c.p, c.o)
			wantCode(t, err, c.code)
		})
	}
	if hops != upload.DefaultMaxRedirects+1 {
		t.Errorf("followed %d redirects", hops-1)
	}
	if n := countKeys(t, disk); n != 0 {
		t.Errorf("%d objects stored by refused fetches", n)
	}
}

// closedPort is a URL nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr + "/x.png"
}

func countKeys(t *testing.T, d *filesystem.Disk) int {
	t.Helper()
	page, err := d.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Entries)
}

// TestFetch reads a Remote directly, as records.Store.Upload would.
func TestFetch(t *testing.T) {
	url := server(t, serve(pngData, "Content-Length", strconv.Itoa(len(pngData))))
	remote, err := upload.Fetch(t.Context(), url+"/a.png", loopback)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(remote.Body)
	if err := remote.Body.Close(); err != nil || string(body) != pngData || remote.Size != int64(len(pngData)) || remote.Filename != "a.png" {
		t.Fatalf("remote = %+v, body %q, close %v", remote, body, err)
	}
}

// TestFetchReportsTheFinalURL: Remote.URL is where redirects ended, so a
// page's relative links resolve against it.
func TestFetchReportsTheFinalURL(t *testing.T) {
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/moved/page.png", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(pngData))
	})
	remote, err := upload.Fetch(t.Context(), url+"/start", loopback)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = remote.Body.Close() }()
	if remote.URL != url+"/moved/page.png" {
		t.Fatalf("URL = %q, want %q", remote.URL, url+"/moved/page.png")
	}
}

func TestFetchKeepsRetryAfter(t *testing.T) {
	throttle := func(value string) string {
		return server(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", value)
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	_, err := upload.Fetch(t.Context(), throttle("7"), loopback)
	if after, ok := filesystem.RetryAfterOf(err); !ok || after != 7*time.Second {
		t.Errorf("Retry-After 7: %v %v (%v)", after, ok, err)
	}
	_, err = upload.Fetch(t.Context(), throttle("soon"), loopback)
	if _, ok := filesystem.RetryAfterOf(err); ok || filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("malformed Retry-After: %v", err)
	}
}

// TestClientGuardsEveryRequest: the exported client refuses a loopback
// server unless AllowAddr lets it through, and with MaxRedirects -1 a
// redirect is an error rather than followed.
func TestClientGuardsEveryRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "/", http.StatusFound)
		}
	}))
	defer srv.Close()
	if resp, err := (upload.FetchOptions{}).Client().Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the guarded client reached a loopback server")
	}
	c := upload.FetchOptions{AllowAddr: loopback.AllowAddr, MaxRedirects: -1}.Client()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("an allowed address: %v", err)
	}
	_ = resp.Body.Close()
	if resp, err := c.Get(srv.URL + "/hop"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("followed a redirect with MaxRedirects -1")
	}
}
