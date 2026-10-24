# Supabase Storage

Supabase Storage has no driver of its own because it does not need one: it speaks the S3 protocol, so the S3 driver (`github.com/ubgo/filesystem/adapters/s3`) stores files in it. See [S3-compatible stores](s3-compatible.md) for the flags that differ between providers. This page takes you from the Supabase dashboard to a working disk.

> Not verified against a Supabase project. The DSN shape below follows Supabase's S3 documentation, and the example proves it reaches the `/storage/v1/s3` path through an in-process stand-in server; no run of this library's tests has talked to Supabase itself. Run the conformance suite against a scratch bucket (Step 6) before you depend on it.

## When to choose Supabase Storage

- Choose it when the application already runs on Supabase: its database and auth live there, and files uploaded through Supabase's own client libraries and through this driver are the same objects in the same buckets.
- Choose a plain object store ([S3](s3.md), [R2](s3-compatible.md#cloudflare-r2), [GCS](gcs.md)) when you do not use Supabase otherwise, or when you need features this page lists as unverified (conditional writes, per-object ACLs, multipart).

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/s3`; read [s3.md](s3.md) for every parameter and behaviour of the driver.
- The runnable program is [`examples/adapters-supabase`](../../examples/adapters-supabase/). It opens the DSN this page gives against an in-process S3 that answers only under `/storage/v1/s3`, so `go run ./adapters-supabase` from `examples/` needs no project and no keys.
- You need a Supabase project with at least one bucket. A Supabase bucket is an S3 bucket; the driver never creates buckets.

## Step 1: Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/s3
```

## Step 2: Enable the S3 connection and create keys

In the Supabase dashboard, open Project Settings, Storage, and enable the S3 connection. It shows the endpoint (`https://<project-ref>.supabase.co/storage/v1/s3`) and the region, and creates an access key and secret. Put the pair in two environment variables on the server, for example `SUPABASE_S3_ACCESS_KEY_ID` and `SUPABASE_S3_SECRET_ACCESS_KEY`. They never go in the DSN: the S3 opener refuses keys in the URL.

The S3 access keys bypass Supabase's row-level security policies: whoever holds them can read and write every bucket of the project. Keep them on the server, never in a browser or mobile app.

## Step 3: Build the DSN

```text
s3://my-bucket?region=<project-region>&endpoint=https://<project-ref>.supabase.co/storage/v1/s3&path_style=true&access_key_env=SUPABASE_S3_ACCESS_KEY_ID&secret_key_env=SUPABASE_S3_SECRET_ACCESS_KEY
```

| Parameter | Value | Why |
|---|---|---|
| bucket (the host) | the Supabase bucket's name | a Supabase bucket is an S3 bucket |
| `region` | the project's region, as the dashboard shows it | SigV4 signs with it |
| `endpoint` | the project URL plus `/storage/v1/s3` | the SDK keeps that path and puts the bucket and key after it |
| `path_style` | `true` | required: the bucket goes in the path, not in the host name |
| `access_key_env`, `secret_key_env` | names of the variables from Step 2 | per-disk keys, so a Supabase disk and an AWS disk can share a process; without them the SDK reads `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` |

The example builds it with `url.Values`, so the endpoint is escaped correctly:

<!-- ds:block id=supabasedsn-cqjtcxsy -->
**supabaseDSN** · [`examples/adapters-supabase/main.go:79-87`](../../examples/adapters-supabase/main.go#L79-L87)

```go
func supabaseDSN(projectURL string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, region)
	q.Set(fss3.ParamEndpoint, projectURL+s3Path)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamAccessKeyEnv, accessKeyEnv)
	q.Set(fss3.ParamSecretKeyEnv, secretKeyEnv)
	return fss3.Scheme + "://" + bucket + "?" + q.Encode()
}
```
<!-- /ds:block hash=4b7734 -->

## Step 4: Open the disk

Register the S3 opener on a Mux and open the DSN. Opening sends no request (unless the DSN declares `versioning` or `object_lock`, which are checked against the bucket), so a wrong key, region or endpoint shows up as an error on the first operation, not at open time.

<!-- ds:block id=opendisk-jg5srcsy -->
**openDisk** · [`examples/adapters-supabase/main.go:91-97`](../../examples/adapters-supabase/main.go#L91-L97)

```go
func openDisk(ctx context.Context, projectURL string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "avatars", supabaseDSN(projectURL))
}
```
<!-- /ds:block hash=44f045 -->

## Step 5: Store, list, read and delete

From here the disk is an ordinary S3 disk. The example writes a user's profile file, lists the folder, reads it back and deletes it:

<!-- ds:block id=storeprofile-8ybfcf7y -->
**storeProfile** · [`examples/adapters-supabase/main.go:102-126`](../../examples/adapters-supabase/main.go#L102-L126)

```go
func storeProfile(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	key := "users/42/profile.json"
	e, err := disk.Write(ctx, key, strings.NewReader(`{"name":"Ada"}`), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored: %s %d bytes %s\n", e.Key, e.Size, e.ContentType)
	for entry, err := range disk.List(ctx, "users/42", filesystem.ListOptions{}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s\n", entry.Key)
	}
	b, err := disk.ReadAll(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read back: %s\n", b)
	if err := disk.Delete(ctx, key); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, key)
	fmt.Fprintf(out, "exists after delete: %t\n", ok)
	return err
}
```
<!-- /ds:block hash=4f6149 -->

The stand-in server records each request; the first one is `PUT /storage/v1/s3/avatars/users/42/profile.json`, which is the endpoint's path, then the bucket, then the key, as Supabase expects with path-style addressing.

## Step 6: Check the flags against your project

Leave `conditional_write` and `acl` off unless the conformance suite, run against a scratch bucket, shows Supabase honours them. Public or private is a bucket setting in Supabase, so per-object visibility does not apply; keep public and private files in two buckets (see [public and private files](../concepts/public-and-private.md)).

```sh
cd adapters/s3
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…
FILESYSTEM_S3_TEST_DSN='s3://scratch-bucket?region=<project-region>&endpoint=https://<project-ref>.supabase.co/storage/v1/s3&path_style=true' \
  go test -count=1 -run TestConformanceAgainstARealStore -v ./...
```

Each case runs under a random prefix that is deleted afterwards. If a case for a feature fails, leave that feature off for Supabase.

## Try it

```sh
cd examples
go run ./adapters-supabase
go test ./adapters-supabase
```

It prints:

```text
capabilities: [copy,delete-prefix,delete-keys,presign,multipart,presign-part,presign-post,metadata,content-type,headers,presign-headers,checksum-sha256]
stored: users/42/profile.json 14 bytes application/json
listed: users/42/profile.json
read back: {"name":"Ada"}
exists after delete: false
first request: PUT /storage/v1/s3/avatars/users/42/profile.json
if-not-exists: unsupported
```

The capabilities are what the S3 driver declares for this DSN. They are not a statement of what Supabase supports: presigned URLs, presigned parts and POST forms, multipart uploads, stored metadata and headers were not checked against Supabase.

## Pitfalls

- **Forgetting `path_style=true`.** Without it the SDK puts the bucket in the host name (`my-bucket.<project-ref>.supabase.co`), which is not Supabase's S3 endpoint.
- **Leaving `/storage/v1/s3` off the endpoint.** The project URL alone is Supabase's API root, not its S3 endpoint; every request then misses.
- **Keys in the DSN, or half a pair.** `s3://key:secret@bucket` is refused, so is `access_key_env` without `secret_key_env`, and so is a variable that is unset or empty: all `CodeInvalidArgument` when the disk opens (the [S3-compatible example](../../examples/adapters-s3-compatible/) prints each).
- **The keys skip row-level security.** Authorise each request in your own code before it touches the disk, and never ship the keys to a client. For a browser upload, sign a URL on the server, after checking Supabase accepts it (Step 6).
- **`IfNotExists` is refused** (`CodeUnsupported`) because `conditional_write` is off; do not turn it on to make the error go away, since a provider that ignores `If-None-Match` would then overwrite silently.
- **Two names for one object.** A file uploaded through Supabase's client libraries is the same object the driver sees, under the same key; nothing keeps the two writers apart except your key layout.

## Related

- [S3-compatible stores](s3-compatible.md): the flags that differ between providers, and running the conformance suite against one.
- [Amazon S3](s3.md): every parameter and behaviour of the driver.
- [Public and private files](../concepts/public-and-private.md) and [serving files](../concepts/serving-files.md).
- [Comparison of every backend](README.md).
