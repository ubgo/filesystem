package fshttp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/signedurl"
)

var secret = signedurl.Key{ID: "k1", Secret: bytes.Repeat([]byte("s"), 32)}

type env struct {
	server *httptest.Server
	disk   *filesystem.Disk
	ro     *filesystem.Disk
}

func setup(t *testing.T, opts ...fshttp.SignedOptions) *env {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	signer, err := signedurl.New(srv.URL+"/files", []signedurl.Key{secret})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("uploads", memory.New(), filesystem.WithURLSigner(signer))
	ro, _ := filesystem.NewDisk("archive", memory.New(), filesystem.WithURLSigner(signer), filesystem.WithInterceptors(readOnlyWrites()))
	m, _ := filesystem.NewManager(d, ro)
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(m, signer, opts...)))
	return &env{server: srv, disk: d, ro: ro}
}

// readOnlyWrites refuses writes but, unlike filesystem.ReadOnly, still lets
// the disk issue signed PUT URLs, so the handler's own refusal is tested.
func readOnlyWrites() filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite {
			return filesystem.ErrReadOnly
		}
		return call(ctx)
	}
}

func (e *env) sign(t *testing.T, d *filesystem.Disk, key string, o filesystem.PresignOptions) string {
	t.Helper()
	req, err := d.SignedURL(t.Context(), key, o)
	if err != nil {
		t.Fatal(err)
	}
	return req.URL
}

func do(t *testing.T, method, url string, body io.Reader, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var get = filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}

func TestSignedDownload(t *testing.T) {
	e := setup(t)
	ctx := t.Context()
	_, _ = e.disk.WriteBytes(ctx, "docs/report.txt", []byte("0123456789"), filesystem.WriteOptions{})
	u := e.sign(t, e.disk, "docs/report.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentDisposition: `attachment; filename="r.txt"`})
	resp, body := do(t, http.MethodGet, u, nil, nil)
	if resp.StatusCode != 200 || body != "0123456789" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}
	h := resp.Header
	if h.Get("Content-Disposition") != `attachment; filename="r.txt"` || h.Get(fshttp.HeaderContentTypeOptions) != "nosniff" ||
		!strings.Contains(h.Get(fshttp.HeaderCSP), "sandbox") || h.Get("ETag") == "" || h.Get("Accept-Ranges") != "bytes" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Errorf("headers = %v", h)
	}
	resp, body = do(t, http.MethodGet, u, nil, http.Header{"Range": {"bytes=2-4"}})
	if resp.StatusCode != http.StatusPartialContent || body != "234" {
		t.Errorf("range = %d %q", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, u, nil, http.Header{"Range": {"bytes=-3"}})
	if resp.StatusCode != http.StatusPartialContent || body != "789" {
		t.Errorf("suffix range = %d %q", resp.StatusCode, body)
	}
	resp, _ = do(t, http.MethodGet, u, nil, http.Header{"If-None-Match": {h.Get("ETag")}})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d", resp.StatusCode)
	}
	resp, body = do(t, http.MethodHead, u, nil, nil)
	if resp.StatusCode != 200 || body != "" || resp.ContentLength != 10 {
		t.Errorf("HEAD = %d %q len %d", resp.StatusCode, body, resp.ContentLength)
	}
	typed := e.sign(t, e.disk, "docs/report.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "application/x-forced"})
	if resp, _ := do(t, http.MethodGet, typed, nil, nil); resp.Header.Get("Content-Type") != "application/x-forced" {
		t.Errorf("signed content type override = %q", resp.Header.Get("Content-Type"))
	}
	_, _ = e.disk.WriteBytes(ctx, "untyped", []byte("x"), filesystem.WriteOptions{})
}

