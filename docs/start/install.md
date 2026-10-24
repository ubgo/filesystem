# Install

You will have the modules you need in your Go module, a first disk writing to a local directory, and the same code running on S3 with nothing changed but a DSN. Read this first whatever you are building; the [first app](first-app.md) starts where it ends.

## Before you start

- Go 1.25 or later for the core module, `adapters/s3`, `adapters/azure`, `contrib/tus` and the `fs` command. `records`, `adapters/gcs`, `adapters/sftp`, `contrib/imaging` and `contrib/otel` need Go 1.26.
- The library is pre-release, so the API can still change; your `go.sum` pins the version you tested.
- Nothing else. The core module depends on the standard library only, so the first steps need no account, no server and no Docker.

## Step 1 — Add the core module

```sh
go get github.com/ubgo/filesystem@latest
```

The core module holds everything that needs only the standard library: the `Disk`, the local and memory drivers, interceptors, the DSN `Mux`, signed URLs, the HTTP handlers, uploads, malware scanning through clamd, and the test fakes. Every package that needs a third-party library is a module of its own, so your app downloads the AWS SDK only if it imports the S3 adapter. A test in the repository (`deps_test.go`) fails if the core ever gains a dependency.

| You want | Add |
|---|---|
| Local directories, memory, uploads, signed URLs, `net/http` handlers, interceptors, clamd scanning, test fakes | nothing more: all in `github.com/ubgo/filesystem` |
| Amazon S3, Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces, SeaweedFS, Garage, MinIO | `github.com/ubgo/filesystem/adapters/s3` |
| Google Cloud Storage | `github.com/ubgo/filesystem/adapters/gcs` |
| Azure Blob Storage (and Azurite) | `github.com/ubgo/filesystem/adapters/azure` |
| SFTP servers | `github.com/ubgo/filesystem/adapters/sftp` |
| Files as database rows, attachments, trash, and the plugins (audit, folders, quota, share, tags, versions, HTTP endpoints) | `github.com/ubgo/filesystem/records`, plus `github.com/ubgo/sqlb` and a SQL driver (step 4) |
| Image resizing, variants, dimensions, stripping EXIF and GPS | `github.com/ubgo/filesystem/contrib/imaging` |
| OpenTelemetry traces and metrics | `github.com/ubgo/filesystem/contrib/otel` |
| Resumable uploads (tus) | `github.com/ubgo/filesystem/contrib/tus` |
| The `fs` command-line tool | clone the repository, then `cd cmd/fs && go install .` (see [the CLI page](../integrations/cli.md)) |

Always list the core module in the same `go get` as any other module of this repository, all at `@latest`. Each module's own `go.mod` points at the core through a placeholder version that only resolves inside the repository; naming the core alongside it lets Go pick a real commit instead. `go get github.com/ubgo/filesystem/records` on its own, in a module that does not have the core yet, fails with:

```text
go: github.com/ubgo/filesystem/records imports
	github.com/ubgo/filesystem: github.com/ubgo/filesystem@v0.0.0-00010101000000-000000000000: invalid version: unknown revision 000000000000
```

Check: `go list -m all | grep ubgo` shows `github.com/ubgo/filesystem` at a resolved version.

## Step 2 — A first disk on a local directory

A disk is a driver (one backend) wrapped in a `*filesystem.Disk`, which is the type your code holds. Every portable behaviour lives in the `Disk`: key cleaning and traversal refusal, content-type detection, SHA-256 hashing while streaming, size caps, typed errors and the interceptor chain. Building one by hand takes two calls:

