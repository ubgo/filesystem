package s3_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// realDisk opens dsn plus extra parameters under a random prefix, removed
// afterwards, and returns it with its client.
func realDisk(t *testing.T, dsn, extra string) (*filesystem.Disk, *s3.Client) {
	t.Helper()
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	prefix := "features-" + strings.ToLower(rand.Text())
	d, err := mux.Open(t.Context(), "real", dsn+extra+"&"+filesystem.ParamPrefix+"="+prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeletePrefix(context.Background(), "") })
	client, _ := filesystem.As[*s3.Client](d)
	return d, client
}

// withBucket returns dsn pointed at a new bucket created by create, and
// removes the bucket with every version afterwards.
func withBucket(t *testing.T, dsn string, create func(ctx context.Context, client *s3.Client, bucket string) error) string {
	t.Helper()
	_, client := realDisk(t, dsn, "")
	bucket := "fs-features-" + strings.ToLower(rand.Text())[:12]
	if err := create(t.Context(), client, bucket); err != nil {
		t.Skipf("the store cannot create this bucket: %v", err)
	}
	t.Cleanup(func() { emptyBucket(client, bucket) })
	scheme, rest, _ := strings.Cut(dsn, "://")
	_, query, _ := strings.Cut(rest, "?")
	return scheme + "://" + bucket + "?" + query
}

// emptyBucket deletes every version of every object in bucket (bypassing
// governance retention) and the bucket itself, best effort.
func emptyBucket(client *s3.Client, bucket string) {
	ctx := context.Background()
	p := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			break
		}
		for _, v := range out.Versions {
			_, _ = client.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{Bucket: aws.String(bucket), Key: v.Key, VersionId: v.VersionId, LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOff}})
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: v.Key, VersionId: v.VersionId, BypassGovernanceRetention: aws.Bool(true)})
		}
		for _, m := range out.DeleteMarkers {
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: m.Key, VersionId: m.VersionId})
		}
	}
	_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)})
}

func realDSN(t *testing.T) string {
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	return dsn
}

// TestTagsAgainstARealStore proves tags round-trip on the store, set on
// write and through SetTags.
func TestTagsAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	d, _ := realDisk(t, realDSN(t), "&tagging=true")
	if _, err := d.WriteBytes(withOpts(t, ctx, d, "t.txt", fss3.RequestOptions{Tags: map[string]string{"team": "a b"}}), "t.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Tags(ctx, "t.txt"); err != nil || got["team"] != "a b" {
		t.Fatalf("tags on write = %v, %v", got, err)
	}
	if err := d.SetTags(ctx, "t.txt", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Tags(ctx, "t.txt"); err != nil || len(got) != 1 || got["k"] != "v" {
		t.Fatalf("tags = %v, %v", got, err)
	}
	if err := d.SetTags(ctx, "t.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := d.Tags(ctx, "t.txt"); err != nil || len(got) != 0 {
		t.Fatalf("cleared tags = %v, %v", got, err)
	}
}

// TestChecksumAlgorithmAgainstARealStore proves the store keeps a CRC32C
// checksum and Stat reports it.
func TestChecksumAlgorithmAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	d, _ := realDisk(t, realDSN(t), "&checksum=CRC32C")
	if _, err := d.WriteBytes(ctx, "c.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Stat(ctx, "c.txt")
	if err != nil || e.Metadata[fss3.MetaChecksumPrefix+"crc32c"] != "mnG7TA==" {
		t.Fatalf("Stat = %v, %v", e.Metadata, err)
	}
}

// TestVersionsAgainstARealStore proves versions on a versioned bucket:
// listing, reading an old one, and restoring it.
func TestVersionsAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	dsn := withBucket(t, realDSN(t), func(ctx context.Context, c *s3.Client, b string) error {
		if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b)}); err != nil {
			return err
		}
		_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String(b), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
		return err
	})
	d, _ := realDisk(t, dsn, "&versioning=true")
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "v.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := d.Versions(ctx, "v.txt")
	if err != nil || len(vs) != 2 || !vs[0].Latest {
		t.Fatalf("Versions = %+v, %v", vs, err)
	}
	if e, err := d.Stat(ctx, "v.txt"); err != nil || e.Metadata[fss3.MetaVersionID] != vs[0].ID {
		t.Fatalf("Stat version = %v, %v", e.Metadata, err)
	}
	if err := d.RestoreVersion(ctx, "v.txt", vs[1].ID); err != nil {
		t.Fatal(err)
	}
	if b, err := d.ReadAll(ctx, "v.txt"); err != nil || string(b) != "one" {
		t.Fatalf("after RestoreVersion = %q, %v", b, err)
	}
	if err := d.Delete(ctx, "v.txt"); err != nil {
		t.Fatal(err)
	}
	vs, _ = d.Versions(ctx, "v.txt")
	if len(vs) != 4 || !vs[0].DeleteMarker {
		t.Fatalf("Versions after delete = %+v", vs)
	}
	_, err = d.ReadVersion(ctx, "v.txt", vs[0].ID)
	wantCode(t, err, filesystem.CodeNotFound)
}

