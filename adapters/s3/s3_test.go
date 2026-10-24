package s3_test

import (
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/filesystemtest"
)

// fakeS3 starts an in-process S3 server (behind s3Proxy) with one bucket and
// returns a client for it.
func fakeS3(t *testing.T) (*s3.Client, string) {
	c, b, _ := fakeS3WithProxy(t)
	return c, b
}

func fakeS3WithProxy(t *testing.T) (*s3.Client, string, *s3Proxy) {
	t.Helper()
	proxy := newProxy(gofakes3.New(s3mem.New()).Server())
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	serversMu.Lock()
	servers[proxy] = srv.URL
	serversMu.Unlock()
	client := s3.New(s3.Options{
		RetryMaxAttempts: 1,
		BaseEndpoint:     aws.String(srv.URL),
		UsePathStyle:     true,
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
	const bucket = "test-bucket"
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	return client, bucket, proxy
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		client, bucket := fakeS3(t)
		drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		d, err := filesystem.NewDisk("s3", drv)
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// TestConformanceACLMode runs the suite with per-object ACLs, conditional
// writes, tags, storage classes and a versioned bucket declared, so the
// Visibility, ConditionalWrite, Tags, StorageClass and VersionRead cases
// run.
func TestConformanceACLMode(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		client, bucket := fakeS3(t)
		if _, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{Bucket: aws.String(bucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
			t.Fatal(err)
		}
		drv, err := fss3.New(fss3.Config{Client: client, Bucket: bucket, Visibility: fss3.VisibilityACL, ConditionalWrites: true, Tagging: true, StorageClasses: true, Versioning: true, PartSize: 5 << 20})
		if err != nil {
			t.Fatal(err)
		}
		d, err := filesystem.NewDisk("s3acl", drv)
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

// servers maps a proxy to its server URL, for tests that build their own
// client through the opener.
var (
	serversMu sync.Mutex
	servers   = map[*s3Proxy]string{}
)

func serverURL(t *testing.T, p *s3Proxy) string {
	t.Helper()
	serversMu.Lock()
	defer serversMu.Unlock()
	u, ok := servers[p]
	if !ok {
		t.Fatal("unknown proxy")
	}
	return u
}
