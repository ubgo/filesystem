package upload_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// connectProxy is a minimal CONNECT proxy, the shape of Smokescreen: it
// records every CONNECT target and either refuses with status or tunnels.
type connectProxy struct {
	ln     net.Listener
	status int // non-zero refuses every CONNECT with it
	mu     sync.Mutex
	seen   []string
	auth   []string
}

func newConnectProxy(t *testing.T, status int) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{ln: ln, status: status}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *connectProxy) url() *url.URL { return &url.URL{Scheme: "http", Host: p.ln.Addr().String()} }

func (p *connectProxy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *connectProxy) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, req.Host)
	p.auth = append(p.auth, req.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	if p.status != 0 {
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n\r\n", p.status, http.StatusText(p.status))
		return
	}
	target, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = target.Close() }()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(target, conn) }()
	_, _ = io.Copy(conn, target)
}

// TestFetchHeader: FetchOptions.Header reaches the server; without it the
// request carries Go's defaults only, as before.
func TestFetchHeader(t *testing.T) {
	var got http.Header
	srvURL := server(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, pngData)
	})
	o := loopback
	o.Header = http.Header{"User-Agent": {"acme-preview/1.0"}, "Authorization": {"Bearer internal"}}
	remote, err := upload.Fetch(t.Context(), srvURL+"/a.png", o)
	if err != nil {
		t.Fatal(err)
	}
	_ = remote.Body.Close()
	if got.Get("User-Agent") != "acme-preview/1.0" || got.Get("Authorization") != "Bearer internal" {
		t.Errorf("custom header not sent: %v", got)
	}
	remote, err = upload.Fetch(t.Context(), srvURL+"/a.png", loopback)
	if err != nil {
		t.Fatal(err)
	}
	_ = remote.Body.Close()
	if !strings.HasPrefix(got.Get("User-Agent"), "Go-http-client/") || got.Get("Authorization") != "" {
		t.Errorf("default headers changed: %v", got)
	}
}

