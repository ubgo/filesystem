package azure_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// reply is one canned answer of a scripted server.
type reply struct {
	status int
	header map[string]string
	body   string
}

// scripted is a stand-in for the Blob service, for what Azurite does not
// implement (customer-provided keys, legal holds, immutability policies,
// versions, rehydration): it records every request and answers the first
// rule whose match is contained in "METHOD path?query", or with the
// success status the SDK expects.
type scripted struct {
	mu    sync.Mutex
	rules []struct {
		match string
		reply reply
	}
	reqs []*http.Request
	// bodies, when not nil, keeps the body of every Put Blob by path and
	// answers a Get Blob no rule matches with it, so a blob the driver
	// wrote (a multipart marker) can be read back.
	bodies map[string][]byte
}

func (s *scripted) on(match string, r reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, struct {
		match string
		reply reply
	}{match, r})
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
	line := r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
	rep := reply{status: defaultStatus(r)}
	matched := false
	for _, rule := range s.rules {
		if strings.Contains(line, rule.match) {
			rep, matched = rule.reply, true
			break
		}
	}
	if s.bodies != nil && !matched {
		switch {
		case r.Method == http.MethodPut && r.URL.RawQuery == "":
			s.bodies[r.URL.Path] = body
		case r.Method == http.MethodGet && s.bodies[r.URL.Path] != nil:
			rep.body = string(s.bodies[r.URL.Path])
			rep.header = map[string]string{"Content-Length": strconv.Itoa(len(rep.body))}
		}
	}
	s.mu.Unlock()
	for k, v := range rep.header {
		w.Header().Set(k, v)
	}
	if rep.status >= http.StatusBadRequest {
		w.Header().Set("x-ms-error-code", rep.header["x-ms-error-code"])
	}
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

// defaultStatus is the success status the SDK expects for r.
func defaultStatus(r *http.Request) int {
	switch {
	case r.Header.Get("x-ms-copy-source") != "", r.Method == http.MethodDelete && r.URL.Query().Get("comp") == "":
		return http.StatusAccepted
	case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "":
		return http.StatusCreated
	case r.Method == http.MethodPut && slices.Contains([]string{"block", "blocklist", "appendblock"}, r.URL.Query().Get("comp")):
		return http.StatusCreated
	}
	return http.StatusOK
}

// sent returns the recorded requests whose "METHOD path?query" contains
// every one of parts.
func (s *scripted) sent(parts ...string) []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*http.Request
outer:
	for _, r := range s.reqs {
		line := r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		for _, p := range parts {
			if !strings.Contains(line, p) {
				continue outer
			}
		}
		out = append(out, r)
	}
	return out
}

