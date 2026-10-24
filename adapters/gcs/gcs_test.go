package gcs_test

import (
	"crypto/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"google.golang.org/api/option"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gcs"
	"github.com/ubgo/filesystem/filesystemtest"
)

// fakeServer starts an in-process fake GCS for the test.
func fakeServer(t *testing.T) *fakestorage.Server {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http", NoListener: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return srv
}

func newBucket(t *testing.T, srv *fakestorage.Server) string {
	t.Helper()
	name := "b" + strings.ToLower(rand.Text())
	srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: name})
	return name
}

// conformanceFake returns a client on a fake GCS whose rewrites honour a
// storage class (see proxy.classRewrites), for the StorageClass case. The
// client keeps the SDK's default endpoints, so reads go through the XML
// API and gzip objects read back as stored; every request is sent to the
// proxy with its original Host, which the fake routes on.
func conformanceFake(t *testing.T) (*storage.Client, *fakestorage.Server) {
	t.Helper()
	_, fake, p, base := proxied(t)
	p.classRewrites = true
	proxyURL, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	via := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = r.URL.Host
		r.URL.Scheme, r.URL.Host = proxyURL.Scheme, proxyURL.Host
		return http.DefaultTransport.RoundTrip(r)
	})
	client, err := storage.NewClient(t.Context(), option.WithHTTPClient(&http.Client{Transport: via}), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return client, fake
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// conformanceOptions names a GCS class for the StorageClass case.
var conformanceOptions = filesystemtest.Options{StorageClass: string(gcs.ClassNearline)}

func TestConformance(t *testing.T) {
	client, fake := conformanceFake(t)
	filesystemtest.RunWith(t, func(t *testing.T) *filesystem.Disk {
		return disk(t, gcs.Config{Client: client, Bucket: newBucket(t, fake)})
	}, conformanceOptions)
}

// TestConformanceVersioned runs the suite on a versioned bucket, so the
// Versions and VersionRead cases run too.
func TestConformanceVersioned(t *testing.T) {
	client, fake := conformanceFake(t)
	filesystemtest.RunWith(t, func(t *testing.T) *filesystem.Disk {
		name := "v" + strings.ToLower(rand.Text())
		fake.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: name, VersioningEnabled: true})
		return disk(t, gcs.Config{Client: client, Bucket: name, Versioned: true})
	}, conformanceOptions)
}
