// Package fwtest is the shared fake application of the framework adapter
// tests (contrib/gin, contrib/echo, contrib/fiber, contrib/chi). It wires
// the storage side once (memory disks, a signer, a records store on
// in-memory SQLite and the library's handlers) and drives every route of
// a server over real HTTP, so each adapter's test is only its routing and
// proves that its Mount, Actor, Scope, ScopeOf, Error and Zip hand the
// library the requests and contexts it expects.
//
// It is a module of its own, imported only by tests, so the adapters'
// production code never requires SQLite.
package fwtest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/sqlb"
	_ "modernc.org/sqlite" // the records tables live in in-memory SQLite

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordshttp"
	"github.com/ubgo/filesystem/signedurl"
	"github.com/ubgo/filesystem/upload"
)

// The routes every adapter's test app registers. PathUpload is an exact
// route on the root; everything else lives in the Group route group, so a
// Mount that forgot the group's base path fails.
const (
	PathUpload = "/upload" // POST, upload.Handler
	Group      = "/api"    // the route group
	// Mounted with the adapter's Mount inside Group.
	MountFiles  = "/files"  // fshttp.Signed
	MountPublic = "/public" // fshttp.Public
	MountMedia  = "/media"  // recordshttp, behind Actor and Scope
	MountSlash  = "/raw/"   // Echo, a prefix with a trailing slash
	MountPlain  = "/path"   // Echo, a prefix without one
	// Exact routes inside Group, served by the app's own handlers.
	PathWhoami    = "/whoami"     // GET, behind Actor and Scope: writes Whoami
	PathZip       = "/zip"        // GET, Zip of ZipFiles on Media
	PathZipBroken = "/zip-broken" // GET, Zip of ZipFiles on Broken
	PathZipEarly  = "/zip-early"  // GET, Zip of EarlyZipFiles on Broken
	PathError     = "/error"      // GET ?case=<ErrorCase>, the adapter's Error
)

// HeaderUser names the signed-in user; it stands in for authentication.
const HeaderUser = "X-User"

// Tenant is the scope of every signed-in request.
const Tenant = "acme"

// Disk names; signed URLs carry them.
const (
	DiskMedia  = "media"
	DiskPublic = "public"
	DiskBroken = "broken"
)

// TextBody is a file every route accepts.
const TextBody = "quarterly numbers\n"

// htmlBody is refused by the policy.
const htmlBody = "<!doctype html><script>alert(1)</script>"

// ownerUser is the only owner type the records store accepts.
const ownerUser = "user"

// ErrorCase names one error the PathError route answers with.
type ErrorCase string

// The closed set of error cases.
const (
	// CaseDriver is an unavailable backend whose message names a bucket
	// and a key: the client must see only the status text.
	CaseDriver ErrorCase = "driver"
	// CaseRejected is the client's own input refused: its words reach it.
	CaseRejected ErrorCase = "rejected"
	// CaseTooLarge is http.MaxBytesReader's cap: 413 too_large.
	CaseTooLarge ErrorCase = "too-large"
	// CaseRetry is a backend asking for a wait: 503 with Retry-After.
	CaseRetry ErrorCase = "retry"
)

// ErrorCaseValues lists every ErrorCase.
var ErrorCaseValues = []ErrorCase{CaseDriver, CaseRejected, CaseTooLarge, CaseRetry}

// Values the error cases carry, checked by Exercise.
const (
	secretBucket   = "secret-bucket"
	rejectedReason = "type text/html is not allowed"
	retryAfter     = 7 * time.Second
	retryHeader    = "7"
)

// Err returns the error of case c, or nil for an unknown case.
func Err(c ErrorCase) error {
	switch c {
	case CaseDriver:
		return filesystem.NewError(filesystem.CodeUnavailable, errors.New("s3: "+secretBucket+"/tenants/acme/payroll.pdf: connection refused"))
	case CaseRejected:
		return filesystem.NewError(filesystem.CodeRejected, errors.New(rejectedReason))
	case CaseTooLarge:
		return &http.MaxBytesError{Limit: 1}
	case CaseRetry:
		return filesystem.NewError(filesystem.CodeUnavailable, errors.New("slow down")).WithRetryAfter(retryAfter)
	}
	return nil
}

