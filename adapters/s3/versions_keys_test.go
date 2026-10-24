package s3_test

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// TestVersions proves CheckBucket refuses Versioning on an unversioned
// bucket, Versions lists newest first with delete markers, ReadVersion
// reads one, RestoreVersion brings one back, and a delete marker or an
// unknown version is CodeNotFound.
func TestVersions(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, Versioning: true})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, drv.CheckBucket(ctx), filesystem.CodeInvalidArgument)
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	if err := drv.CheckBucket(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("s3", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "doc.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := d.WriteBytes(ctx, "doc.txt.bak", []byte("other key"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, err := d.Versions(ctx, "doc.txt")
	if err != nil || len(vs) != 2 || !vs[0].Latest || vs[1].Latest || vs[1].Size != 3 || vs[0].ID == vs[1].ID {
		t.Fatalf("Versions = %+v, %v", vs, err)
	}
	first := vs[1].ID
	rc, err := d.ReadVersion(ctx, "doc.txt", first)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "one" {
		t.Fatalf("read of version = %q", b)
	}
	if err := d.RestoreVersion(ctx, "doc.txt", first); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.ReadAll(ctx, "doc.txt"); string(b) != "one" {
		t.Errorf("latest after RestoreVersion = %q", b)
	}
	if err := d.Delete(ctx, "doc.txt"); err != nil {
		t.Fatal(err)
	}
	vs, _ = d.Versions(ctx, "doc.txt")
	if len(vs) != 4 || !vs[0].DeleteMarker || !vs[0].Latest {
		t.Fatalf("Versions after delete = %+v", vs)
	}
	_, err = d.ReadVersion(ctx, "doc.txt", vs[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, d.RestoreVersion(ctx, "doc.txt", vs[0].ID), filesystem.CodeNotFound)
	_, err = d.ReadVersion(ctx, "doc.txt", "no-such-version")
	wantCode(t, err, filesystem.CodeNotFound)
	// S3 answers a delete marker with 405, which gofakes3 does not.
	p.fail(http.MethodGet, "versionId=", http.StatusMethodNotAllowed, "MethodNotAllowed")
	_, err = d.ReadVersion(ctx, "doc.txt", vs[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)
	// Restoring the version before the delete undoes it.
	if err := d.RestoreVersion(ctx, "doc.txt", vs[1].ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.ReadAll(ctx, "doc.txt"); string(b) != "one" {
		t.Errorf("after undoing the delete = %q", b)
	}

	plain, _ := newDisk(t, fss3.Config{})
	_, err = plain.ReadVersion(ctx, "doc.txt", first)
	wantCode(t, err, filesystem.CodeUnsupported)
	_, err = plain.Versions(ctx, "doc.txt")
	wantCode(t, err, filesystem.CodeUnsupported)
}

// testCustomerKey is a fixed 32-byte SSE-C key.
var testCustomerKey = []byte("0123456789abcdef0123456789abcdef")

// TestCustomerKey proves SSE-C: every request that touches an object's
// bytes sends the key (a copy as source and destination), presigned
// requests sign it, PresignPost is not declared, the key is never
// formatted, and it excludes the other encryption settings.
func TestCustomerKey(t *testing.T) {
	ctx := t.Context()
	key, err := fss3.NewCustomerKey(testCustomerKey)
	if err != nil {
		t.Fatal(err)
	}
	client, bucket, p := fakeS3WithProxy(t)
	// PresignCustomerKey: the signed requests below carry the key.
	cfg := fss3.Config{Client: client, Bucket: bucket, SSECustomerKey: key, PresignCustomerKey: true}
	drv, err := fss3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("s3", drv)
	if d.Capabilities().PresignPost {
		t.Error("PresignPost declared with an SSE-C key")
	}
	sum := md5.Sum(testCustomerKey)
	wantMD5 := base64.StdEncoding.EncodeToString(sum[:])
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("secret"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "secret" {
		t.Fatal(b, err)
	}
	if _, err := d.Copy(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateMultipart(ctx, "m.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := d.UploadPart(ctx, "m.bin", id, 1, strings.NewReader("part"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteMultipart(ctx, "m.bin", id, []filesystem.Part{part}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	all := slices.Clone(p.requests)
	p.mu.Unlock()
	checked := 0
	for _, r := range all {
		if strings.Count(strings.Trim(r.path, "/"), "/") == 0 || strings.Contains(r.query, "list-type") {
			continue // bucket-level requests
		}
		if r.header.Get("X-Amz-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString(testCustomerKey) || r.header.Get("X-Amz-Server-Side-Encryption-Customer-Key-Md5") != wantMD5 || r.header.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm") != "AES256" {
			t.Errorf("%s %s?%s did not send the SSE-C key", r.method, r.path, r.query)
		}
		if strings.Contains(r.query, "CopyObject") && r.header.Get("X-Amz-Copy-Source-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString(testCustomerKey) {
			t.Errorf("copy did not send the source key")
		}
		checked++
	}
	if checked < 8 {
		t.Fatalf("only %d object requests checked", checked)
	}
	for _, m := range []filesystem.HTTPMethod{filesystem.MethodGet, filesystem.MethodPut} {
		sr, err := d.SignedURL(ctx, "a.txt", filesystem.PresignOptions{Method: m, Expires: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(sr.URL)
		if !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "x-amz-server-side-encryption-customer-key-md5") || sr.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString(testCustomerKey) {
			t.Errorf("presigned %s does not pin the key: %v %v", m, u.Query().Get("X-Amz-SignedHeaders"), sr.Header)
		}
	}
	partID, err := d.CreateMultipart(ctx, "direct.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sr, err := d.PresignPart(ctx, "direct.bin", partID, 1, time.Minute); err != nil || sr.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString(testCustomerKey) {
		t.Errorf("presigned part does not pin the key: %v %v", sr.Header, err)
	}
	b64 := base64.StdEncoding.EncodeToString(testCustomerKey)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, v := range []any{key, cfg, drv, &cfg} {
			out := fmt.Sprintf(verb, v)
			if strings.Contains(out, string(testCustomerKey)) || strings.Contains(out, b64) || strings.Contains(out, fmt.Sprintf("%x", testCustomerKey)) {
				t.Fatalf("%s of %T leaks the key: %s", verb, v, out)
			}
		}
	}
	// Without the opt-in, an SSE-C disk signs nothing: a signed request
	// would hand out the disk-wide key.
	careful, _ := fss3.New(fss3.Config{Client: client, Bucket: bucket, SSECustomerKey: key})
	if c := careful.Capabilities(); c.Presign || c.PresignPart || c.PresignHeaders || c.PresignPost {
		t.Errorf("SSE-C disk without PresignCustomerKey declares presigning: %+v", c)
	}
	_, err = fss3.NewCustomerKey([]byte("short"))
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, SSECustomerKey: key, ServerSideEncryption: types.ServerSideEncryptionAes256})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestChecksumAlgorithm proves the disk's algorithm and a per-write one
// reach S3, Stat reports the stored checksum, ChecksumSHA256 is not
// claimed for another algorithm, and a mismatch S3 reports is
// CodeInvalidArgument.
func TestChecksumAlgorithm(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	// The SDK asks for checksums on its own by default; a client that
	// does so only when required proves the driver asks.
	client = s3.New(client.Options(), func(o *s3.Options) { o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired })
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("s3", drv)
	if d.Capabilities().ChecksumSHA256 {
		t.Error("ChecksumSHA256 declared with CRC32C")
	}
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e, _ := d.Stat(ctx, "a.txt")
	// crc32c("hello"), base64.
	if got := e.Metadata[fss3.MetaChecksumPrefix+"crc32c"]; got != "mnG7TA==" {
		t.Errorf("Stat checksum = %v", e.Metadata)
	}
	sha1 := withOpts(t, ctx, d, "b.txt", fss3.RequestOptions{ChecksumAlgorithm: types.ChecksumAlgorithmSha1})
	if _, err := d.WriteBytes(sha1, "b.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if e, _ := d.Stat(sha1, "b.txt"); e.Metadata[fss3.MetaChecksumPrefix+"sha1"] != "qvTGHdzF6KLavt4PO0gs2a6pQ00=" {
		t.Errorf("per-write checksum = %v", e.Metadata)
	}
	p.fail(http.MethodPut, "PutObject", http.StatusBadRequest, "BadDigest")
	_, err = d.WriteBytes(ctx, "c.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.WriteBytes(withOpts(t, ctx, d, "d.txt", fss3.RequestOptions{ChecksumAlgorithm: "MD5"}), "d.txt", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = fss3.New(fss3.Config{Client: client, Bucket: bucket, ChecksumAlgorithm: "XXHASH"})
	wantCode(t, err, filesystem.CodeInvalidArgument)
}

// TestOpenerAdvanced proves the DSN parameters reach the capabilities,
// that object_lock and versioning are checked against the bucket, and
// that the SSE-C key is read from the named variable, never the DSN.
func TestOpenerAdvanced(t *testing.T) {
	ctx := t.Context()
	client, bucket, p := fakeS3WithProxy(t)
	base := "s3://" + bucket + "?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(serverURL(t, p)) + "&access_key_env=FS_TEST_AK&secret_key_env=FS_TEST_SK"
	t.Setenv("FS_TEST_AK", "test")
	t.Setenv("FS_TEST_SK", "test")
	t.Setenv("FS_TEST_SSEC", base64.StdEncoding.EncodeToString(testCustomerKey))
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	d, err := mux.Open(ctx, "a", base+"&storage_classes=true&tagging=true&restore=true&restore_tier=Bulk&checksum=CRC32C&sse_c_key_env=FS_TEST_SSEC")
	if err != nil {
		t.Fatal(err)
	}
	c := d.Capabilities()
	if !c.StorageClass || !c.Tags || !c.Restore || c.Hold || c.Versions || c.Presign || c.PresignPost || c.ChecksumSHA256 {
		t.Errorf("capabilities = %+v", c)
	}
	if d, err := mux.Open(ctx, "a2", base+"&sse_c_key_env=FS_TEST_SSEC&presign_customer_key=true"); err != nil || !d.Capabilities().Presign {
		t.Errorf("presign_customer_key: %v", err)
	}
	if d, err := mux.Open(ctx, "a3", base+"&checksum=CRC32C&presign_without_checksum=true"); err != nil || !d.Capabilities().Presign {
		t.Errorf("presign_without_checksum: %v", err)
	}
	_, err = mux.Open(ctx, "b", base+"&object_lock_days=7")
	wantCode(t, err, filesystem.CodeInvalidArgument) // the bucket has no Object Lock
	_, err = mux.Open(ctx, "c", base+"&versioning=true")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	p.lockEnable()
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	d, err = mux.Open(ctx, "d", base+"&object_lock_mode=COMPLIANCE&versioning=true")
	if err != nil {
		t.Fatal(err)
	}
	if c := d.Capabilities(); !c.Hold || !c.Versions {
		t.Errorf("capabilities = %+v", c)
	}
	t.Setenv("FS_TEST_BAD", "bm90IDMyIGJ5dGVz")
	for _, q := range []string{"&object_lock=true&max_retention_days=1&object_lock_days=2", "&max_retention_days=-1", "&sse_c_key_env=FS_TEST_UNSET", "&sse_c_key_env=FS_TEST_BAD", "&object_lock_days=x", "&tagging=maybe", "&checksum=MD5", "&restore_tier=Now"} {
		_, err := mux.Open(ctx, "bad", base+q)
		wantCode(t, err, filesystem.CodeInvalidArgument)
		if err != nil && strings.Contains(err.Error(), "bm90IDMyIGJ5dGVz") {
			t.Errorf("%s: error leaks the variable's value: %v", q, err)
		}
	}
}
