package azure_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/azure"
)

// TestDriverRefusesReservedKeys proves the Azure-specific methods refuse a
// key under MultipartDir when called on the driver directly, past the
// Disk that normally refuses it first, and that Azure is never asked.
// TestReservedKeysRefused covers the same keys through the Disk.
func TestDriverRefusesReservedKeys(t *testing.T) {
	ctx := t.Context()
	drv, _, s := scriptedDriver(t, azure.Config{Versioned: true})
	marker := azure.MultipartDir + "/abcdefghijklmnopqrstuvwxyz.json"
	checks := map[string]error{
		"set storage class": drv.SetStorageClass(ctx, marker, string(blob.AccessTierCool)),
		"restore":           drv.Restore(ctx, marker, filesystem.RestoreOptions{Days: 1}),
		"set tags":          drv.SetTags(ctx, marker, map[string]string{"a": "b"}),
		"set hold":          drv.SetHold(ctx, marker, filesystem.Hold{Legal: true}),
		"restore version":   drv.RestoreVersion(ctx, marker, "2026-01-01T00:00:00.0000000Z"),
	}
	_, checks["append"] = drv.Append(ctx, marker, strings.NewReader("x"), filesystem.AppendOptions{})
	_, checks["object info"] = drv.ObjectInfo(ctx, marker)
	_, checks["hold"] = drv.Hold(ctx, marker)
	_, checks["tags"] = drv.Tags(ctx, marker)
	_, checks["versions"] = drv.Versions(ctx, marker)
	_, checks["read version"] = drv.ReadVersion(ctx, marker, "2026-01-01T00:00:00.0000000Z")
	for name, err := range checks {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("%s of a marker = %v, want invalid_key", name, err)
		}
	}
	// ObjectInfo cleans its key itself, so a traversal is refused too.
	_, err := drv.ObjectInfo(ctx, "../escape.txt")
	wantCode(t, err, filesystem.CodeInvalidKey)
	if n := len(s.sent("")); n != 0 {
		t.Errorf("%d requests reached Azure for refused keys", n)
	}
}

// TestAppendOptions proves the driver's Append refuses a body over
// opts.MaxSize before any request (the Disk's metered body normally
// refuses it first, so the driver is called directly), sends a MaxSize
// that fits as Azure's own max-size condition, sends a CRC64 with
// ValidateCRC64, and reports a failed create with its code.
func TestAppendOptions(t *testing.T) {
	ctx := t.Context()
	const maxSize = 4
	drv, d, s := scriptedDriver(t, azure.Config{ValidateCRC64: true})
	_, err := drv.Append(ctx, "log.txt", strings.NewReader("12345"), filesystem.AppendOptions{MaxSize: maxSize})
	wantCode(t, err, filesystem.CodeTooLarge)
	if n := len(s.sent("/c/log.txt")); n != 0 {
		t.Fatalf("an append over MaxSize sent %d requests", n)
	}
	s.on("HEAD /c/log.txt", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-blob-type": "AppendBlob"})})
	if _, err := d.Append(ctx, "log.txt", strings.NewReader("1234"), filesystem.AppendOptions{MaxSize: maxSize}); err != nil {
		t.Fatal(err)
	}
	r := s.sent("PUT /c/log.txt", "comp=appendblock")
	if len(r) != 1 || r[0].Header.Get("x-ms-blob-condition-maxsize") != "4" || r[0].Header.Get("x-ms-content-crc64") == "" {
		t.Fatalf("append block sent %v", r)
	}
	s.on("PUT /c/busy.txt", failure(http.StatusServiceUnavailable, "ServerBusy"))
	_, err = d.Append(ctx, "busy.txt", strings.NewReader("x"), filesystem.AppendOptions{})
	wantCode(t, err, filesystem.CodeUnavailable)
	if len(s.sent("PUT /c/busy.txt", "comp=appendblock")) != 0 {
		t.Fatal("appended after the create failed")
	}
}

// TestWriteSendsCRC64 proves Write asks Azure to check a CRC64 of what it
// receives when Config.ValidateCRC64 is on (Azurite ignores the header,
// so only the request can show it).
func TestWriteSendsCRC64(t *testing.T) {
	ctx := t.Context()
	_, d, s := scriptedDriver(t, azure.Config{ValidateCRC64: true})
	s.on("HEAD /c/f.txt", reply{status: http.StatusOK, header: propsHeaders(nil)})
	if _, err := d.WriteBytes(ctx, "f.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.sent("PUT /c/f.txt") {
		if r.URL.Query().Get("comp") != "blocklist" && r.Header.Get("x-ms-content-crc64") == "" {
			t.Errorf("%s %s sent no CRC64", r.Method, r.URL)
		}
	}
	if len(s.sent("PUT /c/f.txt")) == 0 {
		t.Fatal("the write sent nothing")
	}
}

// TestRestoreDefaults proves Restore without Config.RehydrateTier or
// RehydratePriority rehydrates to Hot with Standard priority.
func TestRestoreDefaults(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{})
	if err := d.Restore(t.Context(), "a.bin", filesystem.RestoreOptions{Days: 1}); err != nil {
		t.Fatal(err)
	}
	r := s.sent("PUT /c/a.bin", "comp=tier")
	if len(r) != 1 || r[0].Header.Get("x-ms-access-tier") != string(blob.AccessTierHot) || r[0].Header.Get("x-ms-rehydrate-priority") != string(blob.RehydratePriorityStandard) {
		t.Fatalf("restore sent %v", r)
	}
}

