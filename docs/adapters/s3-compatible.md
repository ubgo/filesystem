# S3-compatible stores

Many storage providers speak the S3 API, and the [S3 driver](s3.md) works with all of them through the same code: only the DSN changes. They differ in the details that matter for correctness: whether they honour conditional writes, whether objects can have their own ACL, and how they list "directories". This page gives a starting DSN for each provider and what is known about it.

## Which provider

All of them run the same driver, so the choice is about the provider, not the code. A short guide from what the notes below record:

| You want | Look at |
|---|---|
| Everything the driver offers, including object ACLs, conditional writes and presigned multipart parts | AWS S3 |
| Public media with no egress fees | Cloudflare R2, Backblaze B2, Wasabi |
| A simple managed bucket next to DigitalOcean droplets | DigitalOcean Spaces |
| A self-hosted store, for development, tests or on-premises | SeaweedFS or RustFS (both tested here), Garage |
| Files next to a Supabase database | [Supabase Storage](supabase.md) |
| An existing MinIO deployment | MinIO (its server repository is archived) |

## Before you start

- Read [s3.md](s3.md) first: every parameter, credential rule and behaviour there applies here.
- Credentials are the provider's access key pair, given to the AWS SDK as `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, or per disk through `access_key_env` and `secret_key_env`, which name the variables holding them (so an R2 disk and an AWS disk can share a process). The keys themselves never go in the DSN.
- The program [`examples/adapters-s3-compatible`](../../examples/adapters-s3-compatible/) opens every DSN on this page and prints the capabilities it declares; its test fails if a DSN here stops parsing. Opening sends no request, so that part proves the DSNs are well-formed and set the intended flags, not that a provider behaves as described. It then opens a provider-shaped DSN against an in-process S3 (gofakes3) and runs a write, a read and a listing through it, so `go run ./adapters-s3-compatible` from `examples/` needs no account.

## Step 1: Pick the DSN for your provider

Start from the provider's line below; placeholders in angle brackets are your account's own values. The flags are on only where the driver's behaviour against that provider is documented.

