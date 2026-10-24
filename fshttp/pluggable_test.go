package fshttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

// problemJSON is an RFC 9457 style ErrorWriter, the kind of house envelope
// an application plugs in; it records the error it was given.
type problemJSON struct{ seen []error }

// problem is the body problemJSON writes.
type problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Code   string `json:"code"`
}

// mediaProblem is the content type problemJSON answers with.
const mediaProblem = "application/problem+json"

func (p *problemJSON) write(w http.ResponseWriter, _ *http.Request, status int, err error) {
	p.seen = append(p.seen, err)
	w.Header().Set("Content-Type", mediaProblem)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{Status: status, Title: fshttp.ClientMessage(err), Code: string(filesystem.CodeOf(err))})
}

func decodeProblem(t *testing.T, resp *http.Response, body string) problem {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != mediaProblem {
		t.Fatalf("Content-Type = %q, want the custom writer's; body %q", ct, body)
	}
	var p problem
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return p
}

func TestPublicErrorWriter(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("pub", memory.New())
	_, _ = d.WriteBytes(ctx, "secret.txt", []byte("s"), filesystem.WriteOptions{})
	pj := &problemJSON{}
	custom := httptest.NewServer(fshttp.Public(d, fshttp.PublicOptions{Errors: pj.write}))
	defer custom.Close()
	plain := httptest.NewServer(fshttp.Public(d))
	defer plain.Close()

	cases := []struct {
		name, method, path string
		status             int
		code               filesystem.Code
		plainBody          string
	}{
		{"private", http.MethodGet, "/secret.txt", 404, filesystem.CodeNotFound, "404 page not found\n"},
		{"missing", http.MethodGet, "/missing", 404, filesystem.CodeNotFound, "Not Found\n"},
		{"bad key", http.MethodGet, "/../x", 400, filesystem.CodeInvalidKey, "Bad Request\n"},
		{"method", http.MethodPost, "/secret.txt", 405, filesystem.CodeInvalidArgument, "Method Not Allowed\n"},
	}
	for _, c := range cases {
		resp, body := do(t, c.method, custom.URL+c.path, nil, nil)
		if resp.StatusCode != c.status {
			t.Errorf("%s: custom status %d, want %d", c.name, resp.StatusCode, c.status)
			continue
		}
		if p := decodeProblem(t, resp, body); p.Code != string(c.code) || p.Status != c.status {
			t.Errorf("%s: custom body %+v", c.name, p)
		}
		// The default is unchanged: plain status text.
		resp, body = do(t, c.method, plain.URL+c.path, nil, nil)
		if resp.StatusCode != c.status || body != c.plainBody || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("%s: default = %d %q %q", c.name, resp.StatusCode, body, resp.Header.Get("Content-Type"))
		}
	}
	for _, err := range pj.seen {
		if err == nil {
			t.Error("the ErrorWriter was given a nil error")
		}
	}
}

func TestSignedErrorWriter(t *testing.T) {
	pj := &problemJSON{}
	e := setup(t, fshttp.SignedOptions{Errors: pj.write})
	_, _ = e.disk.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{})
	url := e.sign(t, e.disk, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	resp, body := do(t, http.MethodGet, url+"x", nil, nil) // a broken signature
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered = %d", resp.StatusCode)
	}
	if p := decodeProblem(t, resp, body); p.Code != string(filesystem.CodePermission) {
		t.Errorf("tampered body %+v", p)
	}
	// A PUT whose Content-Type differs from the signed one is refused by
	// the handler itself, through the writer.
	put := e.sign(t, e.disk, "b.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain"})
	resp, body = do(t, http.MethodPut, put, strings.NewReader("b"), http.Header{"Content-Type": {"image/png"}})
	if resp.StatusCode != http.StatusForbidden || decodeProblem(t, resp, body).Code != string(filesystem.CodePermission) {
		t.Errorf("mismatched PUT = %d %q", resp.StatusCode, body)
	}

	// Default unchanged.
	plain := setup(t)
	resp, body = do(t, http.MethodGet, plain.server.URL+"/files/uploads/a.txt", nil, nil)
	if resp.StatusCode != http.StatusForbidden || body != "Forbidden\n" {
		t.Errorf("default = %d %q", resp.StatusCode, body)
	}
}