// TestSetTagsRefused proves a tag set Azure refuses with 400 (too many
// tags, a key too long, a character it does not allow) is
// CodeInvalidArgument, the caller's mistake, rather than CodeUnknown.
func TestSetTagsRefused(t *testing.T) {
	_, d, s := scriptedDriver(t, azure.Config{})
	s.on("PUT /c/t.txt?comp=tags", failure(http.StatusBadRequest, "InvalidTag"))
	wantCode(t, d.SetTags(t.Context(), "t.txt", map[string]string{"bad key!": "v"}), filesystem.CodeInvalidArgument)
}

// TestServiceErrorsKeepTheirCode proves the Azure-specific calls report a
// failure from the service with its code: properties for ObjectInfo and
// Hold, the legal hold request, the versions listing and a version read.
func TestServiceErrorsKeepTheirCode(t *testing.T) {
	ctx := t.Context()
	drv, d, s := scriptedDriver(t, azure.Config{Versioned: true})
	s.on("HEAD /c/gone.txt", reply{status: http.StatusNotFound, header: map[string]string{"x-ms-error-code": "BlobNotFound"}})
	_, err := drv.ObjectInfo(ctx, "gone.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.Hold(ctx, "gone.txt")
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, d.SetHold(ctx, "gone.txt", filesystem.Hold{Legal: true}), filesystem.CodeNotFound)

	s.on("HEAD /c/held.txt", reply{status: http.StatusOK, header: propsHeaders(nil)})
	s.on("PUT /c/held.txt?comp=legalhold", failure(http.StatusForbidden, "AuthorizationPermissionMismatch"))
	wantCode(t, d.SetHold(ctx, "held.txt", filesystem.Hold{Legal: true}), filesystem.CodePermission)

	s.on("GET /c?", failure(http.StatusServiceUnavailable, "ServerBusy"))
	_, err = d.Versions(ctx, "v.txt")
	wantCode(t, err, filesystem.CodeUnavailable)

	s.on("GET /c/v.txt?versionid=", reply{status: http.StatusNotFound, header: map[string]string{"x-ms-error-code": "BlobNotFound"}})
	_, err = d.ReadVersion(ctx, "v.txt", "2026-01-01T00:00:00.0000000Z")
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestVersionClientBadEndpoint proves a version call on a client whose
// endpoint does not parse as a URL (the SDK builds clients without
// checking) is CodeInvalidArgument, before any request.
func TestVersionClientBadEndpoint(t *testing.T) {
	ctx := t.Context()
	client, err := azblob.NewClientWithNoCredential("http://%zz/", nil)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := azure.New(azure.Config{Client: client, Container: "c", Versioned: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drv.ReadVersion(ctx, "v.txt", "2026-01-01T00:00:00.0000000Z")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	wantCode(t, drv.RestoreVersion(ctx, "v.txt", "2026-01-01T00:00:00.0000000Z"), filesystem.CodeInvalidArgument)
}

// TestRetainFailureAfterCommitAndCopy proves a multipart commit and a
// server-side copy whose immutability policy then fails say the blob was
// stored, keeping the cause's code, as a single-shot write does
// (TestRetainForOnEveryWrite).
func TestRetainFailureAfterCommitAndCopy(t *testing.T) {
	ctx := t.Context()
	drv, d, s := scriptedDriver(t, azure.Config{RetainFor: time.Hour})
	s.on("PUT /c/m.bin?comp=immutabilityPolicies", failure(http.StatusServiceUnavailable, "ServerBusy"))
	s.on("PUT /c/copy.txt?comp=immutabilityPolicies", failure(http.StatusServiceUnavailable, "ServerBusy"))
	s.on("GET /c/"+azure.MultipartDir+"/", reply{status: http.StatusOK, header: map[string]string{"x-ms-blob-type": "BlockBlob"}, body: `{"key":"m.bin"}`})
	s.on("HEAD /c/copy.txt", reply{status: http.StatusOK, header: propsHeaders(map[string]string{"x-ms-copy-status": "success"})})

	id, err := drv.CreateMultipart(ctx, "m.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drv.CompleteMultipart(ctx, "m.bin", id, nil)
	wantStoredButUnprotected(t, err)
	if len(s.sent("PUT /c/m.bin", "comp=blocklist")) != 1 {
		t.Fatal("the commit was not sent before the policy")
	}

	_, err = d.Copy(ctx, "src.txt", "copy.txt")
	wantStoredButUnprotected(t, err)
}

// wantStoredButUnprotected checks err is a failed immutability policy on
// a blob that was stored, with the 503's code.
func wantStoredButUnprotected(t *testing.T, err error) {
	t.Helper()
	wantCode(t, err, filesystem.CodeUnavailable)
	if !strings.Contains(err.Error(), "was stored") {
		t.Fatalf("a failed policy request does not say the blob was stored: %v", err)
	}
}