// signingKeyID names the signer's only key.
const signingKeyID = "k1"

// brokenAfter is how many bytes the Broken disk serves before failing a
// read, and bigSize the size of the object it fails on: far more than any
// buffer, so the zip's headers are sent before the failure.
const (
	brokenAfter = 64 << 10
	bigSize     = 1 << 20
)

// zipKeys are the objects ZipFiles names on Media and Broken.
const (
	zipSmall = "docs/a.txt"
	zipBig   = "docs/big.bin"
	// zipEarly fails on its first byte, after the headers went out but
	// before any body.
	zipEarly = "docs/early.bin"
)

// App is the wired storage side of the fake application.
type App struct {
	Media  *filesystem.Disk // private files behind signed links
	Public *filesystem.Disk // public files by key
	Broken *filesystem.Disk // serves zipBig only partly, then fails
	Store  *records.Store
	// The library's handlers, each a plain http.Handler.
	Upload  http.Handler
	Files   http.Handler
	PublicH http.Handler
	Records http.Handler
	// Echo answers with the path it received, so Exercise checks what
	// Mount stripped.
	Echo http.Handler
}

// NewApp wires the app for a server reached at base. scope is the
// adapter's ScopeOf, given to recordshttp.Config.Scope; Authorize lets a
// user act only on their own owner routes and files, comparing with the
// actor the adapter's Actor middleware recorded.
func NewApp(t *testing.T, base string, scope func(*http.Request) (string, error)) *App {
	t.Helper()
	ctx := t.Context()
	secret := bytes.Repeat([]byte{7}, signedurl.MinKeyBytes)
	signer, err := signedurl.New(base+Group+MountFiles, []signedurl.Key{{ID: signingKeyID, Secret: secret}})
	must(t, err)
	media, err := filesystem.NewDisk(DiskMedia, memory.New(), filesystem.WithURLSigner(signer))
	must(t, err)
	public, err := filesystem.NewDisk(DiskPublic, memory.New(), filesystem.WithPublicURL(base+Group+MountPublic), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	must(t, err)
	broken, err := filesystem.NewDisk(DiskBroken, brokenDriver{memory.New()})
	must(t, err)
	manager, err := filesystem.NewManager(media, public, broken)
	must(t, err)
	db, err := sql.Open("sqlite", ":memory:")
	must(t, err)
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	t.Cleanup(func() { _ = db.Close() })
	store, err := records.New(db, sqlb.SQLiteDialect{}, manager, records.WithOwnerTypes(ownerUser))
	must(t, err)
	_, err = store.Migrate(ctx)
	must(t, err)
	policy := upload.Policy{MaxSize: bigSize, Allowed: []string{"text/plain", "application/octet-stream"}, Key: upload.RandomKey("incoming")}
	rec, err := recordshttp.Handler(recordshttp.Config{
		Store: store, Disk: DiskMedia, Policy: policy, Scope: scope,
		Authorize: func(r *http.Request, _ recordshttp.Action, tg recordshttp.Target) error {
			return authorize(r, tg)
		},
	})
	must(t, err)
	for _, d := range []*filesystem.Disk{media, broken} {
		_, err = d.WriteBytes(ctx, zipSmall, []byte(TextBody), filesystem.WriteOptions{})
		must(t, err)
		_, err = d.WriteBytes(ctx, zipBig, bytes.Repeat([]byte{'z'}, bigSize), filesystem.WriteOptions{})
		must(t, err)
		_, err = d.WriteBytes(ctx, zipEarly, []byte(TextBody), filesystem.WriteOptions{})
		must(t, err)
	}
	return &App{
		Media: media, Public: public, Broken: broken, Store: store,
		Upload:  upload.Handler(media, policy, upload.HandlerOptions{}),
		Files:   fshttp.Signed(manager, signer),
		PublicH: fshttp.Public(public),
		Records: rec,
		Echo: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.Method+" "+r.URL.Path)
		}),
	}
}