func TestSignedDownloadRefusals(t *testing.T) {
	e := setup(t)
	_, _ = e.disk.WriteBytes(t.Context(), "a.txt", []byte("x"), filesystem.WriteOptions{})
	u := e.sign(t, e.disk, "a.txt", get)
	cases := []struct {
		method, url string
		want        int
	}{
		{http.MethodGet, strings.Replace(u, "sig=", "sig=00", 1), 403},
		{http.MethodGet, strings.Replace(u, "/a.txt?", "/b.txt?", 1), 403},
		{http.MethodPut, u, 403},
		{http.MethodDelete, u, 405},
		{http.MethodGet, e.server.URL + "/files/uploads", 404},
		{http.MethodGet, e.server.URL + "/files//a.txt", 404},
		{http.MethodGet, e.sign(t, e.disk, "missing.txt", get), 404},
	}
	for _, c := range cases {
		resp, body := do(t, c.method, c.url, nil, nil)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.url, resp.StatusCode, c.want)
		}
		if strings.Contains(body, "filesystem:") {
			t.Errorf("internal error leaked: %q", body)
		}
	}
	// A disk that is signed for but not registered.
	other, _ := filesystem.NewDisk("other", memory.New(), filesystem.WithURLSigner(mustSigner(t, e.server.URL+"/files")))
	if resp, _ := do(t, http.MethodGet, e.sign(t, other, "a.txt", get), nil, nil); resp.StatusCode != 404 {
		t.Errorf("unknown disk = %d", resp.StatusCode)
	}
}

func mustSigner(t *testing.T, base string) *signedurl.Signer {
	t.Helper()
	s, err := signedurl.New(base, []signedurl.Key{secret})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignedUpload(t *testing.T) {
	e := setup(t, fshttp.SignedOptions{MaxUploadBytes: 16})
	put := filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}
	u := e.sign(t, e.disk, "in/photo.png", put)
	png := "\x89PNG\r\n\x1a\n0000"
	resp, _ := do(t, http.MethodPut, u, strings.NewReader(png), http.Header{"Content-Type": {"text/html"}})
	if resp.StatusCode != 200 || resp.Header.Get("ETag") == "" {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	st, _ := e.disk.Stat(t.Context(), "in/photo.png")
	if st.ContentType != "image/png" {
		t.Errorf("client-declared type trusted: %q", st.ContentType)
	}
	resp, _ = do(t, http.MethodPut, e.sign(t, e.disk, "big", put), strings.NewReader(strings.Repeat("x", 17)), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize PUT = %d", resp.StatusCode)
	}
	if ok, _ := e.disk.Exists(t.Context(), "big"); ok {
		t.Error("oversize PUT stored an object")
	}
	typed := e.sign(t, e.disk, "typed", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "image/png"})
	if resp, _ := do(t, http.MethodPut, typed, strings.NewReader(png), http.Header{"Content-Type": {"image/gif"}}); resp.StatusCode != 403 {
		t.Errorf("content-type mismatch = %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPut, typed, strings.NewReader(png), http.Header{"Content-Type": {"image/png"}}); resp.StatusCode != 200 {
		t.Errorf("matching content type = %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPut, e.sign(t, e.ro, "k", put), strings.NewReader("x"), nil); resp.StatusCode != 403 {
		t.Errorf("PUT on a read-only disk = %d", resp.StatusCode)
	}
	// A GET-signed URL cannot upload.
	if resp, _ := do(t, http.MethodPut, e.sign(t, e.disk, "k", get), strings.NewReader("x"), nil); resp.StatusCode != 403 {
		t.Errorf("PUT with a GET URL = %d", resp.StatusCode)
	}
}

func TestDefaultUploadLimit(t *testing.T) {
	e := setup(t)
	u := e.sign(t, e.disk, "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if resp, _ := do(t, http.MethodPut, u, strings.NewReader("hello"), nil); resp.StatusCode != 200 {
		t.Errorf("PUT under the default limit = %d", resp.StatusCode)
	}
}

// etaglessDriver reports no ETag, to cover the header being omitted.
type etagless struct{ *memory.Driver }

func (e etagless) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	en, err := e.Driver.Stat(ctx, key)
	en.ETag, en.ContentType = "", ""
	return en, err
}

func (e etagless) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	en, err := e.Driver.Write(ctx, key, body, o)
	en.ETag = ""
	return en, err
}

