package gcs_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
)

const testKMSKey = "projects/p/locations/eu/keyRings/r/cryptoKeys/files"

// TestKMSKey proves a disk with a KMS key names it on every write, copy
// and presigned PUT (signed, so the client must send it).
func TestKMSKey(t *testing.T) {
	ctx := t.Context()
	client, fake, p, _ := proxied(t)
	d := disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake), KMSKeyName: testKMSKey, Signing: &storage.SignedURLOptions{GoogleAccessID: "svc@example.iam.gserviceaccount.com", PrivateKey: pemKey(t)}})
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	q := "kmsKeyName=" + url.QueryEscape(testKMSKey)
	if len(p.sent("POST", "uploadType=", q)) == 0 {
		t.Error("write did not name the KMS key")
	}
	if _, err := d.Copy(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "rewriteTo", "destinationKmsKeyName="+url.QueryEscape(testKMSKey))) == 0 {
		t.Error("copy did not name the KMS key")
	}
	put, err := d.SignedURL(ctx, "direct.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	if !strings.Contains(u.Query().Get("X-Goog-SignedHeaders"), "x-goog-encryption-kms-key-name") || put.Header.Get("X-Goog-Encryption-Kms-Key-Name") != testKMSKey {
		t.Errorf("presigned PUT = %s %v", put.URL, put.Header)
	}
}

// TestKMSKeyFromDSN proves kms_key reaches the writes of a disk opened
// from a DSN.
func TestKMSKeyFromDSN(t *testing.T) {
	ctx := t.Context()
	_, fake, p, base := proxied(t)
	bucket := newBucket(t, fake)
	mux := filesystem.NewMux()
	_ = mux.Register(gcs.Scheme, gcs.Opener)
	d, err := mux.Open(ctx, "gcs", "gs://"+bucket+"?no_auth=true&endpoint="+url.QueryEscape(base+"/storage/v1/")+"&kms_key="+url.QueryEscape(testKMSKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.sent("POST", "kmsKeyName="+url.QueryEscape(testKMSKey))) == 0 {
		t.Error("write did not name the DSN's KMS key")
	}
}
