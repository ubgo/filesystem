package s3_test

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

const testKMSKey = "arn:aws:kms:eu-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

// TestServerSideEncryption proves every write path sends the disk's
// encryption and KMS key: a plain write, a copy, a multipart upload, and
// a presigned PUT, which signs both so the client must send them.
func TestServerSideEncryption(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{SSEKMSKeyID: testKMSKey})
	want := recorded{sse: string(types.ServerSideEncryptionAwsKms), kmsKey: testKMSKey}
	check := func(what string, got []recorded) {
		t.Helper()
		if len(got) == 0 {
			t.Fatalf("%s: no request recorded", what)
		}
		for _, r := range got {
			if r.sse != want.sse || r.kmsKey != want.kmsKey {
				t.Errorf("%s %s %s sent sse %q key %q", what, r.method, r.path, r.sse, r.kmsKey)
			}
		}
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	check("write", p.sent(http.MethodPut, "x-id=PutObject"))
	if _, err := d.Copy(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	check("copy", p.sent(http.MethodPut, "x-id=CopyObject"))
	if _, err := d.CreateMultipart(ctx, "m.bin", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	check("multipart", p.sent(http.MethodPost, "uploads"))
	put, err := d.SignedURL(ctx, "direct.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(put.URL)
	signed := u.Query().Get("X-Amz-SignedHeaders")
	if !strings.Contains(signed, "x-amz-server-side-encryption") || !strings.Contains(signed, "x-amz-server-side-encryption-aws-kms-key-id") {
		t.Errorf("presigned PUT does not sign the encryption: %s", signed)
	}
	if put.Header.Get("X-Amz-Server-Side-Encryption") != want.sse || put.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != testKMSKey {
		t.Errorf("presigned PUT headers = %v", put.Header)
	}
	if status, body := get(t, http.MethodPut, put.URL, bytes.NewReader([]byte("x")), put.Header); status != http.StatusOK {
		t.Fatalf("direct upload = %d %s", status, body)
	}
	check("direct upload", p.sent(http.MethodPut, "X-Amz-Signature"))
}

// TestServerSideEncryptionConfig covers the defaults and refusals: a KMS
// key alone implies aws:kms, SSE-S3 sends no key, and a key with SSE-S3 is
// refused, from Config and from the DSN.
func TestServerSideEncryptionConfig(t *testing.T) {
	ctx := t.Context()
	d, p := newDisk(t, fss3.Config{ServerSideEncryption: types.ServerSideEncryptionAes256})
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range p.sent(http.MethodPut, "x-id=PutObject") {
		if r.sse != "AES256" || r.kmsKey != "" {
			t.Errorf("SSE-S3 write sent sse %q key %q", r.sse, r.kmsKey)
		}
	}
	client, bucket, _ := fakeS3WithProxy(t)
	if _, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, ServerSideEncryption: types.ServerSideEncryptionAes256, SSEKMSKeyID: testKMSKey}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("KMS key with SSE-S3 = %v", err)
	}
	if _, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, ServerSideEncryption: types.ServerSideEncryptionAwsKmsDsse, SSEKMSKeyID: testKMSKey}); err != nil {
		t.Errorf("KMS key with DSSE = %v", err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	if _, err := mux.Open(ctx, "s3", "s3://b?region=us-east-1&sse=AES256&sse_kms_key_id="+url.QueryEscape(testKMSKey)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("DSN with a KMS key and SSE-S3 = %v", err)
	}
	if _, err := mux.Open(ctx, "s3", "s3://b?region=us-east-1&sse_kms_key_id="+url.QueryEscape(testKMSKey)); err != nil {
		t.Errorf("DSN with a KMS key = %v", err)
	}
}
