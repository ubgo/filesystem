package gcs_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
	"github.com/ubgo/filesystem/filesystemtest"
)

// proxy sits in front of the fake GCS handler and can answer the next
// request whose method and URL contain a pattern with an error status.
type proxy struct {
	next   http.Handler
	mu     sync.Mutex
	inject []injection
	// urls records "METHOD url" of every request, for tests that check
	// what was sent; bodies records the bodies of PATCH and rewrite requests.
	urls   []string
	bodies []string
	// uploads records the bodies of upload POSTs (the object's metadata
	// JSON, and its bytes on a multipart upload).
	uploads []string
	// headers records the headers of every request, in order with urls.
	headers []http.Header
	// classRewrites makes a rewrite that names a storage class also set
	// that class, which fake-gcs-server ignores on rewrites, so the
	// StorageClass conformance case can see Stat report it.
	classRewrites bool
}

// sent returns the recorded requests containing every one of parts.
func (p *proxy) sent(parts ...string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
outer:
	for _, u := range p.urls {
		for _, part := range parts {
			if !strings.Contains(u, part) {
				continue outer
			}
		}
		out = append(out, u)
	}
	return out
}

type injection struct {
	method, match string
	status        int
	// retryAfter, when set, is sent as the Retry-After header.
	retryAfter string
}

func (p *proxy) fail(method, match string, status int) {
	p.failRetryAfter(method, match, status, "")
}

