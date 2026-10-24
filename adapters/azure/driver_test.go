package azure_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

func newDisk(t *testing.T, client *azblob.Client, cred *azblob.SharedKeyCredential) *filesystem.Disk {
	t.Helper()
	drv, err := azure.New(azure.Config{Client: client, Container: newContainer(t, client), SharedKey: cred})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("azure", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func httpDo(t *testing.T, method, u string, body io.Reader, h http.Header) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, u, body)
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSignedURLs(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	d := newDisk(t, client, cred)
	put, err := d.SignedURL(ctx, "up/a b.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if put.Header.Get("x-ms-blob-type") != "BlockBlob" || put.Header.Get("Content-Type") != "text/plain" || !strings.Contains(put.URL, "sig=") {
		t.Errorf("signed PUT = %+v", put)
	}
	if status, body := httpDo(t, http.MethodPut, put.URL, strings.NewReader("via sas"), put.Header); status != http.StatusCreated {
		t.Fatalf("PUT to SAS = %d %s", status, body)
	}
	// Azure does not pin the type, so Stat must report what the client
	// sent: upload.Confirm compares it with the sniffed bytes and refuses
	// a mismatch.
	if e, err := d.Stat(ctx, "up/a b.txt"); err != nil || e.ContentType != "text/plain" {
		t.Errorf("stored type after a signed PUT = %q, %v", e.ContentType, err)
	}
	other, err := d.SignedURL(ctx, "up/lie.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute, ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	lie := other.Header.Clone()
	lie.Set("Content-Type", "text/html")
	if status, body := httpDo(t, http.MethodPut, other.URL, strings.NewReader("<b>x</b>"), lie); status != http.StatusCreated {
		t.Fatalf("PUT with another type = %d %s", status, body)
	}
	if e, err := d.Stat(ctx, "up/lie.txt"); err != nil || e.ContentType != "text/html" {
		t.Errorf("Stat hides the type the client sent: %q, %v", e.ContentType, err)
	}
	get, err := d.SignedURL(ctx, "up/a b.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute, ContentDisposition: "attachment"})
	if err != nil || get.Header != nil {
		t.Fatalf("signed GET = %+v, %v", get, err)
	}
	if status, body := httpDo(t, http.MethodGet, get.URL, nil, nil); status != 200 || body != "via sas" {
		t.Errorf("GET SAS = %d %q", status, body)
	}
	if put2, _ := d.SignedURL(ctx, "x", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}); put2.Header.Get("Content-Type") != "" {
		t.Errorf("untyped PUT header = %v", put2.Header)
	}
}

func TestMultipartEdges(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	d := newDisk(t, client, cred)
	id, err := d.CreateMultipart(ctx, "mp.bin", filesystem.WriteOptions{ContentType: "application/x-test", Metadata: map[string]string{"k": "v"}, IfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	if parts, err := d.ListParts(ctx, "mp.bin", id); err != nil || len(parts) != 0 {
		t.Errorf("parts before any upload = %v, %v", parts, err)
	}
	// A seekable body and a stream.
	p1, err := d.UploadPart(ctx, "mp.bin", id, 1, strings.NewReader("one-"), 4)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := d.UploadPart(ctx, "mp.bin", id, 2, io.MultiReader(strings.NewReader("two")), 3)
	if err != nil {
		t.Fatal(err)
	}
	// Another upload's blocks on the same blob are not listed.
	other, _ := d.CreateMultipart(ctx, "mp.bin", filesystem.WriteOptions{})
	_, _ = d.UploadPart(ctx, "mp.bin", other, 1, strings.NewReader("x"), 1)
	parts, err := d.ListParts(ctx, "mp.bin", id)
	if err != nil || len(parts) != 2 || parts[0].ETag != p1.ETag || parts[1].Size != 3 {
		t.Fatalf("parts = %+v, %v", parts, err)
	}
	for _, bad := range [][]filesystem.Part{{p2, p1}, {{Number: 1, ETag: "wrong"}}} {
		_, err := d.CompleteMultipart(ctx, "mp.bin", id, bad)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	e, err := d.CompleteMultipart(ctx, "mp.bin", id, []filesystem.Part{p1, p2})
	if err != nil || e.Size != 7 || e.ContentType != "application/x-test" || e.Metadata["k"] != "v" {
		t.Fatalf("complete = %+v, %v", e, err)
	}
	// The stored blob keeps CreateMultipart's type, so upload.Confirm
	// (run by the Uppy complete route) does not refuse it.
	if st, err := d.Stat(ctx, "mp.bin"); err != nil || st.ContentType != "application/x-test" {
		t.Fatalf("stored type after complete = %q, %v", st.ContentType, err)
	}
	// The conditional upload completes only once.
	again, _ := d.CreateMultipart(ctx, "mp.bin", filesystem.WriteOptions{IfNotExists: true})
	q, _ := d.UploadPart(ctx, "mp.bin", again, 1, strings.NewReader("z"), 1)
	_, err = d.CompleteMultipart(ctx, "mp.bin", again, []filesystem.Part{q})
	wantCode(t, err, filesystem.CodeExists)

	for _, fn := range []func() error{
		func() error {
			_, err := d.UploadPart(ctx, "mp.bin", "NOT-AN-ID", 1, strings.NewReader("x"), 1)
			return err
		},
		func() error {
			_, err := d.UploadPart(ctx, "other.bin", other, 1, strings.NewReader("x"), 1)
			return err
		},
		func() error { _, err := d.ListParts(ctx, "mp.bin", "zzzzzzzzzzzzzzzzzzzzzzzzzz"); return err },
		func() error { _, err := d.CompleteMultipart(ctx, "mp.bin", id, []filesystem.Part{p1}); return err },
	} {
		wantCode(t, fn(), filesystem.CodeNotFound)
	}
	if err := d.AbortMultipart(ctx, "mp.bin", other); err != nil {
		t.Errorf("abort = %v", err)
	}
	if err := d.AbortMultipart(ctx, "mp.bin", "NOT-AN-ID"); err != nil {
		t.Errorf("abort of a malformed ID = %v", err)
	}
	_, err = d.UploadPart(ctx, "mp.bin", other, 1, failing{}, 1)
	wantCode(t, err, filesystem.CodeNotFound)
	id3, _ := d.CreateMultipart(ctx, "f.bin", filesystem.WriteOptions{})
	if _, err := d.UploadPart(ctx, "f.bin", id3, 1, failing{}, 1); err == nil {
		t.Error("failing part body accepted")
	}
	if _, err := d.UploadPart(ctx, "f.bin", id3, 1, badSeeker{}, 1); err == nil {
		t.Error("failing seek accepted")
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, filesystem.ErrUnavailable }

type badSeeker struct{ failing }

func (badSeeker) Seek(int64, int) (int64, error) { return 0, filesystem.ErrUnavailable }

func TestReservedKeysAndMarkers(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	d := newDisk(t, client, cred)
	_, err := d.WriteBytes(ctx, azure.MultipartDir+"/x", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = d.Stat(ctx, azure.MultipartDir)
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, _ = d.CreateMultipart(ctx, "a.bin", filesystem.WriteOptions{})
	_, _ = d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	for _, recursive := range []bool{true, false} {
		var keys []string
		for e, err := range d.List(ctx, "", filesystem.ListOptions{Recursive: recursive}) {
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, e.Key)
		}
		if strings.Join(keys, ",") != "a.txt" {
			t.Errorf("recursive=%v listed %v", recursive, keys)
		}
	}
	// A directory marker blob is not an object.
	if _, err := client.UploadBuffer(ctx, containerOf(t, d), "dir/", nil, nil); err != nil {
		t.Fatal(err)
	}
	for e, err := range d.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err == nil && e.Key == "dir/" {
			t.Error("directory marker listed")
		}
	}
	rc, err := d.ReadRange(ctx, "a.txt", filesystem.ByteRange{Offset: 50})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Errorf("read past end = %q", b)
	}
	_, err = d.ReadRange(ctx, "missing", filesystem.ByteRange{Offset: 50})
	wantCode(t, err, filesystem.CodeNotFound)
	if page, err := d.ListPage(ctx, "", filesystem.ListOptions{}); err != nil || len(page.Entries) == 0 {
		t.Errorf("default page = %v", err)
	}
}

// containerOf finds the container a disk writes to through the native
// client and a probe object.
func containerOf(t *testing.T, d *filesystem.Disk) string {
	t.Helper()
	req, err := d.SignedURL(t.Context(), "probe", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(req.URL)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	return parts[1]
}

// faultProxy forwards to Azurite and can answer the next matching request
// with an error.
type faultProxy struct {
	rp     *httputil.ReverseProxy
	mu     sync.Mutex
	status int
	method string
	match  string
	// code is the x-ms-error-code of the injected failure; empty means
	// InjectedFailure.
	code string
	// scopes records "METHOD path?query scope" for every request carrying
	// an x-ms-encryption-scope header.
	scopes []string
	// copyStatus, when set, answers HEADs of a path ending in copyPath with
	// that x-ms-copy-status, copyTimes times.
	copyStatus string
	copyPath   string
	copyTimes  int
}

func (p *faultProxy) fakeCopy(path, status string, times int) {
	p.mu.Lock()
	p.copyPath, p.copyStatus, p.copyTimes = path, status, times
	p.mu.Unlock()
}

func (p *faultProxy) fail(method, match string, status int) {
	p.failCode(method, match, status, "")
}

// failCode is fail with a given Azure error code, such as BlobNotFound.
func (p *faultProxy) failCode(method, match string, status int, code string) {
	p.mu.Lock()
	p.method, p.match, p.status, p.code = method, match, status, code
	p.mu.Unlock()
}

func (p *faultProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if scope := r.Header.Get("x-ms-encryption-scope"); scope != "" {
		p.scopes = append(p.scopes, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+scope)
	}
	// A match is a path suffix or a query fragment, never a substring of
	// the whole URL: container names start with "c" and would match "/c".
	hit := p.status != 0 && r.Method == p.method && (strings.HasSuffix(r.URL.Path, p.match) || strings.Contains(r.URL.RawQuery, p.match))
	status, code := p.status, p.code
	if code == "" {
		code = "InjectedFailure"
	}
	if hit {
		p.status = 0
	}
	copyHit := p.copyTimes > 0 && r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, p.copyPath)
	copyStatus := p.copyStatus
	if copyHit {
		p.copyTimes--
	}
	p.mu.Unlock()
	if copyHit {
		w.Header().Set("x-ms-copy-status", copyStatus)
		w.Header().Set("x-ms-copy-status-description", "injected")
		w.WriteHeader(http.StatusOK)
		return
	}
	if hit {
		w.Header().Set("x-ms-error-code", code)
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>injected</Message></Error>`, code)
		return
	}
	p.rp.ServeHTTP(w, r)
}

// proxied returns a disk on Azurite reached through a fault proxy, with SDK
// retries off so one injected failure is one error.
func proxied(t *testing.T) (*filesystem.Disk, *faultProxy) {
	t.Helper()
	client, cred, cs := account(t)
	container := newContainer(t, client)
	var endpoint string
	for _, part := range strings.Split(cs, ";") {
		if k, v, ok := strings.Cut(part, "="); ok && k == "BlobEndpoint" {
			endpoint = v
		}
	}
	target, _ := url.Parse(endpoint)
	p := &faultProxy{rp: httputil.NewSingleHostReverseProxy(&url.URL{Scheme: target.Scheme, Host: target.Host})}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	pc, err := azblob.NewClientWithSharedKeyCredential(srv.URL+target.Path+"/", cred, &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	drv, _ := azure.New(azure.Config{Client: pc, Container: container, SharedKey: cred})
	d, _ := filesystem.NewDisk("azure", drv)
	return d, p
}

func TestEveryRequestReportsFailures(t *testing.T) {
	ctx := t.Context()
	steps := []struct {
		name, method, match string
		status              int
		code                filesystem.Code
		run                 func(*filesystem.Disk) error
	}{
		{"stat", http.MethodHead, "/k", 403, filesystem.CodePermission, func(d *filesystem.Disk) error { _, err := d.Stat(ctx, "k"); return err }},
		{"read", http.MethodGet, "/k", 403, filesystem.CodePermission, func(d *filesystem.Disk) error { _, err := d.Read(ctx, "k"); return err }},
		{"write", http.MethodPut, "/w", 503, filesystem.CodeUnavailable, func(d *filesystem.Disk) error {
			_, err := d.WriteBytes(ctx, "w", []byte("x"), filesystem.WriteOptions{})
			return err
		}},
		{"delete", http.MethodDelete, "/k", 429, filesystem.CodeUnavailable, func(d *filesystem.Disk) error { return d.Delete(ctx, "k") }},
		{"list flat", http.MethodGet, "comp=list", 403, filesystem.CodePermission, func(d *filesystem.Disk) error {
			_, err := d.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
			return err
		}},
		{"list tree", http.MethodGet, "comp=list", 403, filesystem.CodePermission, func(d *filesystem.Disk) error { _, err := d.ListPage(ctx, "", filesystem.ListOptions{}); return err }},
		{"copy start", http.MethodPut, "/c", 409, filesystem.CodeExists, func(d *filesystem.Disk) error { _, err := d.Copy(ctx, "k", "c"); return err }},
		{"copy poll", http.MethodHead, "/c", 400, filesystem.CodeUnknown, func(d *filesystem.Disk) error { _, err := d.Copy(ctx, "k", "c"); return err }},
		{"not found without a code", http.MethodHead, "/k", 404, filesystem.CodeNotFound, func(d *filesystem.Disk) error { _, err := d.Stat(ctx, "k"); return err }},
		{"create multipart", http.MethodPut, ".json", 403, filesystem.CodePermission, func(d *filesystem.Disk) error {
			_, err := d.CreateMultipart(ctx, "m", filesystem.WriteOptions{})
			return err
		}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			d, p := proxied(t)
			if _, err := d.WriteBytes(ctx, "k", []byte("data"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			p.fail(s.method, s.match, s.status)
			if err := s.run(d); filesystem.CodeOf(err) != s.code {
				t.Errorf("%s: %v, want %s", s.name, err, s.code)
			}
		})
	}
}

func TestMultipartRequestFailures(t *testing.T) {
	ctx := t.Context()
	d, p := proxied(t)
	id, err := d.CreateMultipart(ctx, "m", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.fail(http.MethodGet, ".json", 403)
	_, err = d.ListParts(ctx, "m", id)
	wantCode(t, err, filesystem.CodePermission)
	p.fail(http.MethodPut, "comp=block", 403)
	_, err = d.UploadPart(ctx, "m", id, 1, strings.NewReader("x"), 1)
	wantCode(t, err, filesystem.CodePermission)
	part, _ := d.UploadPart(ctx, "m", id, 1, strings.NewReader("x"), 1)
	p.fail(http.MethodGet, "comp=blocklist", 403)
	_, err = d.ListParts(ctx, "m", id)
	wantCode(t, err, filesystem.CodePermission)
	p.fail(http.MethodPut, "comp=blocklist", 403)
	_, err = d.CompleteMultipart(ctx, "m", id, []filesystem.Part{part})
	wantCode(t, err, filesystem.CodePermission)
	// A marker that is not valid JSON.
	if _, err := d.WriteBytes(ctx, "plain", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	native, _ := filesystem.As[*azblob.Client](d)
	cont := containerOf(t, d)
	if _, err := native.UploadBuffer(ctx, cont, azure.MultipartDir+"/"+id+".json", []byte("{broken"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListParts(ctx, "m", id); err == nil {
		t.Error("corrupt marker accepted")
	}
}

func TestCopyPolling(t *testing.T) {
	ctx := t.Context()
	d, p := proxied(t)
	_, _ = d.WriteBytes(ctx, "k", []byte("data"), filesystem.WriteOptions{})
	p.fakeCopy("/dst", "pending", 2)
	if _, err := d.Copy(ctx, "k", "dst"); err != nil {
		t.Errorf("copy after two pending polls = %v", err)
	}
	p.fakeCopy("/dst2", "failed", 1)
	_, err := d.Copy(ctx, "k", "dst2")
	if filesystem.CodeOf(err) != filesystem.CodeUnknown || !strings.Contains(err.Error(), "copy failed: injected") {
		t.Errorf("failed copy = %v", err)
	}
	p.fakeCopy("/dst3", "pending", 1000)
	short, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer cancel()
	_, err = d.Copy(short, "k", "dst3")
	wantCode(t, err, filesystem.CodeUnavailable)
}

func TestDriverDirectly(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	drv, _ := azure.New(azure.Config{Client: client, Container: newContainer(t, client), SharedKey: cred})
	if _, err := drv.List(ctx, "", filesystem.ListOptions{}); err != nil {
		t.Errorf("List with no limit = %v", err)
	}
	if _, err := drv.CreateMultipart(ctx, azure.MultipartDir+"/x", filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("reserved multipart key = %v", err)
	}
	id, _ := drv.CreateMultipart(ctx, "m", filesystem.WriteOptions{})
	if _, err := drv.UploadPart(ctx, "m", id, 1, badSeeker{}, 1); err == nil {
		t.Error("failing seek accepted")
	}
	// A part never uploaded, with the ETag the driver would give it.
	ghost := filesystem.Part{Number: 9, ETag: base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s-%05d", id, 9))}
	_, err := drv.CompleteMultipart(ctx, "m", id, []filesystem.Part{ghost})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

func TestOpener(t *testing.T) {
	ctx := t.Context()
	_, _, cs := account(t)
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.Opener)
	client, _, _ := account(t)
	cont := newContainer(t, client)
	d, err := mux.Open(ctx, "az", "azblob://"+cont)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "via-dsn", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("write via DSN = %v", err)
	}
	if !d.Capabilities().Presign {
		t.Error("connection string did not enable signing")
	}
	t.Setenv("MY_AZ", cs)
	if _, err := mux.Open(ctx, "az", "azblob://"+cont+"?connection_env=MY_AZ"); err != nil {
		t.Errorf("custom env = %v", err)
	}
	t.Setenv("NO_ENDPOINT", "AccountName=acct;AccountKey="+strings.Repeat("A", 88))
	if _, err := mux.Open(ctx, "az", "azblob://c?connection_env=NO_ENDPOINT"); err != nil {
		t.Errorf("derived endpoint = %v", err)
	}
	t.Setenv("BAD_KEY", "AccountName=acct;AccountKey=!!!")
	t.Setenv("EMPTY", "")
	for _, bad := range []string{"azblob://", "azblob://c?nope=1", "azblob://c?connection_env=EMPTY", "azblob://c?connection_env=BAD_KEY"} {
		if _, err := mux.Open(ctx, "az", bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%q) = %v", bad, err)
		}
	}
}

// TestSweepAbandoned proves a sweep deletes the markers of uploads started
// before the cutoff, so those uploads are gone, and leaves newer uploads,
// stored blobs and foreign blobs under the prefix alone.
func TestSweepAbandoned(t *testing.T) {
	ctx := t.Context()
	client, cred, _ := account(t)
	d := newDisk(t, client, cred)
	if !d.Capabilities().SweepAbandoned {
		t.Fatal("SweepAbandoned not declared")
	}
	if _, err := d.WriteBytes(ctx, "kept.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	old, err := d.CreateMultipart(ctx, "old.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := d.UploadPart(ctx, "old.bin", old, 1, strings.NewReader("a"), 1)
	if err != nil {
		t.Fatal(err)
	}
	// A blob under the prefix that is not one of our markers.
	ctr := client.ServiceClient().NewContainerClient(containerOf(t, d))
	foreign := azure.MultipartDir + "/notes.txt"
	if _, err := client.UploadBuffer(ctx, containerOf(t, d), foreign, []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	// An abandoned upload onto a stored blob: its blocks must stay (a
	// commit would rewrite the blob), and so must the blob.
	onKept, _ := d.CreateMultipart(ctx, "kept.txt", filesystem.WriteOptions{})
	if _, err := d.UploadPart(ctx, "kept.txt", onKept, 1, strings.NewReader("z"), 1); err != nil {
		t.Fatal(err)
	}
	// An abandoned upload sharing its blob with someone else's staged
	// block: the commit would destroy that block, so nothing is discarded.
	shared, _ := d.CreateMultipart(ctx, "shared.bin", filesystem.WriteOptions{})
	if _, err := d.UploadPart(ctx, "shared.bin", shared, 1, strings.NewReader("s"), 1); err != nil {
		t.Fatal(err)
	}
	// Every block ID of a blob must have the same length as ours.
	otherBlock := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", len(shared)) + "-00001"))
	if _, err := ctr.NewBlockBlobClient("shared.bin").StageBlock(ctx, otherBlock, streaming.NopCloser(strings.NewReader("o")), nil); err != nil {
		t.Fatal(err)
	}
	uncommitted := func(key string) int {
		t.Helper()
		bl, err := ctr.NewBlockBlobClient(key).GetBlockList(ctx, blockblob.BlockListTypeUncommitted, nil)
		if err != nil {
			return 0
		}
		return len(bl.UncommittedBlocks)
	}
	if n, err := d.SweepAbandoned(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("sweep with an old cutoff = %d, %v", n, err)
	}
	cutoff := time.Now().Add(time.Minute)
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != 3 {
		t.Fatalf("sweep = %d, %v; want 3", n, err)
	}
	if ok, _ := d.Exists(ctx, "old.bin"); ok || uncommitted("old.bin") != 0 {
		t.Error("the abandoned upload's blocks or an empty blob were left behind")
	}
	if uncommitted("kept.txt") != 1 {
		t.Error("blocks staged on a stored blob were discarded")
	}
	if b, err := d.ReadAll(ctx, "kept.txt"); err != nil || string(b) != "x" {
		t.Errorf("stored blob after sweep = %q, %v", b, err)
	}
	if uncommitted("shared.bin") != 2 {
		t.Error("a blob with another writer's blocks was committed")
	}
	_, err = d.UploadPart(ctx, "old.bin", old, 2, strings.NewReader("b"), 1)
	wantCode(t, err, filesystem.CodeNotFound)
	if _, err := d.CompleteMultipart(ctx, "old.bin", old, []filesystem.Part{part}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("complete after sweep = %v", err)
	}
	if ok, _ := d.Exists(ctx, "kept.txt"); !ok {
		t.Error("sweep removed a stored blob")
	}
	if _, err := ctr.NewBlobClient(foreign).GetProperties(ctx, nil); err != nil {
		t.Errorf("sweep removed a foreign blob: %v", err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("second sweep = %d, %v", n, err)
	}
}

// TestSweepRecoversLeftover proves that when the sweep commits the empty
// blob that discards an upload's blocks and then fails to delete it, the
// marker is kept and the next sweep deletes that blob, while a user's own
// empty blob at another abandoned upload's key is never taken for one.
func TestSweepRecoversLeftover(t *testing.T) {
	ctx := t.Context()
	d, p := proxied(t)
	id, err := d.CreateMultipart(ctx, "gone.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UploadPart(ctx, "gone.bin", id, 1, strings.NewReader("a"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "empty.bin", nil, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateMultipart(ctx, "empty.bin", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(time.Minute)

	p.fail(http.MethodDelete, "/gone.bin", http.StatusInternalServerError)
	first, err := d.SweepAbandoned(ctx, cutoff)
	if err == nil {
		t.Fatal("sweep whose delete failed reported success")
	}
	// The window the docs describe: the empty blob is visible until deleted.
	if e, err := d.Stat(ctx, "gone.bin"); err != nil || e.Size != 0 {
		t.Fatalf("leftover after the failed delete = %+v, %v", e, err)
	}
	if _, err := d.ListParts(ctx, "gone.bin", id); err != nil {
		t.Fatalf("marker lost after the failed delete: %v", err)
	}
	// The markers are swept in ID order, so the user's empty blob's marker
	// may go in either run; between them, both uploads are removed.
	n, err := d.SweepAbandoned(ctx, cutoff)
	if err != nil || first+n != 2 {
		t.Fatalf("sweeps removed %d then %d (%v); want 2 in all", first, n, err)
	}
	if ok, _ := d.Exists(ctx, "gone.bin"); ok {
		t.Error("the leftover empty blob survived the rerun")
	}
	if ok, err := d.Exists(ctx, "empty.bin"); !ok || err != nil {
		t.Errorf("the user's empty blob was deleted: %v", err)
	}
}

// TestSweepRequestFailures injects a failure into each request a sweep
// makes and requires it to surface (or, where the sweep is designed to
// carry on, to be skipped), with the marker kept whenever the upload's
// blocks were not dealt with.
func TestSweepRequestFailures(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name          string
		method, match string
		status        int
		code          string
		wantErr       bool
		wantN         int
	}{
		{"list markers", http.MethodGet, "comp=list", http.StatusInternalServerError, "", true, 0},
		{"read marker", http.MethodGet, ".json", http.StatusInternalServerError, "", false, 1},
		{"block list", http.MethodGet, "comp=blocklist", http.StatusInternalServerError, "", true, 0},
		{"blob gone before the block list", http.MethodGet, "comp=blocklist", http.StatusNotFound, "BlobNotFound", false, 1},
		{"blob created before the commit", http.MethodPut, "comp=blocklist", http.StatusConflict, "BlobAlreadyExists", false, 1},
		{"commit", http.MethodPut, "comp=blocklist", http.StatusInternalServerError, "", true, 0},
		{"delete marker", http.MethodDelete, ".json", http.StatusInternalServerError, "", true, 0},
		{"marker already gone", http.MethodDelete, ".json", http.StatusNotFound, "BlobNotFound", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, p := proxied(t)
			id, err := d.CreateMultipart(ctx, "x.bin", filesystem.WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.UploadPart(ctx, "x.bin", id, 1, strings.NewReader("a"), 1); err != nil {
				t.Fatal(err)
			}
			p.failCode(c.method, c.match, c.status, c.code)
			n, err := d.SweepAbandoned(ctx, cutoff)
			if (err != nil) != c.wantErr || n != c.wantN {
				t.Errorf("sweep = %d, %v; want %d, error %t", n, err, c.wantN, c.wantErr)
			}
		})
	}
}

// TestSweepLeftoverFailures covers the rerun that finds a leftover empty
// blob: reading its properties fails (reported), or the blob is already
// gone (the marker is then removed).
func TestSweepLeftoverFailures(t *testing.T) {
	ctx := t.Context()
	cutoff := time.Now().Add(time.Minute)
	for _, c := range []struct {
		name    string
		status  int
		code    string
		wantErr bool
	}{
		{"properties fail", http.StatusInternalServerError, "", true},
		{"leftover gone", http.StatusNotFound, "BlobNotFound", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, p := proxied(t)
			id, err := d.CreateMultipart(ctx, "gone.bin", filesystem.WriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.UploadPart(ctx, "gone.bin", id, 1, strings.NewReader("a"), 1); err != nil {
				t.Fatal(err)
			}
			p.fail(http.MethodDelete, "/gone.bin", http.StatusInternalServerError)
			if _, err := d.SweepAbandoned(ctx, cutoff); err == nil {
				t.Fatal("first sweep did not fail")
			}
			p.failCode(http.MethodHead, "/gone.bin", c.status, c.code)
			if _, err := d.SweepAbandoned(ctx, cutoff); (err != nil) != c.wantErr {
				t.Errorf("rerun = %v, want error %t", err, c.wantErr)
			}
		})
	}
}

// TestSweepKeepsBlocksOnCommittedBlob proves an abandoned upload onto a
// blob made of committed blocks (a completed multipart upload) keeps its
// staged blocks, since committing would rewrite the blob.
func TestSweepKeepsBlocksOnCommittedBlob(t *testing.T) {
	ctx := t.Context()
	d, _ := proxied(t)
	done, _ := d.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{})
	part, err := d.UploadPart(ctx, "big.bin", done, 1, strings.NewReader("stored"), 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteMultipart(ctx, "big.bin", done, []filesystem.Part{part}); err != nil {
		t.Fatal(err)
	}
	abandoned, _ := d.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{})
	if _, err := d.UploadPart(ctx, "big.bin", abandoned, 1, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if n, err := d.SweepAbandoned(ctx, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if b, err := d.ReadAll(ctx, "big.bin"); err != nil || string(b) != "stored" {
		t.Errorf("stored blob after sweep = %q, %v", b, err)
	}
}
