# S3-compatible providers

One starting DSN per S3-compatible store (AWS S3, Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces, SeaweedFS, Garage, MinIO), each opened through the S3 driver to print the capabilities that configuration declares, then a provider-shaped DSN run against an in-process S3 (gofakes3) for a write, a read and a listing, and the DSNs the opener refuses. It answers "what DSN do I use for my provider, and which flags are safe on it": per-object ACLs and conditional writes are switched on only where the driver's behaviour against that provider is documented. It uses the core `filesystem` package and `github.com/ubgo/filesystem/adapters/s3`. The guide [S3-compatible stores](../../docs/adapters/s3-compatible.md) explains each provider.

## Run it

```sh
cd examples
go run ./adapters-s3-compatible
go test ./adapters-s3-compatible
```

Opening a disk sends no request (the AWS SDK resolves credentials lazily), so the first part runs offline with no credentials and prints one line per provider. That proves the DSNs parse and set the intended flags, not that a provider honours them: for that, run the S3 driver's conformance suite against a real bucket with `FILESYSTEM_S3_TEST_DSN` set to its DSN and `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` set to its keys, as the guide describes. The test fails if any DSN stops parsing. The second part starts gofakes3 in-process and reads the keys from `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY`, which it sets itself, so it needs no account either.

## What it does

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
round trip: read back "meow"
listed: photos/cat.txt 4 bytes
if-not-exists without conditional_write: unsupported
refused DSN (keys in the URL): invalid_argument
refused DSN (access_key_env alone): invalid_argument
refused DSN (unset key variable): invalid_argument
refused DSN (path_style=yes): invalid_argument
```

1. Each provider's DSN is opened through a Mux with the S3 opener registered.
2. `visibility` is true only where `acl=true` is set (AWS with object ACLs enabled on the bucket); everywhere else a per-object public write is `unsupported`.
3. `conditional` is true only for AWS and SeaweedFS, the providers checked to honour `If-None-Match`; on the others `IfNotExists` is refused rather than becoming a silent overwrite.
4. The round trip opens a DSN with an endpoint, `path_style=true` and per-disk key variables against gofakes3, the way any provider's is opened.
5. Keys in the URL, half a key pair, an unset key variable and a flag that is not a boolean are each refused with `invalid_argument` before any request.

## The code

The provider list. Placeholders in angle brackets are the account's own values; credentials never go in a DSN.

<!-- ds:block id=providers-8brajey9 -->
**providers** · [`examples/adapters-s3-compatible/main.go:40-50`](main.go#L40-L50)

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

Every DSN opened and its two provider-dependent capabilities printed.

<!-- ds:block id=run-78w52jn4 -->
**run** · [`examples/adapters-s3-compatible/main.go:66-80`](main.go#L66-L80)

```go
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
```
<!-- /ds:block hash=b29a12 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The provider list and the loop that opens each DSN |
| `main_test.go` | Runs the program and checks each provider's line, so a DSN in the guide cannot stop parsing unnoticed |

## Next

- The guide: [S3-compatible stores](../../docs/adapters/s3-compatible.md), after [Amazon S3](../../docs/adapters/s3.md), whose parameters all apply here.
- [S3 driver](../adapters-s3): the full driver walkthrough against a fake S3.
- [Capabilities](../concepts-capabilities): why an undeclared capability is refused, not faked.

The round trip against the in-process S3.

<!-- ds:block id=roundtrip-866uha3a -->
**roundTrip** · [`examples/adapters-s3-compatible/roundtrip.go:69-96`](roundtrip.go#L69-L96)

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