// TestObjectLockAgainstARealStore proves holds on a bucket created with
// Object Lock: a legal hold and a governance retention both refuse
// Delete, and lifting them allows it.
func TestObjectLockAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	dsn := withBucket(t, realDSN(t), func(ctx context.Context, c *s3.Client, b string) error {
		_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b), ObjectLockEnabledForBucket: aws.Bool(true)})
		return err
	})
	d, _ := realDisk(t, dsn, "&object_lock=true")
	if _, err := d.WriteBytes(ctx, "h.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetHold(ctx, "h.txt", filesystem.Hold{Legal: true}); err != nil {
		t.Fatal(err)
	}
	if h, err := d.Hold(ctx, "h.txt"); err != nil || !h.Legal {
		t.Fatalf("Hold = %+v, %v", h, err)
	}
	wantCode(t, d.Delete(ctx, "h.txt"), filesystem.CodePermission)
	_, err := d.WriteBytes(ctx, "h.txt", []byte("overwrite"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodePermission)
	if h, err := d.Hold(ctx, "h.txt"); err != nil || !h.Legal {
		t.Fatalf("Hold after a refused overwrite = %+v, %v", h, err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := d.SetHold(ctx, "h.txt", filesystem.Hold{RetainUntil: until}); err != nil {
		t.Fatal(err)
	}
	if h, err := d.Hold(ctx, "h.txt"); err != nil || h.Legal || !h.RetainUntil.Equal(until) {
		t.Fatalf("Hold with retention = %+v, %v", h, err)
	}
	wantCode(t, d.Delete(ctx, "h.txt"), filesystem.CodePermission)
	if err := d.SetHold(ctx, "h.txt", filesystem.Hold{}); err != nil {
		var api interface{ ErrorCode() string }
		if errors.As(err, &api) && api.ErrorCode() == "InvalidRequest" {
			// SeaweedFS refuses the empty retention S3 uses to
			// remove a governance retention.
			t.Skipf("the store cannot remove a governance retention: %v", err)
		}
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "h.txt"); err != nil {
		t.Fatal(err)
	}
}

// TestCustomerKeyAgainstARealStore proves an SSE-C object round-trips and
// is refused without the key, where the store implements SSE-C.
func TestCustomerKeyAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	dsn := realDSN(t)
	t.Setenv("FS_TEST_SSEC_KEY", base64.StdEncoding.EncodeToString(testCustomerKey))
	d, client := realDisk(t, dsn, "&sse_c_key_env=FS_TEST_SSEC_KEY")
	if _, err := d.WriteBytes(ctx, "secret.txt", []byte("hidden"), filesystem.WriteOptions{}); err != nil {
		var api interface{ ErrorCode() string }
		if errors.As(err, &api) && api.ErrorCode() == "NotImplemented" {
			t.Skipf("the store does not implement SSE-C: %v", err)
		}
		t.Fatal(err)
	}
	if b, err := d.ReadAll(ctx, "secret.txt"); err != nil || string(b) != "hidden" {
		t.Fatalf("read with the key = %q, %v", b, err)
	}
	e, err := d.Stat(ctx, "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	full := e.Key
	bucket := strings.TrimPrefix(strings.SplitN(dsn, "?", 2)[0], fss3.Scheme+"://")
	page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("features-")})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range page.Contents {
		if strings.HasSuffix(aws.ToString(o.Key), "/"+full) {
			if _, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: o.Key}); err == nil {
				t.Error("the store served an SSE-C object without the key")
			}
			return
		}
	}
	t.Fatal("written object not listed")
}
