package main

import (
	"context"
	"fmt"
	"io"
	"net/url"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// bucketScheme is the DSN scheme of the demo's stand-in for S3: each host
// is one bucket in one region. With a real backend this would be "s3"
// and the host a real bucket name.
const bucketScheme = "bucket"

// tenantConfig is what an application keeps per tenant, typically in its
// tenants table: where the tenant's files must live and the domain its
// links are served from. Neither is the library's decision.
type tenantConfig struct {
	// Name is the tenant's slug, used as its disk name and key prefix.
	Name string
	// Bucket is the bucket in the region the tenant's data must stay in.
	Bucket string
	// Domain is the tenant's own CDN domain for public links.
	Domain string
}

// tenantConfigs are the demo's two tenants: one whose data must stay in
// the EU and one in the US, each with its own domain.
var tenantConfigs = []tenantConfig{
	{Name: "acme", Bucket: "files-eu-west-1", Domain: "https://files.acme.example"},
	{Name: "globex", Bucket: "files-us-east-1", Domain: "https://cdn.globex.example"},
}

// regionalMux returns a Mux whose bucket scheme opens one shared driver
// per bucket, as two DSNs naming the same S3 bucket reach the same bytes.
// buckets is filled in as the demo opens them, so it can show where each
// tenant's files landed.
func regionalMux(buckets map[string]*memory.Driver) (*filesystem.Mux, error) {
	mux := filesystem.NewMux()
	err := mux.Register(bucketScheme, func(_ context.Context, u *url.URL) (filesystem.Driver, error) {
		if buckets[u.Host] == nil {
			buckets[u.Host] = memory.New()
		}
		return buckets[u.Host], nil
	})
	return mux, err
}

// openTenant builds one tenant's disk from configuration: the bucket in
// the tenant's region and the tenant's prefix inside it from the DSN, and
// the tenant's domain as the base of its public URLs. The domain is passed
// as an option rather than the DSN's public_url, because the tenant's CDN
// serves the prefix itself (its origin is the bucket plus the prefix), so
// the prefix must not appear in the URL a second time.
// ds:def id=opentenant-wujwfu92
func openTenant(ctx context.Context, mux *filesystem.Mux, t tenantConfig) (*filesystem.Disk, error) {
	q := url.Values{filesystem.ParamPrefix: {filesystem.JoinKey(tenantsDir, t.Name)}}
	dsn := bucketScheme + "://" + t.Bucket + "?" + q.Encode()
	return mux.Open(ctx, t.Name, dsn,
		filesystem.WithPublicURL(t.Domain),
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
}

// regionsAndDomains opens every tenant's disk into one Manager, writes a
// logo for each, and shows that each file landed in its tenant's regional
// bucket under its prefix, with a public URL on the tenant's own domain.
// ds:def id=regionsanddomains-mf34m22t
func regionsAndDomains(ctx context.Context, out io.Writer) error {
	buckets := map[string]*memory.Driver{}
	mux, err := regionalMux(buckets)
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager()
	if err != nil {
		return err
	}
	for _, t := range tenantConfigs {
		d, err := openTenant(ctx, mux, t)
		if err != nil {
			return err
		}
		if err := disks.Add(d); err != nil {
			return err
		}
	}
	for _, t := range tenantConfigs {
		d, err := disks.Disk(t.Name)
		if err != nil {
			return err
		}
		if _, err := d.WriteBytes(ctx, "logo.png", []byte(t.Name), filesystem.WriteOptions{}); err != nil {
			return err
		}
		link, err := d.PublicURL(ctx, "logo.png")
		if err != nil {
			return err
		}
		stored, err := buckets[t.Bucket].Stat(ctx, filesystem.JoinKey(tenantsDir, t.Name, "logo.png"))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: stored in %s as %s, served at %s\n", t.Name, t.Bucket, stored.Key, link)
	}
	return nil
}