// errNotYours refuses a user acting on someone else's owner or file.
var errNotYours = errors.New("not yours")

// authorize allows the actor's own owner routes and files.
func authorize(r *http.Request, tg recordshttp.Target) error {
	actor := records.ActorFrom(r.Context())
	if tg.File != nil {
		if tg.File.UploadedBy == actor {
			return nil
		}
		return errNotYours
	}
	if tg.Owner.Type == ownerUser && tg.Owner.ID == actor {
		return nil
	}
	return errNotYours
}

// User is the actor of a request, for the adapter's Actor middleware:
// the X-User header, "" for an anonymous request.
func User(r *http.Request) (string, error) { return r.Header.Get(HeaderUser), nil }

// errSignedOut refuses a request without a user a scope.
var errSignedOut = errors.New("sign in first")

// ScopeOfUser is the tenant of a request, for the adapter's Scope
// middleware: Tenant for a signed-in request, an error otherwise.
func ScopeOfUser(r *http.Request) (string, error) {
	if r.Header.Get(HeaderUser) == "" {
		return "", errSignedOut
	}
	return Tenant, nil
}

// Whoami is the body PathWhoami answers with: the actor the request's
// context carries and the scope the adapter's ScopeOf found.
func Whoami(ctx context.Context, scope string) string {
	return "actor=" + records.ActorFrom(ctx) + " scope=" + scope
}

// ZipFiles are the objects the zip routes archive.
func ZipFiles() []fshttp.ZipFile {
	return []fshttp.ZipFile{{Key: zipSmall, Name: "a.txt"}, {Key: zipBig, Name: "big.bin"}}
}

// EarlyZipFiles is an archive whose first object fails on its first byte.
func EarlyZipFiles() []fshttp.ZipFile {
	return []fshttp.ZipFile{{Key: zipEarly, Name: "early.bin"}}
}

// ZipName is the archive's file name.
const ZipName = "docs.zip"

// brokenDriver is a memory driver whose reads of zipBig fail after
// brokenAfter bytes and of zipEarly at once, as a backend that drops the
// connection mid-stream.
type brokenDriver struct{ *memory.Driver }

// errDropped is the mid-stream failure.
var errDropped = errors.New("connection dropped")

func (d brokenDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := d.Driver.Read(ctx, key, rng)
	switch {
	case err != nil:
		return rc, err
	case key == zipBig:
		return failingReader{Reader: io.LimitReader(rc, brokenAfter), Closer: rc}, nil
	case key == zipEarly:
		return failingReader{Reader: io.LimitReader(rc, 0), Closer: rc}, nil
	}
	return rc, nil
}

// failingReader returns errDropped where its limited reader ends.
type failingReader struct {
	io.Reader
	io.Closer
}

func (f failingReader) Read(p []byte) (int, error) {
	n, err := f.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errDropped
	}
	return n, err
}

// Listen opens a listener on a free local port and returns it with the
// base URL clients reach it at.
func Listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	return ln, "http://" + ln.Addr().String()
}

// Serve serves h on ln until the test ends.
func Serve(t *testing.T, ln net.Listener, h http.Handler) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// Exercise drives every route of the fake application through the server
// at base.
func Exercise(t *testing.T, base string, a *App) {
	t.Run("upload", func(t *testing.T) { formUpload(t, base) })
	t.Run("mount strips the group and the prefix", func(t *testing.T) { mountPaths(t, base) })
	t.Run("records with actor and scope", func(t *testing.T) { recordsFlow(t, base) })
	t.Run("public", func(t *testing.T) { publicFile(t, a, base) })
	t.Run("whoami", func(t *testing.T) { whoami(t, base) })
	t.Run("errors", func(t *testing.T) { errorCases(t, base) })
	t.Run("zip", func(t *testing.T) { zipDownload(t, base) })
	t.Run("zip failing mid-stream", func(t *testing.T) { zipBroken(t, base+Group+PathZipBroken) })
	t.Run("zip failing before its body", func(t *testing.T) { zipBroken(t, base+Group+PathZipEarly) })
}