// TestFetchProxyKeepsTheGuard: through a proxy the fetch still decides the
// destination: the proxy is asked for the checked address (never a name),
// an address the guard refuses is refused before the proxy is asked, and a
// proxy's own refusal fails closed.
func TestFetchProxyKeepsTheGuard(t *testing.T) {
	srv := httptest.NewServer(serve(pngData))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	byName := "http://localhost:" + target.Port() + "/a.png"

	// Allowed: the request goes through the proxy, to the IP it checked.
	proxy := newConnectProxy(t, 0)
	o := loopback
	o.Proxy = proxy.url()
	o.Proxy.User = url.UserPassword("egress", "s3cret")
	remote, err := upload.Fetch(t.Context(), byName, o)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(remote.Body)
	_ = remote.Body.Close()
	seen := proxy.targets()
	if string(body) != pngData || len(seen) == 0 {
		t.Fatalf("proxied fetch: body %q, CONNECT targets %v", body, seen)
	}
	for _, target := range seen {
		if _, err := netip.ParseAddrPort(target); err != nil {
			t.Errorf("CONNECT %q: the proxy must be given the checked address, not the name", target)
		}
	}
	if !strings.HasPrefix(proxy.auth[0], "Basic ") {
		t.Errorf("Proxy-Authorization = %q", proxy.auth[0])
	}

	// Denied: a loopback target without AllowAddr never reaches the proxy.
	guarded := newConnectProxy(t, 0)
	_, err = upload.Fetch(t.Context(), byName, upload.FetchOptions{Proxy: guarded.url()})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if seen := guarded.targets(); len(seen) != 0 {
		t.Errorf("the proxy was asked for a refused destination: %v", seen)
	}

	// The proxy's own refusal (an egress policy's 403) fails closed.
	refusing := newConnectProxy(t, http.StatusForbidden)
	o.Proxy = refusing.url()
	_, err = upload.Fetch(t.Context(), byName, o)
	wantCode(t, err, filesystem.CodeInvalidArgument)

	// Only plain http proxies.
	_, err = upload.Fetch(t.Context(), byName, upload.FetchOptions{Proxy: &url.URL{Scheme: "socks5", Host: "127.0.0.1:1080"}})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestKeyContext: a KeyContextFunc sees the upload's context, its error
// refuses the upload, and a policy with both Key and KeyContext is
// refused by Validate.
func TestKeyContext(t *testing.T) {
	type tenantKey struct{}
	d := newDisk(t, memory.New())
	p := images
	p.KeyContext = func(ctx context.Context, info upload.Info) (string, error) {
		tenant, ok := ctx.Value(tenantKey{}).(string)
		if !ok {
			return "", filesystem.Errorf(filesystem.CodePermission, "no tenant")
		}
		return "tenants/" + tenant + "/avatar.png", nil
	}
	ctx := context.WithValue(t.Context(), tenantKey{}, "acme")
	res, err := upload.Receive(ctx, d, reader(pngData), "a.png", p)
	if err != nil || res.Key != "tenants/acme/avatar.png" {
		t.Fatalf("Receive = %+v, %v", res, err)
	}
	_, err = upload.Receive(t.Context(), d, reader(pngData), "a.png", p)
	wantCode(t, err, filesystem.CodePermission)

	both := p
	both.Key = upload.RandomKey("x")
	wantCode(t, both.Validate(), filesystem.CodeInvalidArgument)

	// Default unchanged: neither set is RandomKey("").
	res, err = upload.Receive(t.Context(), d, reader(pngData), "a.png", images)
	if err != nil || strings.Contains(res.Key, "/") || !strings.HasSuffix(res.Key, ".png") {
		t.Errorf("default key = %q, %v", res.Key, err)
	}
}

// uppyComplete runs create and complete for one part, returning the
// complete response.
func uppyComplete(t *testing.T, d *filesystem.Disk, c uppyClient, extra string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := c.call(http.MethodPost, "/multipart", `{"filename":"big.png","type":"image/png"}`)
	var created struct{ UploadID, Key string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	part, err := d.UploadPart(t.Context(), created.Key, created.UploadID, 1, reader(pngData), int64(len(pngData)))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"parts":[{"PartNumber":1,"ETag":%q}]%s}`, part.ETag, extra)
	return c.call(http.MethodPost, "/multipart/"+created.UploadID+"/complete?key="+created.Key, body), created.Key
}

// TestUppyOnStored: the hook sees the confirmed file with the client's
// name; its failure deletes the file and fails the request.
func TestUppyOnStored(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	var got []upload.Result
	ok := uppyClient{t, upload.Uppy(d, images, upload.UppyOptions{OnStored: func(_ context.Context, _ *http.Request, f upload.Result) error {
		got = append(got, f)
		return nil
	}})}
	rec, key := uppyComplete(t, d, ok, `,"filename":"holiday.png"`)
	if rec.Code != 200 || len(got) != 1 || got[0].Key != key || got[0].Filename != "holiday.png" {
		t.Fatalf("complete = %d %s, OnStored saw %+v", rec.Code, rec.Body, got)
	}

	refuse := uppyClient{t, upload.Uppy(d, images, upload.UppyOptions{OnStored: func(context.Context, *http.Request, upload.Result) error {
		return filesystem.Errorf(filesystem.CodeConflict, "row exists")
	}})}
	rec, key = uppyComplete(t, d, refuse, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("failed OnStored = %d %s", rec.Code, rec.Body)
	}
	if exists, _ := d.Exists(t.Context(), key); exists {
		t.Error("the file stayed after OnStored failed")
	}

	// The name is held to the policy's name rules at complete too.
	rec, _ = uppyComplete(t, d, ok, `,"filename":"shell.php.png"`)
	if rec.Code != http.StatusUnsupportedMediaType && rec.Code != http.StatusBadRequest {
		t.Errorf("a refused name at complete = %d %s", rec.Code, rec.Body)
	}
}

// TestUploadErrorWriter: every upload handler answers through Errors, and
// without it keeps the ErrorResponse JSON.
func TestUploadErrorWriter(t *testing.T) {
	d := newDisk(t, presigning{memory.New()})
	var seen []error
	ew := func(w http.ResponseWriter, _ *http.Request, status int, err error) {
		seen = append(seen, err)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"status":%d,"type":%q}`, status, filesystem.CodeOf(err))
	}
	staging := upload.ChunkedOptions{Staging: "staging", Owner: func(*http.Request) (string, error) { return "u", nil }, Errors: ew}
	chunked, err := upload.Chunked(d, images, staging)
	if err != nil {
		t.Fatal(err)
	}
	handlers := map[string]http.Handler{
		"handler": upload.Handler(d, images, upload.HandlerOptions{Errors: ew}),
		"presign": upload.PresignHandlerWith(d, images, 0, upload.PresignHandlerOptions{Errors: ew}),
		"uppy":    upload.Uppy(d, images, upload.UppyOptions{Errors: ew}),
		"limits":  upload.LimitsHandler(images, upload.LimitsOptions{Errors: ew}),
		"chunked": chunked,
	}
	for name, h := range handlers {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/multipart", nil))
		if rec.Code < 400 || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: %d %q %s", name, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
	}
	for _, err := range seen {
		if err == nil {
			t.Error("the ErrorWriter was given a nil error")
		}
	}
	// A Resolver's refusal goes through it too.
	resolved := upload.Handler(d, images, upload.HandlerOptions{Errors: ew, Resolve: func(*http.Request, upload.Policy) (upload.Policy, error) {
		return upload.Policy{}, filesystem.Errorf(filesystem.CodePermission, "no plan")
	}})
	rec := httptest.NewRecorder()
	resolved.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"type":"permission"`) {
		t.Errorf("resolver refusal = %d %s", rec.Code, rec.Body)
	}
	// A Resolver returning a policy Validate refuses is a misconfiguration:
	// the writer gets a 500 and the cause, never a 4xx the client could act on.
	seen = nil
	broken := upload.Handler(d, images, upload.HandlerOptions{Errors: ew, Resolve: func(*http.Request, upload.Policy) (upload.Policy, error) {
		return upload.Policy{}, nil
	}})
	rec = httptest.NewRecorder()
	broken.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil))
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/problem+json" || len(seen) != 1 || seen[0] == nil {
		t.Errorf("misconfigured resolve = %d %s, seen %v", rec.Code, rec.Body, seen)
	}

	// Default unchanged.
	rec = httptest.NewRecorder()
	upload.Handler(d, images, upload.HandlerOptions{}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	var body upload.ErrorResponse
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Content-Type") != "application/json" || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Code != filesystem.CodeInvalidArgument || body.Error != "uploads must be POSTed" {
		t.Errorf("default = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}

// rawProxy accepts one CONNECT per connection and answers with reply,
// written as is: a canned answer stands in for a proxy that hangs up, or
// that sends the target's first bytes in the same packet as its 200.
func rawProxy(t *testing.T, reply string) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = io.WriteString(conn, reply)
				if reply != "" {
					// Hold the tunnel open until the client has read
					// the answer and its request is in.
					_, _ = http.ReadRequest(br)
				}
			}()
		}
	}()
	return &url.URL{Scheme: "http", Host: ln.Addr().String()}
}

// TestFetchProxyEdges: an IP-literal target is checked as it is, a name
// that does not resolve and a proxy that is down or hangs up fail the
// fetch, bytes the target sent with the proxy's 200 reach the client,
// and Client() refuses a proxy Fetch would have refused.
func TestFetchProxyEdges(t *testing.T) {
	ctx := t.Context()
	srv := httptest.NewServer(serve(pngData))
	t.Cleanup(srv.Close)

	// An IP literal, with a deadline on the context (the CONNECT is bound
	// by it).
	proxy := newConnectProxy(t, 0)
	o := loopback
	o.Proxy = proxy.url()
	dl, cancel := context.WithTimeout(ctx, 5*time.Second) // sooner than the dial timeout
	defer cancel()
	remote, err := upload.Fetch(dl, srv.URL+"/a.png", o)
	if err != nil {
		t.Fatal(err)
	}
	_ = remote.Body.Close()
	if seen := proxy.targets(); len(seen) != 1 || seen[0] != strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("CONNECT targets %v", seen)
	}

	// A name that does not resolve fails before the proxy is asked.
	if _, err := upload.Fetch(ctx, "http://no-such-host.invalid/a.png", o); err == nil {
		t.Error("an unresolvable name was fetched")
	}
	if seen := proxy.targets(); len(seen) != 1 {
		t.Errorf("the proxy was asked for an unresolvable name: %v", seen)
	}

	// A proxy that is down, and one named without a port (port 80).
	down, _ := net.Listen("tcp", "127.0.0.1:0")
	downURL := &url.URL{Scheme: "http", Host: down.Addr().String()}
	_ = down.Close()
	for _, p := range []*url.URL{downURL, {Scheme: "http", Host: "127.0.0.1"}} {
		o.Proxy = p
		if _, err := upload.Fetch(ctx, srv.URL+"/a.png", o); err == nil {
			t.Errorf("fetch through %v succeeded", p)
		}
	}

	// A proxy that hangs up without answering.
	o.Proxy = rawProxy(t, "")
	if _, err := upload.Fetch(ctx, srv.URL+"/a.png", o); err == nil {
		t.Error("fetch through a proxy that hung up succeeded")
	}

	// The target's response arrives in the same packet as the 200.
	canned := "HTTP/1.1 200 Connection established\r\n\r\n" +
		"HTTP/1.1 200 OK\r\nContent-Type: image/png\r\nContent-Length: " + fmt.Sprint(len(pngData)) + "\r\n\r\n" + pngData
	o.Proxy = rawProxy(t, canned)
	remote, err = upload.Fetch(ctx, srv.URL+"/a.png", o)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(remote.Body)
	_ = remote.Body.Close()
	if string(body) != pngData {
		t.Errorf("buffered body = %q", body)
	}

	// Client() checks the proxy too, for callers that skip Fetch.
	bad := upload.FetchOptions{Proxy: &url.URL{Scheme: "https", Host: "127.0.0.1:1"}}
	if _, err := bad.Client().Get(srv.URL + "/a.png"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("Client() with an https proxy = %v", err)
	}
}