func TestPublic(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("pub", memory.New())
	_, _ = d.WriteBytes(ctx, "logo.svg", []byte("<svg><script>x</script></svg>"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	_, _ = d.WriteBytes(ctx, "secret.txt", []byte("s"), filesystem.WriteOptions{})
	srv := httptest.NewServer(http.StripPrefix("/public", fshttp.Public(d)))
	defer srv.Close()
	resp, body := do(t, http.MethodGet, srv.URL+"/public/logo.svg", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "<svg>") || !strings.Contains(resp.Header.Get(fshttp.HeaderCSP), "sandbox") {
		t.Errorf("public GET = %d %v", resp.StatusCode, resp.Header)
	}
	for path, want := range map[string]int{"/public/secret.txt": 404, "/public/missing": 404, "/public/../x": 400} {
		if resp, _ := do(t, http.MethodGet, srv.URL+path, nil, nil); resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	if resp, _ := do(t, http.MethodPost, srv.URL+"/public/logo.svg", nil, nil); resp.StatusCode != 405 {
		t.Errorf("POST = %d", resp.StatusCode)
	}

	// A disk without per-object visibility follows its default.
	for vis, want := range map[filesystem.Visibility]int{filesystem.VisibilityPublic: 200, filesystem.VisibilityPrivate: 404} {
		plain, _ := filesystem.NewDisk("plain", noVisibility{etagless{memory.New()}}, filesystem.WithDefaultVisibility(vis))
		_, _ = plain.WriteBytes(ctx, "f", []byte("x"), filesystem.WriteOptions{})
		s := httptest.NewServer(fshttp.Public(plain))
		resp, _ := do(t, http.MethodGet, s.URL+"/f", nil, nil)
		if resp.StatusCode != want || (want == 200 && (resp.Header.Get("ETag") != "" || resp.Header.Get("Content-Type") != "application/octet-stream")) {
			t.Errorf("default %s = %d %v", vis, resp.StatusCode, resp.Header)
		}
		s.Close()
	}
}

// noVisibility hides the visibility capability and reports none.
type noVisibility struct{ etagless }

func (n noVisibility) Capabilities() filesystem.Capabilities {
	c := n.etagless.Capabilities()
	c.SetVisibility, c.ContentType = false, false
	return c
}

func (n noVisibility) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := n.etagless.Stat(ctx, key)
	e.Visibility = ""
	return e, err
}

func TestStatusOf(t *testing.T) {
	cases := map[error]int{
		nil:                    200,
		filesystem.ErrNotFound: 404,
		filesystem.ErrExists:   409,
		filesystem.Errorf(filesystem.CodeConflict, "etag changed"): 409,
		filesystem.Errorf(filesystem.CodeNotModified, "unchanged"): 304,
		filesystem.ErrInvalidKey:                                   400,
		filesystem.ErrInvalidArgument:                              400,
		filesystem.ErrUnsupported:                                  501,
		filesystem.ErrReadOnly:                                     403,
		filesystem.ErrPermission:                                   403,
		filesystem.ErrTooLarge:                                     413,
		filesystem.ErrRejected:                                     415,
		filesystem.ErrQuotaExceeded:                                507,
		filesystem.ErrUnavailable:                                  503,
		filesystem.ErrUnknown:                                      500,
		errors.New("anything else"):                                500,
		context.DeadlineExceeded:                                   503,
	}
	for err, want := range cases {
		if got := fshttp.StatusOf(err); got != want {
			t.Errorf("StatusOf(%v) = %d, want %d", err, got, want)
		}
	}
}

// flakyRead fails ranged reads after Stat succeeds, so ServeContent sees a
// read error mid-response.
type flakyRead struct{ *memory.Driver }

func (flakyRead) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return nil, filesystem.ErrUnavailable
}

func TestReadFailureDuringServe(t *testing.T) {
	d, _ := filesystem.NewDisk("d", flakyRead{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	_, _ = d.WriteBytes(t.Context(), "k", []byte("data"), filesystem.WriteOptions{})
	srv := httptest.NewServer(fshttp.Public(d))
	defer srv.Close()
	resp, body := do(t, http.MethodGet, srv.URL+"/k", nil, nil)
	if resp.StatusCode == 200 && body == "data" {
		t.Error("a failing read served the object")
	}
}

// throttledStat fails Stat the way a throttled object store does.
type throttledStat struct{ *memory.Driver }

func (throttledStat) Stat(context.Context, string) (filesystem.Entry, error) {
	return filesystem.Entry{}, filesystem.NewError(filesystem.CodeUnavailable, errors.New("slow down")).WithRetryAfter(1500 * time.Millisecond)
}

func TestRetryAfterReachesTheClient(t *testing.T) {
	d, _ := filesystem.NewDisk("d", throttledStat{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	srv := httptest.NewServer(fshttp.Public(d))
	defer srv.Close()
	resp, _ := do(t, http.MethodGet, srv.URL+"/k", nil, nil)
	// 1.5s rounds up so the client never comes back early.
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(fshttp.HeaderRetryAfter) != "2" {
		t.Errorf("throttled = %d, Retry-After %q", resp.StatusCode, resp.Header.Get(fshttp.HeaderRetryAfter))
	}
	h := http.Header{}
	fshttp.SetRetryAfter(h, filesystem.ErrUnavailable)
	if len(h) != 0 {
		t.Errorf("Retry-After set without a wait: %v", h)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", 0, false},
		{"120", 2 * time.Minute, true},
		{"0", 0, false},
		{"-5", 0, false},
		{"99999999999999", 0, false},
		{"soon", 0, false},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Second).Format(http.TimeFormat), 0, false},
	}
	for _, c := range cases {
		got, ok := fshttp.ParseRetryAfter(c.value, now)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseRetryAfter(%q) = %v %v, want %v %v", c.value, got, ok, c.want, c.ok)
		}
	}
}

func TestSignedUploadStoresPinnedHeaders(t *testing.T) {
	e := setup(t)
	h := filesystem.HTTPHeaders{CacheControl: "public, max-age=60", ContentDisposition: "attachment; filename=photo.png"}
	req, err := e.disk.SignedURL(t.Context(), "in/photo.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	png := "\x89PNG\r\n\x1a\n0000"
	// Missing, changed or extra headers are refused, and nothing is stored.
	for _, sent := range []http.Header{
		{},
		{"Cache-Control": {"no-store"}, "Content-Disposition": {h.ContentDisposition}},
		{"Cache-Control": {h.CacheControl}, "Content-Disposition": {h.ContentDisposition}, "Content-Encoding": {"gzip"}},
	} {
		if resp, _ := do(t, http.MethodPut, req.URL, strings.NewReader(png), sent); resp.StatusCode != http.StatusForbidden {
			t.Errorf("headers %v: PUT = %d", sent, resp.StatusCode)
		}
	}
	if ok, _ := e.disk.Exists(t.Context(), "in/photo.png"); ok {
		t.Fatal("a refused PUT stored the object")
	}
	// The headers the ticket lists are accepted, stored and served.
	if resp, _ := do(t, http.MethodPut, req.URL, strings.NewReader(png), req.Header); resp.StatusCode != 200 {
		t.Fatalf("PUT with the signed headers = %d", resp.StatusCode)
	}
	if st, _ := e.disk.Stat(t.Context(), "in/photo.png"); st.Headers != h {
		t.Errorf("stored %+v", st.Headers)
	}
	// A plain signed GET sends the stored disposition but its own
	// Cache-Control: the stored one is for public serving, and a shared
	// cache must not keep a private download.
	resp, _ := do(t, http.MethodGet, e.sign(t, e.disk, "in/photo.png", get), nil, nil)
	if resp.Header.Get("Cache-Control") != fshttp.DefaultSignedCacheControl || resp.Header.Get("Content-Disposition") != h.ContentDisposition {
		t.Errorf("served %v", resp.Header)
	}
	// With StoredCacheControl, the stored value is served too.
	opted := setup(t, fshttp.SignedOptions{StoredCacheControl: true})
	if _, err := opted.disk.WriteBytes(t.Context(), "in/photo.png", []byte(png), filesystem.WriteOptions{Headers: h}); err != nil {
		t.Fatal(err)
	}
	resp, _ = do(t, http.MethodGet, opted.sign(t, opted.disk, "in/photo.png", get), nil, nil)
	if resp.Header.Get("Cache-Control") != h.CacheControl {
		t.Errorf("with StoredCacheControl, served %v", resp.Header)
	}
	// A URL that pins nothing stores nothing the client sends.
	plain := e.sign(t, e.disk, "in/plain.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if resp, _ := do(t, http.MethodPut, plain, strings.NewReader(png), http.Header{"Cache-Control": {"public, max-age=99999999"}}); resp.StatusCode != 200 {
		t.Fatalf("plain PUT = %d", resp.StatusCode)
	}
	if st, _ := e.disk.Stat(t.Context(), "in/plain.png"); !st.Headers.IsZero() {
		t.Errorf("client headers stored: %+v", st.Headers)
	}
}