// failRetryAfter is fail with a Retry-After header on the answer.
func (p *proxy) failRetryAfter(method, match string, status int, retryAfter string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inject = append(p.inject, injection{method: method, match: match, status: status, retryAfter: retryAfter})
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.urls = append(p.urls, r.Method+" "+r.URL.String())
	p.headers = append(p.headers, r.Header.Clone())
	if r.Method == http.MethodPatch || strings.Contains(r.URL.Path, "/rewriteTo/") {
		body, _ := io.ReadAll(r.Body)
		p.bodies = append(p.bodies, string(body))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	if r.Method == http.MethodPost && strings.Contains(r.URL.RawQuery, "uploadType=") {
		body, _ := io.ReadAll(r.Body)
		p.uploads = append(p.uploads, string(body))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	for i, inj := range p.inject {
		if r.Method == inj.method && strings.Contains(r.URL.String(), inj.match) {
			p.inject = append(p.inject[:i], p.inject[i+1:]...)
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if inj.retryAfter != "" {
				w.Header().Set("Retry-After", inj.retryAfter)
			}
			w.WriteHeader(inj.status)
			_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"injected"}}`, inj.status)
			return
		}
	}
	classRewrites := p.classRewrites
	p.mu.Unlock()
	if class := rewriteClass(r); classRewrites && class != "" {
		p.rewriteWithClass(w, r, class)
		return
	}
	p.next.ServeHTTP(w, r)
}

// rewriteClass returns the storage class a rewrite request names, or "".
func rewriteClass(r *http.Request) string {
	if !strings.Contains(r.URL.Path, "/rewriteTo/") {
		return ""
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var meta struct {
		StorageClass string `json:"storageClass"`
	}
	_ = json.Unmarshal(body, &meta)
	return meta.StorageClass
}

// rewriteWithClass runs a rewrite on the fake, then patches the
// destination's class, which a real rewrite sets and the fake does not.
func (p *proxy) rewriteWithClass(w http.ResponseWriter, r *http.Request, class string) {
	rec := httptest.NewRecorder()
	p.next.ServeHTTP(rec, r)
	if rec.Code == http.StatusOK {
		_, dst, _ := strings.Cut(r.URL.EscapedPath(), "/rewriteTo/")
		patch := httptest.NewRequestWithContext(r.Context(), http.MethodPatch, "/storage/v1/"+dst, strings.NewReader(`{"storageClass":"`+class+`"}`))
		patch.Header.Set("Content-Type", "application/json")
		p.next.ServeHTTP(httptest.NewRecorder(), patch)
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

// proxied returns a client talking to a fake GCS through a fault proxy.
func proxied(t *testing.T) (*storage.Client, *fakestorage.Server, *proxy, string) {
	t.Helper()
	fake, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{next: fake.HTTPHandler()}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	client, err := storage.NewClient(t.Context(), option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication(), storage.WithJSONReads())
	if err != nil {
		t.Fatal(err)
	}
	// The SDK retries 429 and 5xx itself, which would consume an injected
	// failure; the driver's own retry belongs to layer.Retry.
	client.SetRetry(storage.WithPolicy(storage.RetryNever))
	return client, fake, p, srv.URL
}

func disk(t *testing.T, cfg gcs.Config) *filesystem.Disk {
	t.Helper()
	drv, err := gcs.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("gcs", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

func TestConformanceACLMode(t *testing.T) {
	client, fake := conformanceFake(t)
	filesystemtest.RunWith(t, func(t *testing.T) *filesystem.Disk {
		return disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Visibility: gcs.VisibilityACL})
	}, conformanceOptions)
}

func TestNewValidation(t *testing.T) {
	srv := fakeServer(t)
	c := srv.Client()
	for _, cfg := range []gcs.Config{
		{Bucket: "b"},
		{Client: c},
		{Client: c, Bucket: "b", Visibility: "public"},
		{Client: c, Bucket: "b", ChunkSize: -1},
		{Client: c, Bucket: "b", Signing: &storage.SignedURLOptions{}},
		{Client: c, Bucket: "b", Signing: &storage.SignedURLOptions{PrivateKey: []byte("k")}},
		{Client: c, Bucket: "b", Signing: &storage.SignedURLOptions{GoogleAccessID: "a", PrivateKey: []byte("k"), SignBytes: func(b []byte) ([]byte, error) { return b, nil }}},
	} {
		if _, err := gcs.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%+v) = %v", cfg, err)
		}
	}
	d := disk(t, gcs.Config{Client: c, Bucket: newBucket(t, srv)})
	if got, ok := filesystem.As[*storage.Client](d); !ok || got != c {
		t.Error("As did not return the client")
	}
	if caps := d.Capabilities(); caps.Presign || caps.SetVisibility || caps.Multipart {
		t.Errorf("default capabilities = %v", caps)
	}
	if len(gcs.VisibilityModeValues) != 2 {
		t.Error("VisibilityModeValues")
	}
}

func pemKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func TestPresign(t *testing.T) {
	ctx := t.Context()
	srv := fakeServer(t)
	d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "b", Signing: &storage.SignedURLOptions{GoogleAccessID: "svc@example.iam.gserviceaccount.com", PrivateKey: pemKey(t)}})
	get, err := d.SignedURL(ctx, "a b.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentType: "application/pdf", ContentDisposition: "attachment"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(get.URL)
	if u.Query().Get("X-Goog-Signature") == "" || u.Query().Get("response-content-disposition") != "attachment" || get.Header != nil {
		t.Errorf("signed GET = %s", get.URL)
	}
	put, err := d.SignedURL(ctx, "up.png", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "image/png"})
	if err != nil || put.Header.Get("Content-Type") != "image/png" {
		t.Errorf("signed PUT = %+v, %v", put, err)
	}
	if _, err := d.SignedURL(ctx, "up.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}); err != nil {
		t.Errorf("signed PUT without a type = %v", err)
	}
	bad := disk(t, gcs.Config{Client: srv.Client(), Bucket: "b", Signing: &storage.SignedURLOptions{GoogleAccessID: "svc", PrivateKey: []byte("not a key")}})
	if _, err := bad.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err == nil {
		t.Error("signing with a broken key succeeded")
	}
}

func TestEveryRequestReportsFailures(t *testing.T) {
	ctx := t.Context()
	steps := []struct {
		name, method, match string
		run                 func(*filesystem.Disk) error
	}{
		{"stat", http.MethodGet, "/o/k", func(d *filesystem.Disk) error { _, err := d.Stat(ctx, "k"); return err }},
		{"read", http.MethodGet, "alt=media", func(d *filesystem.Disk) error { _, err := d.Read(ctx, "k"); return err }},
		{"write", http.MethodPost, "/upload/", func(d *filesystem.Disk) error {
			_, err := d.WriteBytes(ctx, "w", []byte("x"), filesystem.WriteOptions{})
			return err
		}},
		{"delete", http.MethodDelete, "/o/k", func(d *filesystem.Disk) error { return d.Delete(ctx, "k") }},
		{"list", http.MethodGet, "/o?", func(d *filesystem.Disk) error { _, err := d.ListPage(ctx, "", filesystem.ListOptions{}); return err }},
		{"copy", http.MethodPost, "rewriteTo", func(d *filesystem.Disk) error { _, err := d.Copy(ctx, "k", "c"); return err }},
		{"copy acl", http.MethodGet, "/o/k", func(d *filesystem.Disk) error { _, err := d.Copy(ctx, "k", "c2"); return err }},
		{"set public", http.MethodPut, "/acl/", func(d *filesystem.Disk) error { return d.SetVisibility(ctx, "k", filesystem.VisibilityPublic) }},
		{"set private stat", http.MethodGet, "/o/k", func(d *filesystem.Disk) error { return d.SetVisibility(ctx, "k", filesystem.VisibilityPrivate) }},
		{"set private", http.MethodDelete, "/acl/", func(d *filesystem.Disk) error { return d.SetVisibility(ctx, "k", filesystem.VisibilityPrivate) }},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			client, fake, p, _ := proxied(t)
			bucket := newBucket(t, fake)
			d := disk(t, gcs.Config{Client: client, Bucket: bucket, Visibility: gcs.VisibilityACL})
			if _, err := d.WriteBytes(ctx, "k", []byte("data"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			p.fail(s.method, s.match, http.StatusForbidden)
			if s.name == "copy acl" {
				// The copy itself succeeds; the source stat for its ACL fails.
				p.inject = nil
				p.fail(http.MethodGet, "/o/k?", http.StatusForbidden)
			}
			err := s.run(d)
			if filesystem.CodeOf(err) != filesystem.CodePermission {
				t.Errorf("%s: %v", s.name, err)
			}
		})
	}
}

func TestReadPastEndAndStatuses(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake)})
	_, _ = d.WriteBytes(ctx, "k", []byte("abc"), filesystem.WriteOptions{})
	p.fail(http.MethodGet, "alt=media", http.StatusRequestedRangeNotSatisfiable)
	rc, err := d.ReadRange(ctx, "k", filesystem.ByteRange{Offset: 99})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Errorf("read past end = %q", b)
	}
	p.fail(http.MethodGet, "alt=media", http.StatusRequestedRangeNotSatisfiable)
	_, err = d.ReadRange(ctx, "missing", filesystem.ByteRange{Offset: 99})
	wantCode(t, err, filesystem.CodeNotFound)
	for status, want := range map[int]filesystem.Code{
		http.StatusNotFound:           filesystem.CodeNotFound,
		http.StatusPreconditionFailed: filesystem.CodeExists,
		http.StatusUnauthorized:       filesystem.CodePermission,
		http.StatusTooManyRequests:    filesystem.CodeUnavailable,
		http.StatusServiceUnavailable: filesystem.CodeUnavailable,
		http.StatusBadRequest:         filesystem.CodeUnknown,
	} {
		p.fail(http.MethodGet, "/o/k", status)
		_, err := d.Stat(ctx, "k")
		if got := filesystem.CodeOf(err); got != want {
			t.Errorf("status %d => %q, want %q", status, got, want)
		}
	}
	if err := d.Delete(ctx, "never-existed"); err != nil {
		t.Errorf("delete missing = %v", err)
	}
	// A private object that was never public: removing AllUsers is a no-op.
	acl := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), Visibility: gcs.VisibilityACL})
	_, _ = acl.WriteBytes(ctx, "p", []byte("x"), filesystem.WriteOptions{})
	p.fail(http.MethodDelete, "/acl/", http.StatusNotFound)
	if err := acl.SetVisibility(ctx, "p", filesystem.VisibilityPrivate); err != nil {
		t.Errorf("make private an already private object = %v", err)
	}
	wantCode(t, acl.SetVisibility(ctx, "missing", filesystem.VisibilityPrivate), filesystem.CodeNotFound)
}

func TestListingDetails(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	bucket := newBucket(t, fake)
	w := client.Bucket(bucket).Object("dir/").NewWriter(ctx)
	_ = w.Close()
	drv, _ := gcs.New(gcs.Config{Client: client, Bucket: bucket})
	page, err := drv.List(ctx, "dir", filesystem.ListOptions{Recursive: true})
	if err != nil || len(page.Entries) != 0 {
		t.Errorf("directory marker listed: %+v, %v", page.Entries, err)
	}
	// Copy in ACL mode: carrying the visibility over fails.
	d := disk(t, gcs.Config{Client: client, Bucket: bucket, Visibility: gcs.VisibilityACL})
	_, _ = d.WriteBytes(ctx, "src", []byte("x"), filesystem.WriteOptions{})
	p.fail(http.MethodDelete, "/acl/", http.StatusForbidden)
	_, err = d.Copy(ctx, "src", "dst")
	wantCode(t, err, filesystem.CodePermission)
}

func TestOpener(t *testing.T) {
	ctx := t.Context()
	_, fake, _, base := proxied(t)
	bucket := newBucket(t, fake)
	keyFile := filepath.Join(t.TempDir(), "sa.json")
	raw, _ := json.Marshal(map[string]string{"client_email": "svc@x.iam.gserviceaccount.com", "private_key": string(pemKey(t))})
	_ = os.WriteFile(keyFile, raw, 0o600)
	mux := filesystem.NewMux()
	_ = mux.Register(gcs.Scheme, gcs.Opener)
	dsn := "gs://" + bucket + "?endpoint=" + url.QueryEscape(base+"/storage/v1/") + "&no_auth=true&acl=true&signing_key=" + url.QueryEscape(keyFile)
	d, err := mux.Open(ctx, "gcs", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities(); !c.Presign || !c.SetVisibility {
		t.Errorf("capabilities from DSN = %v", c)
	}
	if _, err := d.WriteBytes(ctx, "via-dsn", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("write via DSN = %v", err)
	}
	badKey := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(badKey, []byte(`{"type":"user"}`), 0o600)
	for _, bad := range []string{
		"gs://?no_auth=true",
		"gs://b?nope=1",
		"gs://b?acl=maybe",
		"gs://b?no_auth=true&signing_key=/does/not/exist",
		"gs://b?no_auth=true&signing_key=" + url.QueryEscape(badKey),
	} {
		if _, err := mux.Open(ctx, "gcs", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%q) = %v", bad, err)
		}
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", badKey)
	if _, err := mux.Open(ctx, "gcs", "gs://b"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("unusable default credentials = %v", err)
	}
}

// TestRetryAfter proves a 429 or 503 carrying Retry-After reports the wait
// on the error, and that one without it reports none.
func TestRetryAfter(t *testing.T) {
	for _, c := range []struct {
		status int
		header string
		want   time.Duration
		ok     bool
	}{
		{http.StatusTooManyRequests, "3", 3 * time.Second, true},
		{http.StatusServiceUnavailable, "11", 11 * time.Second, true},
		{http.StatusServiceUnavailable, "", 0, false},
	} {
		client, fake, p, _ := proxied(t)
		d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake)})
		p.failRetryAfter(http.MethodGet, "/o/k", c.status, c.header)
		_, err := d.Stat(t.Context(), "k")
		if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
			t.Fatalf("%d => %v", c.status, err)
		}
		if got, ok := filesystem.RetryAfterOf(err); ok != c.ok || got != c.want {
			t.Errorf("%d Retry-After %q => %v, %t; want %v", c.status, c.header, got, ok, c.want)
		}
	}
}

// TestPresignPinsHeaders proves a presigned PUT signs the stored headers
// it was asked to pin (V4 lists them in X-Goog-SignedHeaders, so GCS
// refuses an upload without them) and hands them back for the client to
// send. fake-gcs-server does not verify signatures, so the refusal itself
// needs a real bucket.
func TestPresignPinsHeaders(t *testing.T) {
	ctx := t.Context()
	srv := fakeServer(t)
	d := disk(t, gcs.Config{Client: srv.Client(), Bucket: "b", Signing: &storage.SignedURLOptions{GoogleAccessID: "svc@example.iam.gserviceaccount.com", PrivateKey: pemKey(t)}})
	if !d.Capabilities().PresignHeaders {
		t.Fatal("PresignHeaders not declared")
	}
	h := filesystem.HTTPHeaders{CacheControl: "no-store", ContentDisposition: "attachment", ContentEncoding: "gzip"}
	put, err := d.SignedURL(ctx, "pinned.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "application/octet-stream", Headers: h})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	signedHeaders := u.Query().Get("X-Goog-SignedHeaders")
	for _, name := range []string{"cache-control", "content-disposition", "content-encoding", "content-type"} {
		if !strings.Contains(signedHeaders, name) {
			t.Errorf("%s not signed: %s", name, signedHeaders)
		}
	}
	if filesystem.HTTPHeadersFrom(put.Header) != h || put.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("returned headers = %v", put.Header)
	}
	plain, err := d.SignedURL(ctx, "plain.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil || plain.Header != nil {
		t.Errorf("PUT without headers = %+v, %v", plain, err)
	}
	if unsigned := disk(t, gcs.Config{Client: srv.Client(), Bucket: "b"}); unsigned.Capabilities().PresignHeaders {
		t.Error("PresignHeaders claimed without Signing")
	}
}
