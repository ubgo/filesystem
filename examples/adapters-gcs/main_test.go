package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun walks the whole program against a fresh fake GCS and checks the
// lines the guide quotes.
func TestRun(t *testing.T) {
	srv, err := startFakeGCS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	var out bytes.Buffer
	if err := run(t.Context(), &out, srv); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"capabilities: [copy,set-visibility,presign,presign-post,metadata,content-type,headers,presign-headers,presign-visibility,conditional-write,hold,storage-class,compare-and-swap]",
		"avatars/u1.png: image/png, public",
		"invoices/42.pdf: application/pdf, private",
		"after SetVisibility: private",
		"signed GET: algorithm=GOOG4-RSA-SHA256 disposition=attachment",
		"signed PUT: client must send Content-Type: image/png",
		"identical=true",
		`cache-control="private, max-age=60"`,
		"multipart: unsupported",
		"after DeletePrefix: exists=false",
		"matching SHA-256: stored",
		"wrong SHA-256: invalid_argument, stored=false",
		"GCS CRC32C matches local=true, MD5 matches local=true, generation set=true",
		"via DSN: [copy,metadata,content-type,headers,conditional-write,hold,storage-class,compare-and-swap]",
		"storage class: NEARLINE",
		`versions: 2, older reads "draft", after RestoreVersion the live object reads "draft"`,
		"retention: mode Unlocked on write, extended to 30 days: true",
		"signed GET override: response-content-type=text/plain",
		"kms: presigned PUT signs the key: true, client sends " + kmsKeyName,
		"per call: invoices/42.pdf stored as NEARLINE", "per call: thumbs/42.png stored as STANDARD",
		"per call: presigned PUT signs the key true and the class true, client sends " + tenantKMSKey,
		"per call: unknown class invalid_argument, stored false",
		`csek: read "for your eyes only", key printed as gcs.CustomerKey{[REDACTED]}, presign declared: false`,
		"lifecycle: Delete,SetStorageClass,AbortIncompleteMultipartUpload; logs/ move to ARCHIVE after 90 days",
		"replication: ASYNC_TURBO",
		"delegated media/users/42 with inRole:roles/storage.objectViewer,inRole:roles/storage.objectCreator",
		`token prints as: gcs.ScopedToken{AccessToken: [REDACTED], Bucket: "media", Prefix: "users/42"}`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
