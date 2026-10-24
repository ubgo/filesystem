// Command adapters-s3-compatible opens one disk per S3-compatible provider
// from a ready DSN and prints the capabilities each configuration declares.
//
// Opening a disk sends no request (the AWS SDK resolves credentials
// lazily), so this program runs offline: it proves the DSNs parse and set
// the intended flags, not that a provider honours them. Run the conformance
// suite against a real bucket for that (see docs/adapters/s3-compatible.md).
//
// It then opens a provider-shaped DSN against an in-process S3 (gofakes3)
// and runs a write, a read and a listing through it, and prints the DSNs
// the opener refuses.
//
//	go run ./adapters-s3-compatible            (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
)

// provider is one S3-compatible store and the DSN that fits it.
type provider struct {
	// Name is the provider, as the guide lists it.
	Name string
	// DSN opens a bucket on it. Placeholders in angle brackets are the
	// account's own values.
	DSN string
}

// providers lists a starting DSN per provider. Flags are on only where the
// driver's behaviour against that provider is documented: SeaweedFS is
// verified by task test:s3; the others rely on the provider's own S3
// compatibility documentation.
// ds:def id=providers-8brajey9
var providers = []provider{
	{"AWS S3", "s3://my-bucket?region=us-east-1&conditional_write=true"},
	{"AWS S3 with object ACLs", "s3://my-bucket?region=us-east-1&acl=true&conditional_write=true"},
	{"Cloudflare R2", "s3://my-bucket?region=auto&endpoint=https://<account-id>.r2.cloudflarestorage.com"},
	{"Backblaze B2", "s3://my-bucket?region=us-west-004&endpoint=https://s3.us-west-004.backblazeb2.com"},
	{"Wasabi", "s3://my-bucket?region=us-east-1&endpoint=https://s3.us-east-1.wasabisys.com"},
	{"DigitalOcean Spaces", "s3://my-space?region=us-east-1&endpoint=https://nyc3.digitaloceanspaces.com"},
	{"SeaweedFS", "s3://my-bucket?region=us-east-1&endpoint=http://127.0.0.1:8333&path_style=true&prune_prefixes=true&conditional_write=true"},
	{"Garage", "s3://my-bucket?region=garage&endpoint=http://127.0.0.1:3900&path_style=true"},
	{"MinIO", "s3://my-bucket?region=us-east-1&endpoint=http://127.0.0.1:9000&path_style=true"},
}

func main() {
	ctx := context.Background()
	if err := run(ctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := runFake(ctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run opens every provider's DSN and prints its declared capabilities.
// ds:def id=run-78w52jn4
func run(ctx context.Context, out io.Writer) error {
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return err
	}
	for _, p := range providers {
		disk, err := mux.Open(ctx, "media", p.DSN)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		c := disk.Capabilities()
		fmt.Fprintf(out, "%-24s visibility=%-5t conditional=%t\n", p.Name, c.SetVisibility, c.ConditionalWrite)
	}
	return nil
}