func TestZipOptionsCacheControlAndErrors(t *testing.T) {
	z := newZipEnv(t)
	z.files = []fshttp.ZipFile{{Key: "a.txt"}}
	resp, _, err := z.get(t, http.MethodHead)
	if err != nil || resp.Header.Get("Cache-Control") != fshttp.DefaultSignedCacheControl {
		t.Fatalf("default Cache-Control = %v %v", resp, err)
	}
	z.opts.CacheControl = "public, max-age=60"
	if resp, _, _ := z.get(t, http.MethodHead); resp.Header.Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("custom Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	pj := &problemJSON{}
	z.opts.Errors = pj.write
	z.files = []fshttp.ZipFile{{Key: "missing"}}
	resp, body, _ := z.get(t, http.MethodGet)
	if resp.StatusCode != http.StatusNotFound || decodeProblem(t, resp, string(body)).Code != string(filesystem.CodeNotFound) {
		t.Errorf("custom zip error = %d %q", resp.StatusCode, body)
	}
}

func TestInlinePDF(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("pub", memory.New())
	pdf := []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n")
	pub := filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}
	_, _ = d.WriteBytes(ctx, "doc.pdf", pdf, pub)
	_, _ = d.WriteBytes(ctx, "logo.png", []byte("\x89PNG\r\n\x1a\n0000"), pub)
	_, _ = d.WriteBytes(ctx, "page.html", []byte("<html><script>x</script></html>"), pub)
	att := pub
	att.Headers = filesystem.HTTPHeaders{ContentDisposition: "attachment"}
	_, _ = d.WriteBytes(ctx, "dl.pdf", pdf, att)

	csp := func(srv *httptest.Server, key string) string {
		resp, _ := do(t, http.MethodGet, srv.URL+"/"+key, nil, nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get(fshttp.HeaderContentTypeOptions) != "nosniff" {
			t.Fatalf("%s = %d %v", key, resp.StatusCode, resp.Header)
		}
		return resp.Header.Get(fshttp.HeaderCSP)
	}
	plain := httptest.NewServer(fshttp.Public(d))
	defer plain.Close()
	inline := httptest.NewServer(fshttp.Public(d, fshttp.PublicOptions{InlinePDF: true}))
	defer inline.Close()

	if got := csp(plain, "doc.pdf"); got != "default-src 'none'; sandbox" {
		t.Errorf("default PDF CSP = %q, want the sandbox", got)
	}
	if got := csp(inline, "doc.pdf"); got != fshttp.InlinePDFCSP {
		t.Errorf("opted-in PDF CSP = %q", got)
	}
	for _, key := range []string{"logo.png", "page.html", "dl.pdf"} {
		if got := csp(inline, key); !strings.Contains(got, "sandbox") {
			t.Errorf("%s with InlinePDF: CSP %q lost the sandbox", key, got)
		}
	}

	// Signed has the same opt-in.
	e := setup(t, fshttp.SignedOptions{InlinePDF: true})
	_, _ = e.disk.WriteBytes(ctx, "doc.pdf", pdf, filesystem.WriteOptions{})
	resp, _ := do(t, http.MethodGet, e.sign(t, e.disk, "doc.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}), nil, nil)
	if resp.Header.Get(fshttp.HeaderCSP) != fshttp.InlinePDFCSP {
		t.Errorf("signed PDF CSP = %q", resp.Header.Get(fshttp.HeaderCSP))
	}
}

// bearer is a RequestVerifier taking its token from the Authorization
// header, a model the query-only Verifier could not express.
type bearer struct {
	tokens map[string]signedurl.Claims
	err    error
}

func (b bearer) Verify(string, string, filesystem.HTTPMethod, url.Values) (signedurl.Claims, error) {
	return signedurl.Claims{}, filesystem.ErrPermission
}

func (b bearer) VerifyRequest(r *http.Request, _ filesystem.HTTPMethod) (signedurl.Claims, error) {
	if b.err != nil {
		return signedurl.Claims{}, b.err
	}
	c, ok := b.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return signedurl.Claims{}, filesystem.ErrPermission
	}
	return c, nil
}

func TestRequestVerifier(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("uploads", memory.New())
	_, _ = d.WriteBytes(ctx, "a.txt", []byte("alpha"), filesystem.WriteOptions{})
	m, _ := filesystem.NewManager(d)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	v := bearer{tokens: map[string]signedurl.Claims{
		"good":    {Disk: "uploads", Key: "a.txt", Method: filesystem.MethodGet, ExpiresAt: later},
		"zero":    {},
		"put":     {Disk: "uploads", Key: "a.txt", Method: filesystem.MethodPut, ExpiresAt: later},
		"noexp":   {Disk: "uploads", Key: "a.txt", Method: filesystem.MethodGet},
		"expired": {Disk: "uploads", Key: "a.txt", Method: filesystem.MethodGet, ExpiresAt: now},
	}}
	opts := fshttp.SignedOptions{Now: func() time.Time { return now }}
	srv := httptest.NewServer(fshttp.Signed(m, v, opts))
	defer srv.Close()
	get := func(srv *httptest.Server, token string) (*http.Response, string) {
		return do(t, http.MethodGet, srv.URL+"/anything/the/verifier/reads", nil, http.Header{"Authorization": {"Bearer " + token}})
	}
	// Allow: the claims choose the object, not the path.
	if resp, body := get(srv, "good"); resp.StatusCode != http.StatusOK || body != "alpha" {
		t.Errorf("good token = %d %q", resp.StatusCode, body)
	}
	// Deny, and claims that grant nothing, all fail closed with 403.
	for _, token := range []string{"unknown", "zero", "put", "noexp", "expired"} {
		if resp, _ := get(srv, token); resp.StatusCode != http.StatusForbidden {
			t.Errorf("token %q = %d, want 403", token, resp.StatusCode)
		}
	}
	// A verifier's own failure is never a grant.
	broken := httptest.NewServer(fshttp.Signed(m, bearer{err: filesystem.Errorf(filesystem.CodeUnavailable, "token store down")}, opts))
	defer broken.Close()
	if resp, body := get(broken, "good"); resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(body, "alpha") {
		t.Errorf("broken verifier = %d %q", resp.StatusCode, body)
	}
}