// scriptedDriver returns a driver on container "c" of a scripted service.
func scriptedDriver(t *testing.T, cfg azure.Config) (*azure.Driver, *filesystem.Disk, *scripted) {
	t.Helper()
	s := &scripted{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	client, err := azblob.NewClientWithNoCredential(srv.URL+"/", &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Client, cfg.Container = client, "c"
	drv, err := azure.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("azure", drv)
	if err != nil {
		t.Fatal(err)
	}
	return drv, d, s
}

// propsHeaders are the headers of a Get Blob Properties answer.
func propsHeaders(extra map[string]string) map[string]string {
	h := map[string]string{"Content-Length": "1", "ETag": `"0x1"`, "Last-Modified": time.Now().UTC().Format(http.TimeFormat), "x-ms-blob-type": "BlockBlob"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func failure(status int, code string) reply {
	return reply{status: status, header: map[string]string{"x-ms-error-code": code}}
}

var keyBytes = bytes.Repeat([]byte{0xCD}, azure.CustomerKeySize)

func TestCustomerKeyIsRedacted(t *testing.T) {
	k, err := azure.NewCustomerKey(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	cfg := azure.Config{Container: "c", CustomerKey: k}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("cfg", "key", k, "cfg", cfg)
	asJSON, _ := json.Marshal(struct{ Key azure.CustomerKey }{k})
	printed := fmt.Sprintf("%v %+v %#v %s %x", k, cfg, cfg, k, k) + logged.String() + string(asJSON)
	for _, leak := range []string{base64.StdEncoding.EncodeToString(keyBytes), "cdcd", "\xcd\xcd", "205, 205"} {
		if strings.Contains(printed, leak) {
			t.Fatalf("key leaked (%q) in %s", leak, printed)
		}
	}
	if _, err := azure.NewCustomerKey(keyBytes[:16]); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a 16-byte key = %v", err)
	}
}

func TestCustomerKeyOnEveryBlobCall(t *testing.T) {
	ctx := t.Context()
	k, _ := azure.NewCustomerKey(keyBytes)
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	drv, d, s := scriptedDriver(t, azure.Config{CustomerKey: k, SharedKey: cred})
	if c := drv.Capabilities(); c.Presign || c.Copy {
		t.Fatalf("a CPK disk declares signed URLs or server-side copy: %+v", c)
	}
	s.on("HEAD /c/f.txt", reply{status: http.StatusOK, header: propsHeaders(nil)})
	s.on("GET /c/f.txt", reply{status: http.StatusOK, header: propsHeaders(nil), body: "x"})
	if _, err := d.WriteBytes(ctx, "f.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadAll(ctx, "f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Append(ctx, "f.txt", strings.NewReader("y"), filesystem.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(keyBytes)
	want := base64.StdEncoding.EncodeToString(sum[:])
	reqs := s.sent("/c/f.txt")
	if len(reqs) < 6 { // stage, commit, stat, read, create, append, stat...
		t.Fatalf("only %d requests", len(reqs))
	}
	for _, r := range reqs {
		if r.Header.Get("x-ms-encryption-key-sha256") != want || r.Header.Get("x-ms-encryption-key") == "" {
			t.Errorf("%s %s sent no customer key", r.Method, r.URL)
		}
	}
	if _, err := azure.New(azure.Config{Client: drv.Native(), Container: "c", CustomerKey: k, EncryptionScope: "s"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("CustomerKey with EncryptionScope = %v", err)
	}
}

func TestHoldRequests(t *testing.T) {
	ctx := t.Context()
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	drv, d, s := scriptedDriver(t, azure.Config{RetainFor: time.Hour, RetentionMode: blob.ImmutabilityPolicySettingLocked})
	s.on("HEAD /c/held.txt", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-legal-hold": "true", "x-ms-immutability-policy-until-date": until.Format(http.TimeFormat), "x-ms-immutability-policy-mode": "locked"})})
	h, err := d.Hold(ctx, "held.txt")
	if err != nil || !h.Legal || !h.RetainUntil.Equal(until) {
		t.Fatalf("hold = %+v, %v", h, err)
	}
	info, err := drv.ObjectInfo(ctx, "held.txt")
	if err != nil || info.RetentionMode != blob.ImmutabilityPolicyModeLocked || !info.Hold.Legal {
		t.Fatalf("object info = %+v, %v", info, err)
	}
	// Release the legal hold and extend the policy.
	later := until.Add(24 * time.Hour)
	if err := d.SetHold(ctx, "held.txt", filesystem.Hold{RetainUntil: later}); err != nil {
		t.Fatal(err)
	}
	if r := s.sent("PUT /c/held.txt", "comp=legalhold"); len(r) != 1 || r[0].Header.Get("x-ms-legal-hold") != "false" {
		t.Fatalf("legal hold release not sent: %v", r)
	}
	r := s.sent("PUT /c/held.txt", "comp=immutabilityPolicies")
	if len(r) != 1 || r[0].Header.Get("x-ms-immutability-policy-mode") != "Locked" || r[0].Header.Get("x-ms-immutability-policy-until-date") != later.Format(http.TimeFormat) {
		t.Fatalf("policy not sent: %v", r)
	}
	// The same hold again sends nothing.
	s.reqs = nil
	if err := d.SetHold(ctx, "held.txt", h); err != nil || len(s.sent("PUT")) != 0 {
		t.Fatalf("an unchanged hold sent %d requests (%v)", len(s.sent("PUT")), err)
	}
	// Lifting the policy deletes it.
	if err := d.SetHold(ctx, "held.txt", filesystem.Hold{Legal: true}); err != nil || len(s.sent("DELETE /c/held.txt", "comp=immutabilityPolicies")) != 1 {
		t.Fatalf("policy delete not sent (%v)", err)
	}
	// A write gets RetainFor's policy right after its commit.
	s.reqs = nil
	s.on("HEAD /c/new.txt", reply{status: http.StatusOK, header: propsHeaders(nil)})
	if _, err := d.WriteBytes(ctx, "new.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(s.sent("PUT /c/new.txt", "comp=immutabilityPolicies")) != 1 {
		t.Fatal("a write with RetainFor set no policy")
	}
	// Azure refuses to delete a protected blob.
	s.on("DELETE /c/held.txt?", failure(http.StatusConflict, "BlobImmutableDueToPolicy"))
	wantCode(t, d.Delete(ctx, "held.txt"), filesystem.CodePermission)
	if _, err := azure.New(azure.Config{Client: drv.Native(), Container: "c", RetentionMode: "Forever"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("unknown retention mode = %v", err)
	}
}

func TestTierAndRestore(t *testing.T) {
	ctx := t.Context()
	drv, d, s := scriptedDriver(t, azure.Config{AccessTier: blob.AccessTierCool, RehydratePriority: blob.RehydratePriorityHigh})
	s.on("HEAD /c/a.bin", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-access-tier": "Archive", "x-ms-archive-status": "rehydrate-pending-to-hot"})})
	s.on("GET /c/a.bin", failure(http.StatusConflict, "BlobArchived"))
	if _, err := d.WriteBytes(ctx, "a.bin", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := s.sent("PUT /c/a.bin"); len(r) != 1 || r[0].Header.Get("x-ms-access-tier") != "Cool" {
		t.Fatalf("the upload did not carry the tier: %v", r)
	}
	_, err := d.ReadAll(ctx, "a.bin")
	wantCode(t, err, filesystem.CodeConflict)
	if !errors.Is(err, azure.ErrArchived) {
		t.Fatalf("a read of an archived blob = %v, want ErrArchived", err)
	}
	if err := d.Restore(ctx, "a.bin", filesystem.RestoreOptions{Days: 1}); err != nil {
		t.Fatal(err)
	}
	if r := s.sent("PUT /c/a.bin", "comp=tier"); len(r) != 1 || r[0].Header.Get("x-ms-access-tier") != "Hot" || r[0].Header.Get("x-ms-rehydrate-priority") != "High" {
		t.Fatalf("restore sent %v", r)
	}
	info, err := drv.ObjectInfo(ctx, "a.bin")
	if err != nil || info.ArchiveStatus != blob.ArchiveStatusRehydratePendingToHot {
		t.Fatalf("object info = %+v, %v", info, err)
	}
	if e, err := d.Stat(ctx, "a.bin"); err != nil || e.StorageClass != "Archive" {
		t.Fatalf("stat reports class %q (%v), want the tier", e.StorageClass, err)
	}
	if err := d.SetStorageClass(ctx, "a.bin", string(blob.AccessTierCold)); err != nil {
		t.Fatal(err)
	}
	if r := s.sent("PUT /c/a.bin", "comp=tier"); len(r) != 2 || r[1].Header.Get("x-ms-access-tier") != "Cold" || r[1].Header.Get("x-ms-rehydrate-priority") != "High" {
		t.Fatalf("set tier sent %v", r)
	}
	wantCode(t, d.SetStorageClass(ctx, "a.bin", "Frozen"), filesystem.CodeInvalidArgument)
}

// versionList is a List Blobs answer with versions of v.txt (and of a
// longer key sharing its prefix, which must be left out).
const versionList = `<?xml version="1.0" encoding="utf-8"?><EnumerationResults ServiceEndpoint="x" ContainerName="c"><Blobs>
<Blob><Name>v.txt</Name><VersionId>2026-01-01T00:00:00.0000000Z</VersionId><Properties><Content-Length>2</Content-Length><Etag>"0x1"</Etag></Properties></Blob>
<Blob><Name>v.txt</Name><VersionId>2026-03-01T00:00:00.0000000Z</VersionId><IsCurrentVersion>true</IsCurrentVersion><Properties><Content-Length>3</Content-Length><Etag>"0x3"</Etag></Properties></Blob>
<Blob><Name>v.txt</Name><VersionId>2026-02-01T00:00:00.0000000Z</VersionId><Properties><Content-Length>4</Content-Length><Etag>"0x2"</Etag></Properties></Blob>
<Blob><Name>v.txt.bak</Name><VersionId>2026-04-01T00:00:00.0000000Z</VersionId><IsCurrentVersion>true</IsCurrentVersion></Blob>
</Blobs><NextMarker/></EnumerationResults>`

func TestVersions(t *testing.T) {
	ctx := t.Context()
	drv, d, s := scriptedDriver(t, azure.Config{Versioned: true})
	s.on("GET /c?", reply{status: http.StatusOK, header: map[string]string{"Content-Type": "application/xml"}, body: versionList})
	vs, err := d.Versions(ctx, "v.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 || !vs[0].Latest || vs[0].ETag != "0x3" || vs[2].ID != "2026-01-01T00:00:00.0000000Z" || vs[1].Latest {
		t.Fatalf("versions = %+v", vs)
	}
	if len(s.sent("GET /c?", "include=versions", "prefix=v.txt")) != 1 {
		t.Fatal("listing did not ask for versions of the key")
	}
	_, err = d.Versions(ctx, "other.txt")
	wantCode(t, err, filesystem.CodeNotFound)

	s.on("GET /c/v.txt?versionid=", reply{status: http.StatusOK, header: propsHeaders(nil), body: "o"})
	r, err := d.ReadVersion(ctx, "v.txt", vs[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if len(s.sent("GET /c/v.txt", "versionid=2026-01-01")) != 1 {
		t.Fatal("read did not name the version")
	}

	s.on("HEAD /c/v.txt", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-copy-status": "success"})})
	if err := d.RestoreVersion(ctx, "v.txt", vs[2].ID); err != nil {
		t.Fatal(err)
	}
	cp := s.sent("PUT /c/v.txt")
	if len(cp) != 1 || !strings.Contains(cp[0].Header.Get("x-ms-copy-source"), "versionid=2026-01-01") {
		t.Fatalf("restore copied from %v", cp)
	}
	k, _ := azure.NewCustomerKey(keyBytes)
	_, encrypted, _ := scriptedDriver(t, azure.Config{Versioned: true, CustomerKey: k})
	wantCode(t, encrypted.RestoreVersion(ctx, "v.txt", vs[2].ID), filesystem.CodeUnsupported)
	if !drv.Capabilities().VersionRead {
		t.Fatal("VersionRead not declared with Versioned")
	}

	_, plain, _ := scriptedDriver(t, azure.Config{})
	_, err = plain.Versions(ctx, "v.txt")
	wantCode(t, err, filesystem.CodeUnsupported)
}

func TestSwapAndAppendErrors(t *testing.T) {
	ctx := t.Context()
	_, d, s := scriptedDriver(t, azure.Config{})
	// The blob changed since the caller's ETag.
	s.on("PUT /c/doc.json", failure(http.StatusPreconditionFailed, "ConditionNotMet"))
	_, err := d.WriteBytes(ctx, "doc.json", []byte("x"), filesystem.WriteOptions{IfMatch: "0x1"})
	wantCode(t, err, filesystem.CodeConflict)
	if r := s.sent("PUT /c/doc.json"); len(r) != 1 || r[0].Header.Get("If-Match") != `"0x1"` {
		t.Fatalf("upload sent %v", r)
	}
	// A body of several blocks (one byte over a block, so the SDK stages
	// rather than putting it whole): another writer's commit discarded
	// this writer's staged blocks.
	s.on("PUT /c/big.json?comp=blocklist", failure(http.StatusBadRequest, "InvalidBlockList"))
	_, err = d.Write(ctx, "big.json", io.LimitReader(zeros{}, azure.DefaultBlockSize+1), filesystem.WriteOptions{IfMatch: "0x1"})
	wantCode(t, err, filesystem.CodeConflict)
	if r := s.sent("PUT /c/big.json", "comp=blocklist"); len(r) != 1 || r[0].Header.Get("If-Match") != `"0x1"` {
		t.Fatalf("commit sent %v", r)
	}
	// Appending to a block blob.
	s.on("PUT /c/block.txt?comp=appendblock", failure(http.StatusConflict, "InvalidBlobType"))
	s.on("PUT /c/block.txt", failure(http.StatusConflict, "BlobAlreadyExists"))
	_, err = d.Append(ctx, "block.txt", strings.NewReader("x"), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeConflict)
	_, err = d.Append(ctx, "big.txt", io.LimitReader(zeros{}, azure.MaxAppendBlock+1), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeTooLarge)
	if len(s.sent("/c/big.txt")) != 0 {
		t.Fatal("an oversized append reached Azure")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestOpenerAdvancedParams(t *testing.T) {
	ctx := t.Context()
	t.Setenv("AZ_TEST_CONN", "AccountName=acct;AccountKey="+base64.StdEncoding.EncodeToString([]byte("k"))+";BlobEndpoint=http://127.0.0.1:1/acct")
	t.Setenv("AZ_TEST_CPK", base64.StdEncoding.EncodeToString(keyBytes))
	t.Setenv("AZ_TEST_SHORT", base64.StdEncoding.EncodeToString([]byte("short")))
	mux := filesystem.NewMux()
	_ = mux.Register(azure.Scheme, azure.Opener)
	_ = mux.Register("azid", azure.TokenOpener(fakeToken{}))
	base := "azblob://c?connection_env=AZ_TEST_CONN"
	d, err := mux.Open(ctx, "az", base+"&access_tier=Cool&versioned=true&customer_key_env=AZ_TEST_CPK")
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities(); !c.Versions || c.Presign || c.Copy {
		t.Fatalf("capabilities from DSN = %+v", c)
	}
	if d, err := mux.Open(ctx, "az", "azid://c?account=acct&versioned=true"); err != nil || !d.Capabilities().Versions {
		t.Fatalf("token opener with versioned = %v", err)
	}
	for _, bad := range []string{"&access_tier=Frozen", "&versioned=maybe", "&customer_key_env=AZ_TEST_SHORT", "&customer_key_env=AZ_TEST_CONN"} {
		if _, err := mux.Open(ctx, "az", base+bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Open(%s) = %v, want CodeInvalidArgument", bad, err)
		}
	}
}

// TestRetainForOnEveryWrite checks that every path creating a blob sets
// Config.RetainFor's policy, that what cannot (SAS uploads, appends to a
// protected append blob) is not offered, and that a failed policy request
// says the blob was stored.
func TestRetainForOnEveryWrite(t *testing.T) {
	ctx := t.Context()
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	drv, d, s := scriptedDriver(t, azure.Config{RetainFor: time.Hour, Versioned: true, SharedKey: cred})
	if c := drv.Capabilities(); c.Presign || c.Append {
		t.Fatalf("a disk with RetainFor offers SAS uploads or appends: %+v", c)
	}
	done := propsHeaders(map[string]string{"x-ms-copy-status": "success"})
	s.on("HEAD /c/", reply{status: http.StatusOK, header: done})
	if _, err := d.Copy(ctx, "src.txt", "copy.txt"); err != nil {
		t.Fatal(err)
	}
	if err := d.RestoreVersion(ctx, "restored.txt", "2026-01-01T00:00:00.0000000Z"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"copy.txt", "restored.txt"} {
		if len(s.sent("PUT /c/"+key, "comp=immutabilityPolicies")) != 1 {
			t.Errorf("%s got no immutability policy", key)
		}
	}
	s.on("PUT /c/unprotected.txt?comp=immutabilityPolicies", failure(http.StatusServiceUnavailable, "ServerBusy"))
	_, err := d.WriteBytes(ctx, "unprotected.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeUnavailable)
	if !strings.Contains(err.Error(), "was stored") {
		t.Fatalf("a failed policy request does not say the blob was stored: %v", err)
	}
}

func TestHoldRefusalsArePermission(t *testing.T) {
	ctx := t.Context()
	_, d, s := scriptedDriver(t, azure.Config{})
	s.on("PUT /c/held.txt", failure(http.StatusConflict, "BlobImmutableDueToLegalHold"))
	s.on("DELETE /c/held.txt", failure(http.StatusConflict, "BlobImmutableDueToLegalHold"))
	s.on("PUT /c/other.txt", failure(http.StatusConflict, "BlobAlreadyExists"))
	_, err := d.WriteBytes(ctx, "held.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodePermission)
	wantCode(t, d.Delete(ctx, "held.txt"), filesystem.CodePermission)
	_, err = d.WriteBytes(ctx, "other.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeExists)
}
