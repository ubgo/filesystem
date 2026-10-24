package s3_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// realStore opens the store named by EnvTestDSN, skipping without one, and
// returns its client and bucket.
func realStore(t *testing.T) (*s3.Client, string, string) {
	t.Helper()
	dsn := os.Getenv(EnvTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run against a real S3-compatible store", EnvTestDSN)
	}
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		t.Fatal(err)
	}
	ensureBucket(t, mux, dsn)
	d, err := mux.Open(t.Context(), "cfg", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := filesystem.As[*s3.Client](d)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return client, u.Host, u.Query().Get("endpoint")
}

// unimplemented reports whether err is a store's "not implemented"
// answer, so a feature the store lacks is reported as a skip, never as a
// pass.
func unimplemented(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && slices.Contains([]string{"NotImplemented", "InvalidAction", "MethodNotAllowed"}, api.ErrorCode())
}

// TestLifecycleAgainstARealStore proves the lifecycle rules round-trip on
// a real store.
func TestLifecycleAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	client, bucket, _ := realStore(t)
	want := []fss3.LifecycleRule{{ID: "tmp", Prefix: "tmp/", ExpireAfterDays: 1}, {ID: "uploads", Prefix: "uploads/", AbortIncompleteUploadsAfterDays: 7}}
	if err := fss3.SetLifecycle(ctx, client, bucket, want...); unimplemented(err) {
		t.Skipf("the store has no lifecycle configuration: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteBucketLifecycle(context.Background(), &s3.DeleteBucketLifecycleInput{Bucket: aws.String(bucket)})
	})
	got, err := fss3.Lifecycle(ctx, client, bucket)
	if err != nil || !slices.EqualFunc(got, want, lifecycleEqual) {
		t.Fatalf("read back %+v, %v", got, err)
	}
}

// TestDelegatePrefixAgainstARealStore proves, on a store with STS
// AssumeRole and session policies (MinIO-compatible stores answer it at
// the S3 endpoint), that the delegated credentials write under the prefix
// and are denied outside it, including the sibling prefix that shares
// its name ("users/42" against "users/420").
func TestDelegatePrefixAgainstARealStore(t *testing.T) {
	ctx := t.Context()
	client, bucket, endpoint := realStore(t)
	stsClient := sts.New(sts.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), Credentials: client.Options().Credentials})
	prefix := "delegate-" + strings.ToLower(rand.Text()) + "/users/42"
	creds, err := fss3.DelegatePrefix(ctx, stsClient, fss3.DelegateOptions{Role: "arn:aws:iam::000000000000:role/delegate", SessionName: "test", Bucket: bucket, Prefix: prefix, Write: true, Duration: fss3.MinDelegateDuration})
	if err != nil {
		t.Skipf("the store does not issue STS session credentials: %v", err)
	}
	scoped := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: creds.Provider()})
	put := func(key string) error {
		_, err := scoped.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("x")})
		return err
	}
	if err := put(prefix + "/a.txt"); err != nil {
		t.Fatalf("write inside the prefix: %v", err)
	}
	for _, outside := range []string{prefix + "0/a.txt", "other/a.txt", strings.TrimSuffix(prefix, "/42") + "/a.txt"} {
		if err := put(outside); err == nil {
			t.Errorf("write to %s outside the prefix succeeded", outside)
		}
	}
	if time.Until(creds.Expires) <= 0 {
		t.Errorf("credentials already expired: %v", creds.Expires)
	}
}