<!-- ds:block id=install-firstdisk-knpegaay -->
**firstDisk** · [`examples/start-install/main.go:51-57`](../../examples/start-install/main.go#L51-L57)

```go
func firstDisk(dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir)
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(diskName, drv)
}
```
<!-- /ds:block hash=08a1aa -->

The local driver creates the directory if it does not exist and opens it through an `os.Root`, so no key, symlink or `..` can reach outside it even if the `Disk`'s own checks were bypassed. Writes go to a temporary file in a reserved `.filesystem/` directory at the root and are renamed into place, so a reader never sees half a file and a failed write leaves nothing behind. The disk name (`media` here) appears in errors, logs and signed URLs, and must match `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`.

Most apps never build disks by hand: they open them from a DSN, so the backend becomes configuration. A `Mux` maps DSN schemes to the drivers your program imports. Nothing registers itself when a package is imported; your code lists the drivers it uses:

<!-- ds:block id=install-newmux-rs6juqjr -->
**newMux** · [`examples/start-install/main.go:62-69`](../../examples/start-install/main.go#L62-L69)

```go
func newMux() (*filesystem.Mux, error) {
	mux := filesystem.NewMux()
	return mux, errors.Join(
		mux.Register(local.Scheme, local.Opener),
		mux.Register(memory.Scheme, memory.Opener),
		mux.Register(s3.Scheme, s3.Opener),
	)
}
```
<!-- /ds:block hash=eb5ff8 -->

Here is a whole program body. It opens the disk named by a DSN, proves it works with `Check` (which writes a small object, reads it back, compares and deletes it), then writes a file and reads it back:

<!-- ds:block id=install-run-zd8h2968 -->
**run** · [`examples/start-install/main.go:74-97`](../../examples/start-install/main.go#L74-L97)

```go
func run(ctx context.Context, dsn string, out io.Writer) error {
	mux, err := newMux()
	if err != nil {
		return err
	}
	disk, err := mux.Open(ctx, diskName, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	if err := disk.Check(ctx); err != nil {
		return err
	}
	e, err := disk.Write(ctx, helloKey, strings.NewReader(helloText), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, helloKey)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "wrote %s: %d bytes, %s\nread back %q\ncapabilities: %s\n", e.Key, e.Size, e.ContentType, got, disk.Capabilities())
	return err
}
```
<!-- /ds:block hash=227e02 -->

The program is [examples/start-install](../../examples/start-install). With no `STORAGE_DSN` it uses `file:storage`, a directory next to where it runs:

```sh
cd examples && go run ./start-install
```

```text
wrote hello/world.txt: 14 bytes, text/plain; charset=utf-8
read back "hello, storage"
capabilities: [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap]
```

Check: `storage/hello/world.txt` exists and holds `hello, storage`, and `storage/.filesystem/` is the driver's staging directory (keys inside it are refused and listings skip it). The content type was detected from the bytes and the `.txt` extension; you did not pass one.

Two DSN forms open a local directory: `file:relative/path` (relative to the working directory) and `file:///absolute/path`. `file://storage` is an error, because `storage` would be a host name: `file DSN has a host "storage"; use file:///absolute/path or file:relative/path`.

## Step 3 — The same code on S3

Add the adapter in the same `go get` as the core, and register its opener (the `s3.Scheme` line in `newMux` above):

```sh
go get github.com/ubgo/filesystem@latest github.com/ubgo/filesystem/adapters/s3@latest
```

Then point the DSN at a bucket. The bucket must exist; the driver never creates one. Credentials come from the AWS default chain (`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, shared config files, or an instance role), never from the DSN, so a DSN can be logged without leaking a secret. This run is against a SeaweedFS server on the same machine, the S3 server the repository's own tests use:

```sh
AWS_ACCESS_KEY_ID=any AWS_SECRET_ACCESS_KEY=any \
STORAGE_DSN='s3://docs-install?region=us-east-1&endpoint=http://127.0.0.1:8333&path_style=true&prune_prefixes=true&conditional_write=true' \
go run ./start-install
```

```text
wrote hello/world.txt: 14 bytes, text/plain; charset=utf-8
read back "hello, storage"
capabilities: [copy,delete-prefix,delete-keys,presign,multipart,presign-part,presign-post,metadata,content-type,headers,presign-headers,conditional-write,checksum-sha256]
```

The code did not change; the capabilities did. S3 copies on the server, deletes 1,000 keys per request and signs its own URLs (`presign`), but has no atomic rename (`Move` falls back to copy and delete) and no per-object visibility unless you turn on ACLs. A capability a backend lacks either falls back to a portable implementation or fails with `CodeUnsupported`; it is never silently dropped. The [capabilities](../concepts/capabilities.md) page has the full table.

The DSN for the common providers:

| Provider | DSN |
|---|---|
| Amazon S3 | `s3://my-bucket?region=eu-west-1` |
| Cloudflare R2 | `s3://my-bucket?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com` |
| SeaweedFS, Garage, MinIO (self-hosted) | `s3://my-bucket?region=us-east-1&endpoint=http://host:port&path_style=true` |
| In memory, for tests | `mem://` |

Every parameter of every adapter is in the [DSN reference](../reference/dsn.md), and each provider's quirks are in [S3-compatible stores](../adapters/s3-compatible.md). A misspelt parameter is an error at start-up, not a silently ignored setting:

```text
filesystem: invalid_argument: disk media: unknown DSN parameter "regoin" for scheme "s3"
```

The repository's test for this page runs `run` three times, on a temporary directory, on `mem://` and on an in-process S3 fake, to prove the same function works on each. The fake is how you can test S3 code without an account:

<!-- ds:block id=install-fakes3-wu6urkkk -->
**fakeS3** · [`examples/start-install/main_test.go:56-69`](../../examples/start-install/main_test.go#L56-L69)

```go
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
```
<!-- /ds:block hash=85643f -->

## Step 4 — Records and a SQL driver

If your app keeps files as database rows (who uploaded what, which post a photo belongs to, the trash), add the `records` module, the SQL builder it renders queries with, and a driver for your database:

```sh
go get github.com/ubgo/filesystem@latest github.com/ubgo/filesystem/records@latest github.com/ubgo/sqlb modernc.org/sqlite
```

`records` never imports a database driver: your app opens the `*sql.DB` with the driver it wants and passes the matching dialect to `records.New`. It is tested on all three engines.

| Engine | Driver | Dialect |
|---|---|---|
| Postgres | `github.com/jackc/pgx/v5/stdlib` (`sql.Open("pgx", dsn)`) | `sqlb.PostgresDialect{}` |
| MySQL 8 | `github.com/go-sql-driver/mysql` with `parseTime=true&loc=UTC` in the DSN | `sqlb.MySQLDialect{}` |
| SQLite | `modernc.org/sqlite` (pure Go, `sql.Open("sqlite", dsn)`) | `sqlb.SQLiteDialect{}` |

For SQLite, add a busy timeout so a second writer waits instead of failing with "database is locked", and use a file rather than `:memory:` (each connection to `:memory:` is a separate, empty database):

```text
file:app.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)
```

Check: `go build ./...` succeeds with `_ "modernc.org/sqlite"` (or your driver) imported in `main`. The [first app](first-app.md) wires `records.New` and creates the tables.

## What can go wrong

| Symptom | Fix |
|---|---|
| `invalid version: unknown revision 000000000000` | A module of this repository was fetched without the core. List `github.com/ubgo/filesystem@latest` in the same `go get`. |
| `module requires go >= 1.26` | `records`, `gcs`, `sftp`, `imaging` and `otel` need Go 1.26. Update Go. |
| `filesystem: invalid_argument: disk media: no opener registered for scheme "gs"` | The DSN names a backend the `Mux` does not know. Import the adapter and `Register` its `Scheme` and `Opener`. |
| `filesystem: invalid_argument: disk media: unknown DSN parameter "…" for scheme "…"` | A typo in the DSN, or a parameter another adapter takes. The [DSN reference](../reference/dsn.md) lists them. |
| `check disk media: write: … permission: operation error S3: PutObject, https response error StatusCode: 403 …` | Wrong credentials, or a bucket that does not exist (SeaweedFS answers 403 for a missing bucket; AWS answers `NoSuchBucket`), or a missing `endpoint` for a non-AWS provider. The SDK's own error is at the end of the message. |
| `go install github.com/ubgo/filesystem/cmd/fs@latest` fails with `The go.mod file for the module providing named packages contains one or more replace directives` | `go install …@version` refuses modules with `replace` directives, which every module but the core has. Build from a clone instead. |
| `sql: unknown driver "sqlite"` | The driver is not imported. Add `_ "modernc.org/sqlite"` to `main`. |

## Next

- [Your first app](first-app.md): an HTTP server where users upload photos, recorded in SQLite, served through signed links.
- [Disks and drivers](../concepts/disks-and-drivers.md) and [configuration](../concepts/configuration.md): the `Disk`, the `Manager` and every way to open disks.
- [Choosing a backend](../adapters/README.md): what each adapter can do.