func formUpload(t *testing.T, base string) {
	body, ct := Form(t, "report.txt", TextBody)
	status, out, _ := Do(t, http.MethodPost, base+PathUpload, Headers("Content-Type", ct), body)
	if status != http.StatusCreated || !strings.Contains(out, `"filename":"report.txt"`) {
		t.Fatalf("upload = %d %s", status, out)
	}
	body, ct = Form(t, "page.html", htmlBody)
	if status, out, _ := Do(t, http.MethodPost, base+PathUpload, Headers("Content-Type", ct), body); status != http.StatusUnsupportedMediaType || !strings.Contains(out, `"code":"rejected"`) {
		t.Errorf("html upload = %d %s", status, out)
	}
}

func mountPaths(t *testing.T, base string) {
	for path, want := range map[string]string{
		Group + MountSlash + "abc":     "PATCH abc",
		Group + MountSlash:             "PATCH ",
		Group + MountPlain + "/a/b%20": "PATCH /a/b ",
		Group + MountPlain + "/":       "PATCH /",
	} {
		if status, got, _ := Do(t, http.MethodPatch, base+path, nil, nil); status != http.StatusOK || got != want {
			t.Errorf("%s = %d %q, want %q", path, status, got, want)
		}
	}
}

func recordsFlow(t *testing.T, base string) {
	owner := base + Group + MountMedia + "/owners/user/alice/docs"
	for user, want := range map[string]int{"": http.StatusUnauthorized, "bob": http.StatusForbidden} {
		body, ct := Form(t, "notes.txt", TextBody)
		status, out, _ := Do(t, http.MethodPost, owner, Headers("Content-Type", ct, HeaderUser, user), body)
		if status != want || !strings.Contains(out, `"code":"permission"`) {
			t.Errorf("user %q = %d %s, want %d", user, status, out, want)
		}
	}
	body, ct := Form(t, "notes.txt", TextBody)
	status, out, _ := Do(t, http.MethodPost, owner, Headers("Content-Type", ct, HeaderUser, "alice"), body)
	var attached struct {
		Files []struct {
			FileID     string `json:"file_id"`
			UploadedBy string `json:"uploaded_by"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &attached); err != nil || status != http.StatusCreated || len(attached.Files) != 1 {
		t.Fatalf("attach = %d %s", status, out)
	}
	if attached.Files[0].UploadedBy != "alice" {
		t.Errorf("uploaded_by = %q, want alice: the actor did not reach the store", attached.Files[0].UploadedBy)
	}
	status, _, resp := Do(t, http.MethodGet, base+Group+MountMedia+"/files/"+attached.Files[0].FileID, Headers(HeaderUser, "alice"), nil)
	if status != http.StatusFound {
		t.Fatalf("read = %d", status)
	}
	if status, got, _ := Do(t, http.MethodGet, resp.Header.Get("Location"), nil, nil); status != http.StatusOK || got != TextBody {
		t.Errorf("signed link = %d %q", status, got)
	}
}

func publicFile(t *testing.T, a *App, base string) {
	ctx := t.Context()
	if _, err := a.Public.WriteBytes(ctx, "logos/app.txt", []byte(TextBody), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	link, err := a.Public.PublicURL(ctx, "logos/app.txt")
	if err != nil || link != base+Group+MountPublic+"/logos/app.txt" {
		t.Fatalf("public URL = %q, %v", link, err)
	}
	if status, got, _ := Do(t, http.MethodGet, link, nil, nil); status != http.StatusOK || got != TextBody {
		t.Errorf("public file = %d %q", status, got)
	}
}

func whoami(t *testing.T, base string) {
	if status, got, _ := Do(t, http.MethodGet, base+Group+PathWhoami, Headers(HeaderUser, "alice"), nil); status != http.StatusOK || got != "actor=alice scope="+Tenant {
		t.Errorf("whoami = %d %q", status, got)
	}
	if status, got, _ := Do(t, http.MethodGet, base+Group+PathWhoami, nil, nil); status != http.StatusUnauthorized || !strings.Contains(got, `"code":"permission"`) {
		t.Errorf("anonymous whoami = %d %q, want 401", status, got)
	}
}

func errorCases(t *testing.T, base string) {
	for _, c := range ErrorCaseValues {
		status, out, resp := Do(t, http.MethodGet, base+Group+PathError+"?case="+url.QueryEscape(string(c)), nil, nil)
		var body upload.ErrorResponse
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatalf("%s: body %q: %v", c, out, err)
		}
		want := wantError(c)
		if status != want.status || body.Code != want.code || body.Error != want.message || resp.Header.Get(fshttp.HeaderRetryAfter) != want.retry {
			t.Errorf("%s = %d %+v Retry-After %q, want %+v", c, status, body, resp.Header.Get(fshttp.HeaderRetryAfter), want)
		}
		if strings.Contains(out, secretBucket) {
			t.Errorf("%s leaks the backend's message: %s", c, out)
		}
	}
}

// wantedError is what a client must see for one error case.
type wantedError struct {
	status  int
	code    filesystem.Code
	message string
	retry   string
}

func wantError(c ErrorCase) wantedError {
	switch c {
	case CaseDriver:
		return wantedError{http.StatusServiceUnavailable, filesystem.CodeUnavailable, http.StatusText(http.StatusServiceUnavailable), ""}
	case CaseRejected:
		return wantedError{http.StatusUnsupportedMediaType, filesystem.CodeRejected, rejectedReason, ""}
	case CaseTooLarge:
		return wantedError{http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge, (&http.MaxBytesError{Limit: 1}).Error(), ""}
	case CaseRetry:
		return wantedError{http.StatusServiceUnavailable, filesystem.CodeUnavailable, http.StatusText(http.StatusServiceUnavailable), retryHeader}
	}
	return wantedError{}
}

func zipDownload(t *testing.T, base string) {
	status, got, resp := Do(t, http.MethodGet, base+Group+PathZip, nil, nil)
	if status != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), ZipName) {
		t.Fatalf("zip = %d %v", status, resp.Header)
	}
	if !strings.HasPrefix(got, "PK") || len(got) < bigSize {
		t.Errorf("zip body: %d bytes", len(got))
	}
}

// zipBroken proves a zip whose source fails after the headers went out
// reaches the client as a failed download, never as a clean end of a
// truncated archive.
func zipBroken(t *testing.T, link string) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, nil)
	must(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return // reset before the headers: also a failed download
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Errorf("a zip cut short after %d bytes ended cleanly (status %d); the client would keep a truncated archive", n, resp.StatusCode)
	}
}

// Form returns a multipart body with one file in the "file" field.
func Form(t *testing.T, name, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(upload.DefaultField, name)
	must(t, err)
	_, err = io.WriteString(fw, content)
	must(t, err)
	must(t, mw.Close())
	return &buf, mw.FormDataContentType()
}

// Do sends a request without following redirects and returns the status,
// body and response.
func Do(t *testing.T, method, url string, header http.Header, body io.Reader) (int, string, *http.Response) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	must(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	must(t, err)
	return resp.StatusCode, string(b), resp
}

// Headers builds a header set from name, value pairs, skipping empty
// values.
func Headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			h.Set(kv[i], kv[i+1])
		}
	}
	return h
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
