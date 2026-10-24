package main

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// The bucket the in-process fake holds, and the environment variables a
// provider DSN names for its keys (one pair per provider, so two providers
// can share a process).
const (
	demoBucket   = "media"
	accessKeyEnv = "R2_ACCESS_KEY_ID"
	secretKeyEnv = "R2_SECRET_ACCESS_KEY"
)

// runFake opens a provider-shaped DSN against an in-process S3 (gofakes3)
// and runs a write, a read and a listing through it, then shows the DSNs
// the opener refuses. It stands in for a real provider: only the endpoint
// differs.
func runFake(ctx context.Context, out io.Writer) error {
	backend := s3mem.New()
	if err := backend.CreateBucket(demoBucket); err != nil {
		return err
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	for env, value := range map[string]string{accessKeyEnv: "demo-key", secretKeyEnv: "demo-secret"} {
		if err := os.Setenv(env, value); err != nil {
			return err
		}
	}
	if err := roundTrip(ctx, out, srv.URL); err != nil {
		return err
	}
	refusedDSNs(ctx, out)
	return nil
}

// providerDSN is the DSN of the fake's bucket, built the way a provider's
// is: an endpoint, path-style addressing for a host that is an IP, and
// keys read from the variables the DSN names. conditional_write stays off,
// as it should on any provider not checked to honour If-None-Match.
// ds:def id=providerdsn-m32nvg78
func providerDSN(endpoint string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, "auto")
	q.Set(fss3.ParamEndpoint, endpoint)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamAccessKeyEnv, accessKeyEnv)
	q.Set(fss3.ParamSecretKeyEnv, secretKeyEnv)
	return fss3.Scheme + "://" + demoBucket + "?" + q.Encode()
}

// roundTrip opens the provider DSN and stores, reads back and lists one
// object, then asks for a create-only write the DSN did not declare.
// ds:def id=roundtrip-866uha3a
func roundTrip(ctx context.Context, out io.Writer, endpoint string) error {
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return err
	}
	disk, err := mux.Open(ctx, "media", providerDSN(endpoint))
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	if _, err := disk.Write(ctx, "photos/cat.txt", strings.NewReader("meow"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	b, err := disk.ReadAll(ctx, "photos/cat.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "round trip: read back %q\n", b)
	for entry, err := range disk.List(ctx, "photos", filesystem.ListOptions{}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s %d bytes\n", entry.Key, entry.Size)
	}
	_, err = disk.Write(ctx, "photos/cat.txt", strings.NewReader("woof"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "if-not-exists without conditional_write: %s\n", filesystem.CodeOf(err))
	return nil
}

// refusedDSNs opens DSNs the S3 opener refuses before any request: keys
// in the URL, half a key pair, a key variable that is not set, and a flag
// that is not a boolean.
// ds:def id=refuseddsns-wayhqjk7
func refusedDSNs(ctx context.Context, out io.Writer) {
	mux := filesystem.NewMux()
	_ = mux.Register(fss3.Scheme, fss3.Opener)
	for _, c := range []struct{ why, dsn string }{
		{"keys in the URL", "s3://AKIAEXAMPLE:secret@my-bucket?region=auto"},
		{"access_key_env alone", "s3://my-bucket?region=auto&access_key_env=" + accessKeyEnv},
		{"unset key variable", "s3://my-bucket?region=auto&access_key_env=NOT_SET_ANYWHERE&secret_key_env=" + secretKeyEnv},
		{"path_style=yes", "s3://my-bucket?region=auto&path_style=yes"},
	} {
		_, err := mux.Open(ctx, "refused", c.dsn)
		fmt.Fprintf(out, "refused DSN (%s): %s\n", c.why, filesystem.CodeOf(err))
	}
}