<!-- ds:block id=providers-8brajey9 -->
**providers** · [`examples/adapters-s3-compatible/main.go:40-50`](../../examples/adapters-s3-compatible/main.go#L40-L50)

```go
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
```
<!-- /ds:block hash=641159 -->

Check: from `examples/`, `AWS_ACCESS_KEY_ID=x AWS_SECRET_ACCESS_KEY=y go run ./adapters-s3-compatible` prints these lines first (the rest is in [Try it](#try-it)):

```text
AWS S3                   visibility=false conditional=true
AWS S3 with object ACLs  visibility=true  conditional=true
Cloudflare R2            visibility=false conditional=false
Backblaze B2             visibility=false conditional=false
Wasabi                   visibility=false conditional=false
DigitalOcean Spaces      visibility=false conditional=false
SeaweedFS                visibility=false conditional=true
Garage                   visibility=false conditional=false
MinIO                    visibility=false conditional=false
```

## Step 2: Give each disk its own keys

`access_key_env` and `secret_key_env` name the environment variables holding a provider's key pair, so the keys stay out of the DSN and two providers can share a process. `path_style=true` is needed whenever the endpoint's host is `localhost` or an IP, as with the stand-in server here. `conditional_write` stays off, as it should on any provider not checked to honour `If-None-Match`:

<!-- ds:block id=providerdsn-m32nvg78 -->
**providerDSN** · [`examples/adapters-s3-compatible/roundtrip.go:56-64`](../../examples/adapters-s3-compatible/roundtrip.go#L56-L64)

```go
func providerDSN(endpoint string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, "auto")
	q.Set(fss3.ParamEndpoint, endpoint)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamAccessKeyEnv, accessKeyEnv)
	q.Set(fss3.ParamSecretKeyEnv, secretKeyEnv)
	return fss3.Scheme + "://" + demoBucket + "?" + q.Encode()
}
```
<!-- /ds:block hash=41ae87 -->

## Step 3: Open the disk and use it

Register the S3 opener once and open the DSN. From here the disk is the same as on AWS: only the endpoint differs. The example stores, reads back and lists an object, then asks for a create-only write the DSN did not declare:

<!-- ds:block id=roundtrip-866uha3a -->
**roundTrip** · [`examples/adapters-s3-compatible/roundtrip.go:69-96`](../../examples/adapters-s3-compatible/roundtrip.go#L69-L96)

```go
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
```
<!-- /ds:block hash=cee259 -->

It prints `round trip: read back "meow"`, `listed: photos/cat.txt 4 bytes` and `if-not-exists without conditional_write: unsupported`: with the flag off, `IfNotExists` is refused before any request rather than becoming a silent overwrite.

## Step 4: Set the flags that differ between providers

- `conditional_write=true` declares that a PUT with `If-None-Match: *` fails when the key exists. On a provider that ignores the header, `IfNotExists` would silently overwrite, so the flag is off by default and every DSN below leaves it off unless the provider is known to honour it.
- `acl=true` declares per-object `public-read` / `private` ACLs. Most providers decide visibility per bucket, so it is off everywhere except AWS buckets that still allow ACLs. Without it, keep public and private files in two buckets (see [public and private files](../concepts/public-and-private.md)).
- `path_style=true` addresses `endpoint/bucket/key` instead of `bucket.endpoint/key`. Self-hosted stores on `localhost` or a bare IP need it.
- `prune_prefixes=true` hides "directories" with no objects left in them, for stores that keep them (SeaweedFS).

## Step 5: Verify a flag before you depend on it

| Provider | How the driver is checked against it |
|---|---|
| SeaweedFS | `task test:s3` runs the full conformance suite, plus the pinned-headers check, against a throwaway SeaweedFS container, with `prune_prefixes=true` and `conditional_write=true` |
| RustFS | `task test:rustfs` runs the full conformance suite with `conditional_write=true`, plus the check that a presigned PUT pins its stored headers, against a throwaway RustFS container (verified 2026-10-05) |
| AWS S3 | The driver follows the S3 API; the conformance suite is not part of the repository's gate against AWS itself |
| R2, B2, Wasabi, Spaces, Garage, MinIO | Not tested here. The notes below rely on each provider's own S3-compatibility documentation |

Before you depend on a flag with a provider that is not tested here, run the conformance suite against a throwaway bucket on it:

```sh
cd adapters/s3
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…
FILESYSTEM_S3_TEST_DSN='s3://scratch-bucket?region=auto&endpoint=https://<account-id>.r2.cloudflarestorage.com&conditional_write=true' \
  go test -count=1 -run TestConformanceAgainstARealStore -v ./...
```

Each case runs under a random prefix that is deleted afterwards. If the `ConditionalWrite` cases fail, the provider ignores `If-None-Match` and the flag must stay off.

## Step 6: Check what the opener refuses

The opener refuses a DSN that would leak or half-configure the keys, and a flag that does not parse, before any request:

<!-- ds:block id=refuseddsns-wayhqjk7 -->
**refusedDSNs** · [`examples/adapters-s3-compatible/roundtrip.go:102-114`](../../examples/adapters-s3-compatible/roundtrip.go#L102-L114)

```go
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
```
<!-- /ds:block hash=be736a -->

All four print `invalid_argument`. Unknown parameters are refused too, so a typo in a flag name fails at open time instead of being ignored.

## Provider notes

### AWS S3

```text
s3://my-bucket?region=eu-west-1&conditional_write=true
```

- Region is required for AWS (or `AWS_REGION` in the environment); no endpoint.
- AWS honours `If-None-Match: *` on PUT since 2024, which is why `conditional_write=true` is in the DSN; the driver's doc comment lists it.
- `acl=true` only for buckets whose object ownership allows ACLs. New buckets have ACLs disabled ("bucket owner enforced") and reject ACL requests.
- Every capability, including presigned parts for browser multipart uploads.

### Cloudflare R2

```text
s3://my-bucket?region=auto&endpoint=https://<account-id>.r2.cloudflarestorage.com
```

- `region=auto` is R2's region for SigV4. The endpoint carries your account ID; for a bucket in a jurisdiction, R2's documentation gives a jurisdiction-specific endpoint.
- Keys come from an R2 API token (its access key ID and secret).
- No per-object ACLs: a bucket is made public through R2's public bucket or custom domain settings. Use `visibility=public&public_url=https://<your-domain>` on the public bucket's DSN.
- Conditional writes: the driver's doc comment lists R2 among the providers that honour `If-None-Match`, but it is not tested here. Run the conformance suite against an R2 bucket before adding `conditional_write=true`.
- R2 charges no egress, which makes it a common choice for public media behind its own CDN.

### Backblaze B2

```text
s3://my-bucket?region=us-west-004&endpoint=https://s3.us-west-004.backblazeb2.com
```

- The region is the part of the bucket's S3 endpoint after `s3.`, shown in the B2 console (for example `us-west-004` or `eu-central-003`).
- Keys come from a B2 application key (key ID as the access key ID).
- Do not set `conditional_write=true`: reports from other S3 clients state that B2 rejects PUT requests carrying `If-None-Match`. With the flag off, `IfNotExists` is refused with `CodeUnsupported` before any request.
- Visibility is per bucket (public or private).

### Wasabi

```text
s3://my-bucket?region=us-east-1&endpoint=https://s3.us-east-1.wasabisys.com
```

- The endpoint follows the bucket's region: `s3.<region>.wasabisys.com`.
- Conditional writes and object ACLs are not declared; check Wasabi's documentation and run the conformance suite before turning either on.

### DigitalOcean Spaces

```text
s3://my-space?region=us-east-1&endpoint=https://nyc3.digitaloceanspaces.com
```

- The datacenter is chosen by the endpoint (`nyc3`, `ams3`, `sgp1`, ...). DigitalOcean's own AWS SDK examples set the SDK region to `us-east-1` while the endpoint selects the datacenter; this DSN follows them.
- Leave `path_style` off: the SDK then addresses `my-space.nyc3.digitaloceanspaces.com`.
- Spaces documents per-object `public-read` / `private` ACLs, so `acl=true` may suit it; not tested here.
- The Spaces CDN serves public objects from `https://<space>.<region>.cdn.digitaloceanspaces.com`; use that as `public_url` on a public disk.

### SeaweedFS

```text
s3://my-bucket?region=us-east-1&endpoint=http://127.0.0.1:8333&path_style=true&prune_prefixes=true&conditional_write=true
```

- This is one of the two stores the repository tests against (`task test:s3`, the `chrislusf/seaweedfs` image with `server -s3` and an identity file so it verifies signatures, port 8333). Checked on 2026-10-03: it honours `If-None-Match` and keeps an empty "directory" after its last object is deleted, which `prune_prefixes=true` hides.
- Without `prune_prefixes`, a non-recursive listing keeps showing directories that hold no files; the cost of the flag is one extra list request per directory in a non-recursive listing.
- Keys are whatever the SeaweedFS S3 configuration defines; the test container accepts any.

### RustFS

```text
s3://my-bucket?region=us-east-1&endpoint=http://localhost:9000&path_style=true&conditional_write=true
```

- RustFS is an S3-compatible server written in Rust, a replacement for MinIO's archived server. Use path-style addressing.
- Conditional writes work: RustFS refuses a PUT with `If-None-Match: *` when the key exists, so `conditional_write=true` is safe.
- It verifies SigV4 signatures, so a presigned PUT keeps `presign_headers` at its default: a PUT that leaves out a signed header is refused.
- An empty "directory" disappears with its last object, so `prune_prefixes` is not needed.
- Per-object ACLs are not verified: the suite ran without `acl=true`, so visibility stays bucket-wide.

### Garage

```text
s3://my-bucket?region=garage&endpoint=http://127.0.0.1:3900&path_style=true
```

- `garage` is Garage's default `s3_region`, and 3900 its default S3 API port; use the values from your `garage.toml`.
- Keys are created with `garage key create` and given access to the bucket with `garage bucket allow`.
- No ACLs (Garage decides access per key and bucket, and serves public websites per bucket). Conditional writes are not declared; run the conformance suite before turning them on.

### MinIO

```text
s3://my-bucket?region=us-east-1&endpoint=http://127.0.0.1:9000&path_style=true
```

- The MinIO server repository was archived on 2026-04-25, so it is not the store this project tests with; existing MinIO deployments still work through this driver.
- The driver's doc comment says recent MinIO honours `If-None-Match`; check yours with the conformance suite before adding `conditional_write=true`.
- Visibility is per bucket policy.

## Try it

```sh
cd examples
AWS_ACCESS_KEY_ID=x AWS_SECRET_ACCESS_KEY=y go run ./adapters-s3-compatible
go test ./adapters-s3-compatible
```

After the nine provider lines of Step 1 it prints:

```text
round trip: read back "meow"
listed: photos/cat.txt 4 bytes
if-not-exists without conditional_write: unsupported
refused DSN (keys in the URL): invalid_argument
refused DSN (access_key_env alone): invalid_argument
refused DSN (unset key variable): invalid_argument
refused DSN (path_style=yes): invalid_argument
```

The test checks these lines and the provider lines.

## Pitfalls

- **Turning on `conditional_write` on trust.** On a provider that ignores `If-None-Match: *`, `IfNotExists` silently overwrites, which is worse than the `CodeUnsupported` you get with the flag off. Turn it on only after the conformance suite passes against that provider (Step 5).
- **`acl=true` on a bucket that refuses ACLs.** In ACL mode every write carries an ACL (`private` or `public-read`), and new AWS buckets have ACLs disabled while most other providers have none, so writes are rejected. Decide visibility per bucket instead.
- **Forgetting `path_style=true` for self-hosted stores.** With an endpoint on `localhost` or an IP, virtual-hosted addressing puts the bucket in the host name, which does not resolve.
- **Empty "directories" that stay.** SeaweedFS keeps a prefix after its last object is deleted, so non-recursive listings show directories with nothing in them until you set `prune_prefixes=true`.
- **Keys in the URL, half a pair, or an unset variable** are refused at open time (Step 6); a wrong key or region is not, since opening sends no request, and shows up on the first operation.
- **One provider's quirks are not another's.** A flag that is safe on SeaweedFS or AWS says nothing about R2, B2 or Wasabi; the table in Step 5 records what is tested here and what is not.
- **The region is not optional.** SigV4 signs with it: R2 uses `auto`, Garage its configured `s3_region`, B2 the part of the endpoint after `s3.`. A wrong region can fail the first request with a signature error rather than at open time.

## Related

- [Amazon S3](s3.md): every parameter and behaviour of the driver.
- [Supabase Storage](supabase.md): a provider with an endpoint path, step by step.
- [Public CDN](../recipes/public-cdn.md): a public bucket behind a CDN with `public_url`.
- [Move to S3](../recipes/move-to-s3.md): copying an existing store to one of these providers.
- [DSN reference](../reference/dsn.md) and [comparison of every backend](README.md).
