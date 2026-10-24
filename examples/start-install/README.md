# A first disk, on any backend

The program behind the install guide: a first `Disk` on a local directory, then the same code on memory or S3 by changing one DSN. It writes a file, reads it back and prints the disk's capabilities, which is the whole contract an app relies on whatever the backend. It uses the core `github.com/ubgo/filesystem` package with the `local` and `memory` drivers and `adapters/s3`. The guide [Install](../../docs/start/install.md) explains each step.

## Run it

```sh
cd examples
go run ./start-install                                    # a local directory
STORAGE_DSN=mem:// go run ./start-install                 # memory
STORAGE_DSN='s3://bucket?region=auto&endpoint=…' go run ./start-install
go test ./start-install
```

With no `STORAGE_DSN` it uses `file:storage`, a directory named `storage` next to where you run it (here `examples/storage`), which it creates and leaves in place; delete it with `rm -rf storage`. `mem://` writes nothing to disk. For S3, credentials come from the AWS default chain (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` or a profile), never from the DSN. The test needs no network: it runs the same function on a temporary directory, on memory, and on an in-process S3 fake (`gofakes3`).

## What it does

On a local directory:

```sh
go run ./start-install
```

```text
wrote hello/world.txt: 14 bytes, text/plain; charset=utf-8
read back "hello, storage"
capabilities: [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap]
```

On memory, the same code and a longer capability list:

```sh
STORAGE_DSN=mem:// go run ./start-install
```

```text
wrote hello/world.txt: 14 bytes, text/plain; charset=utf-8
read back "hello, storage"
capabilities: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]
```

1. The DSN is opened through a `Mux` that knows the `file`, `mem` and `s3` schemes. Nothing is registered by importing a package: the app lists the drivers it uses.
2. `Check` proves the disk works end to end before anything else runs.
3. The program writes `hello/world.txt`; the content type is detected from the bytes.
4. It reads the file back and prints the capabilities, which differ by backend: a local directory has no metadata or headers, memory has both.

A misspelt DSN parameter is an error, never ignored; the test checks `s3://bucket?regoin=auto` is refused.

## The code

A disk with no DSN at all: the driver, then the `Disk` around it. This is all the core module needs:

<!-- ds:block id=install-firstdisk-knpegaay -->
**firstDisk** · [`examples/start-install/main.go:51-57`](main.go#L51-L57)

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

The schemes this program can open from a DSN:

<!-- ds:block id=install-newmux-rs6juqjr -->
**newMux** · [`examples/start-install/main.go:62-69`](main.go#L62-L69)

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

Open, check, write and read back, the same for every backend:

<!-- ds:block id=install-run-zd8h2968 -->
**run** · [`examples/start-install/main.go:74-97`](main.go#L74-L97)

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

The in-process S3 fake the test runs against, with credentials from the environment as in production:

<!-- ds:block id=install-fakes3-wu6urkkk -->
**fakeS3** · [`examples/start-install/main_test.go:56-69`](main_test.go#L56-L69)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: a disk from a directory, the Mux, and `run` |
| `main_test.go` | The first disk, and the same `run` on a directory, memory and an S3 fake, plus a misspelt parameter |

## Next

- The guide: [Install](../../docs/start/install.md), then [Your first app](../../docs/start/first-app.md).
- Related examples: [quickstart](../quickstart) for a full app, [basics](../basics) for every disk operation, [adapters-s3](../adapters-s3) and [adapters-local](../adapters-local) for each backend, and the [DSN reference](../../docs/reference/dsn.md).
