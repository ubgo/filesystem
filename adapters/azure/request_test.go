package azure_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestRequestOptionsWrite proves a write of the bound key gets the call's
// scope, tier, tags, legal hold and retention, while a write of any other
// key with the same context keeps the disk's Config (the default).
func TestRequestOptionsWrite(t *testing.T) {
	until := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	_, d, s := scriptedDriver(t, azure.Config{EncryptionScope: "scope-default", AccessTier: blob.AccessTierCool})
	ctx, err := azure.WithRequestOptions(t.Context(), d, "tenants/a/x", azure.RequestOptions{
		EncryptionScope: "scope-a", AccessTier: blob.AccessTierArchive, Tags: map[string]string{"tenant": "a"}, LegalHold: true, RetainUntil: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tenants/a/x", "tenants/a/y"} {
		if _, err := d.WriteBytes(ctx, key, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put := withComp(s.sent("PUT /c/tenants/a/x?"), "")
	if len(put) != 1 || put[0].Header.Get("x-ms-encryption-scope") != "scope-a" || put[0].Header.Get("x-ms-access-tier") != "Archive" || put[0].Header.Get("x-ms-tags") != "tenant=a" {
		t.Fatalf("the bound key's upload = %v", put)
	}
	if r := s.sent("PUT /c/tenants/a/x", "comp=legalhold"); len(r) != 1 || r[0].Header.Get("x-ms-legal-hold") != "true" {
		t.Errorf("legal hold not set: %v", r)
	}
	if r := s.sent("PUT /c/tenants/a/x", "comp=immutabilityPolicies"); len(r) != 1 || r[0].Header.Get("x-ms-immutability-policy-until-date") != until.Format(http.TimeFormat) {
		t.Errorf("retention not set: %v", r)
	}
	other := withComp(s.sent("PUT /c/tenants/a/y?"), "")
	if len(other) != 1 || other[0].Header.Get("x-ms-encryption-scope") != "scope-default" || other[0].Header.Get("x-ms-access-tier") != "Cool" || other[0].Header.Get("x-ms-tags") != "" {
		t.Fatalf("another key inherited the options: %v", other)
	}
	if len(s.sent("PUT /c/tenants/a/y", "comp=")) != 0 {
		t.Error("another key got a hold or a policy")
	}
}

// TestRequestOptionsScopedDisk proves the options bind to the key the
// driver sees under a scoped disk's prefix.
func TestRequestOptionsScopedDisk(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{})
	tenant, err := d.Scope("tenant-a", "tenants/a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := azure.WithRequestOptions(t.Context(), tenant, "x", azure.RequestOptions{EncryptionScope: "scope-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.WriteBytes(ctx, "x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := s.sent("PUT /c/tenants/a/x?"); len(r) != 1 || r[0].Header.Get("x-ms-encryption-scope") != "scope-a" {
		t.Fatalf("scoped write = %v", r)
	}
	if _, err := azure.WithRequestOptions(t.Context(), tenant, "../escape", azure.RequestOptions{}); err == nil {
		t.Error("an invalid key was bound")
	}
}

// withComp keeps the requests whose comp query parameter is comp ("" for
// Put Blob and Start Copy, which have none).
func withComp(rs []*http.Request, comp string) []*http.Request {
	var out []*http.Request
	for _, r := range rs {
		if r.URL.Query().Get("comp") == comp {
			out = append(out, r)
		}
	}
	return out
}

// bind returns a context carrying o for key on d, failing the test on an
// invalid key.
func bind(t *testing.T, d *filesystem.Disk, key string, o azure.RequestOptions) context.Context {
	t.Helper()
	ctx, err := azure.WithRequestOptions(t.Context(), d, key, o)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestRequestOptionsRefused proves values Azure does not know, and fields
// a call cannot honour, fail before any request instead of being dropped.
func TestRequestOptionsRefused(t *testing.T) {
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	_, d, s := scriptedDriver(t, azure.Config{SharedKey: cred})
	_, err := d.WriteBytes(bind(t, d, "k", azure.RequestOptions{AccessTier: "Frozen"}), "k", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	wantCode(t, d.Restore(bind(t, d, "k", azure.RequestOptions{RehydratePriority: "Instant"}), "k", filesystem.RestoreOptions{Days: 1}), filesystem.CodeInvalidArgument)
	wantCode(t, d.SetStorageClass(bind(t, d, "k", azure.RequestOptions{RehydrateTier: "Frozen"}), "k", "Hot"), filesystem.CodeInvalidArgument)
	_, err = d.Copy(bind(t, d, "dst", azure.RequestOptions{EncryptionScope: "scope-a"}), "src", "dst")
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = d.Append(bind(t, d, "log", azure.RequestOptions{AccessTier: blob.AccessTierCool}), "log", strings.NewReader("x"), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = d.SignedURL(bind(t, d, "up", azure.RequestOptions{Tags: map[string]string{"a": "b"}}), "up", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = d.CreateMultipart(bind(t, d, "big", azure.RequestOptions{AccessTier: "Frozen"}), "big", filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if r := s.sent("PUT"); len(r) != 0 {
		t.Fatalf("refused calls sent %v", r)
	}
	key, _ := azure.NewCustomerKey(keyBytes)
	_, cpk, _ := scriptedDriver(t, azure.Config{CustomerKey: key})
	_, err = cpk.WriteBytes(bind(t, cpk, "k", azure.RequestOptions{EncryptionScope: "scope-a"}), "k", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestRequestOptionsCopy proves a server-side copy (and RestoreVersion,
// which is one) carries the call's tags and tier and gets its hold, and
// that a plain copy still sends no tier.
func TestRequestOptionsCopy(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{AccessTier: blob.AccessTierCool, Versioned: true})
	s.on("HEAD /c/", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-copy-status": "success"})})
	ctx := bind(t, d, "dst", azure.RequestOptions{Tags: map[string]string{"kind": "copy"}, AccessTier: blob.AccessTierCold, LegalHold: true})
	if _, err := d.Copy(ctx, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	if r := withComp(s.sent("PUT /c/dst?"), ""); len(r) != 1 || r[0].Header.Get("x-ms-tags") != "kind=copy" || r[0].Header.Get("x-ms-access-tier") != "Cold" {
		t.Fatalf("copy = %v", r)
	}
	if len(s.sent("PUT /c/dst", "comp=legalhold")) != 1 {
		t.Error("the copy got no legal hold")
	}
	if err := d.RestoreVersion(bind(t, d, "v.txt", azure.RequestOptions{Tags: map[string]string{"restored": "yes"}}), "v.txt", "2026-01-01T00:00:00.0000000Z"); err != nil {
		t.Fatal(err)
	}
	if r := withComp(s.sent("PUT /c/v.txt?"), ""); len(r) != 1 || r[0].Header.Get("x-ms-tags") != "restored=yes" {
		t.Fatalf("restore version = %v", r)
	}
	if _, err := d.Copy(t.Context(), "src", "plain"); err != nil {
		t.Fatal(err)
	}
	if r := withComp(s.sent("PUT /c/plain?"), ""); len(r) != 1 || r[0].Header.Get("x-ms-access-tier") != "" || r[0].Header.Get("x-ms-tags") != "" {
		t.Fatalf("a plain copy changed: %v", r)
	}
}

// TestRequestOptionsMultipart proves an upload keeps CreateMultipart's
// options: parts and completion made with a bare context still stage and
// commit with its scope, tier and tags.
func TestRequestOptionsMultipart(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{})
	s.bodies = map[string][]byte{}
	id, err := d.CreateMultipart(bind(t, d, "big", azure.RequestOptions{EncryptionScope: "scope-m", AccessTier: blob.AccessTierCool, Tags: map[string]string{"t": "1"}}), "big", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	p, err := d.UploadPart(ctx, "big", id, 1, bytes.NewReader([]byte("part")), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteMultipart(ctx, "big", id, []filesystem.Part{p}); err != nil {
		t.Fatal(err)
	}
	if r := withComp(s.sent("PUT /c/big"), "block"); len(r) != 1 || r[0].Header.Get("x-ms-encryption-scope") != "scope-m" {
		t.Fatalf("staged block = %v", r)
	}
	c := withComp(s.sent("PUT /c/big"), "blocklist")
	if len(c) != 1 || c[0].Header.Get("x-ms-encryption-scope") != "scope-m" || c[0].Header.Get("x-ms-access-tier") != "Cool" || c[0].Header.Get("x-ms-tags") != "t=1" {
		t.Fatalf("commit = %v", c)
	}
}

// TestRequestOptionsAppendAndPresign proves Append creates the blob with
// the call's scope and tags, and a presigned PUT's SAS carries the call's
// scope, while a GET signed with the same context is unaffected.
func TestRequestOptionsAppendAndPresign(t *testing.T) {
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	_, d, s := scriptedDriver(t, azure.Config{SharedKey: cred})
	if _, err := d.Append(bind(t, d, "log", azure.RequestOptions{EncryptionScope: "scope-l", Tags: map[string]string{"t": "log"}}), "log", strings.NewReader("x"), filesystem.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	create := withComp(s.sent("PUT /c/log?"), "")
	if len(create) != 1 || create[0].Header.Get("x-ms-encryption-scope") != "scope-l" || create[0].Header.Get("x-ms-tags") != "t=log" {
		t.Fatalf("append create = %v", create)
	}
	if r := s.sent("PUT /c/log", "comp=appendblock"); len(r) != 1 || r[0].Header.Get("x-ms-encryption-scope") != "scope-l" {
		t.Fatalf("append block = %v", r)
	}
	ctx := bind(t, d, "up", azure.RequestOptions{EncryptionScope: "scope-p"})
	put, err := d.SignedURL(ctx, "up", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(put.URL); u.Query().Get("ses") != "scope-p" {
		t.Errorf("presigned PUT scope = %q", u.Query().Get("ses"))
	}
	get, err := d.SignedURL(ctx, "up", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := url.Parse(get.URL); u.Query().Has("ses") {
		t.Errorf("presigned GET carries a scope: %s", get.URL)
	}
}

// TestRequestOptionsRehydration proves Restore and SetStorageClass take
// the call's tier and priority over Config's.
func TestRequestOptionsRehydration(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{RehydratePriority: blob.RehydratePriorityStandard})
	ctx := bind(t, d, "a.bin", azure.RequestOptions{RehydrateTier: blob.AccessTierCool, RehydratePriority: blob.RehydratePriorityHigh})
	if err := d.Restore(ctx, "a.bin", filesystem.RestoreOptions{Days: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStorageClass(ctx, "a.bin", string(blob.AccessTierHot)); err != nil {
		t.Fatal(err)
	}
	r := s.sent("PUT /c/a.bin", "comp=tier")
	if len(r) != 2 || r[0].Header.Get("x-ms-access-tier") != "Cool" || r[0].Header.Get("x-ms-rehydrate-priority") != "High" || r[1].Header.Get("x-ms-rehydrate-priority") != "High" {
		t.Fatalf("tier requests = %v", r)
	}
}

// TestRequestOptionsLateRefusals proves the checks that need the disk's
// own settings refuse before any request too: a copy to an unknown tier,
// and an append with an encryption scope on a customer-key disk; and that
// a legal hold that fails after the write says the blob was stored.
func TestRequestOptionsLateRefusals(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{})
	_, err := d.Copy(bind(t, d, "dst", azure.RequestOptions{AccessTier: "Frozen"}), "src", "dst")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	key, _ := azure.NewCustomerKey(keyBytes)
	_, cpk, cs := scriptedDriver(t, azure.Config{CustomerKey: key})
	_, err = cpk.Append(bind(t, cpk, "log", azure.RequestOptions{EncryptionScope: "scope-a"}), "log", strings.NewReader("x"), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// A customer-key disk declares no presigning; its driver, called
	// directly, refuses a scope it could not honour.
	cred, _ := azblob.NewSharedKeyCredential("acct", base64.StdEncoding.EncodeToString([]byte("k")))
	cpkDrv, cpkSigned, _ := scriptedDriver(t, azure.Config{CustomerKey: key, SharedKey: cred})
	_, err = cpkDrv.Presign(bind(t, cpkSigned, "up", azure.RequestOptions{EncryptionScope: "scope-a"}), "up", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if r := append(s.sent("PUT"), cs.sent("PUT")...); len(r) != 0 {
		t.Fatalf("refused calls sent %v", r)
	}
	s.on("PUT /c/held.txt?comp=legalhold", failure(http.StatusServiceUnavailable, "ServerBusy"))
	_, err = d.WriteBytes(bind(t, d, "held.txt", azure.RequestOptions{LegalHold: true}), "held.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeUnavailable)
	if !strings.Contains(err.Error(), "was stored") {
		t.Fatalf("a failed legal hold does not say the blob was stored: %v", err)
	}
}
