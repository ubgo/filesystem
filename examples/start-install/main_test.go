package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// testBucket is the bucket created on the in-process S3 fake.
const testBucket = "install-test"

// TestFirstDisk writes and reads through a Disk built without a DSN.
func TestFirstDisk(t *testing.T) {
	disk, err := firstDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disk.Close() }()
	if err := disk.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestSameCodeEveryBackend runs the same function on a directory, on
// memory and on S3 (an in-process fake); only the DSN differs.
func TestSameCodeEveryBackend(t *testing.T) {
	dsns := map[string]string{
		"local":  "file://" + t.TempDir(),
		"memory": "mem://",
		"s3":     fakeS3(t),
	}
	for name, dsn := range dsns {
		var out strings.Builder
		if err := run(t.Context(), dsn, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out.String(), `read back "hello, storage"`) {
			t.Errorf("%s: %s", name, out.String())
		}
	}
	if err := run(t.Context(), "s3://bucket?regoin=auto", &strings.Builder{}); err == nil {
		t.Error("a misspelt DSN parameter was accepted")
	}
}

// fakeS3 starts an S3 fake with an empty bucket and returns its DSN.
// Credentials come from the environment, as they would in production.
// ds:def id=install-fakes3-wu6urkkk
func fakeS3(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	t.Cleanup(srv.Close)
	client := awss3.New(awss3.Options{BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("a", "b", "")})
	if _, err := client.CreateBucket(t.Context(), &awss3.CreateBucketInput{Bucket: aws.String(testBucket)}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "b")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	return "s3://" + testBucket + "?region=us-east-1&path_style=true&endpoint=" + url.QueryEscape(srv.URL)
}
