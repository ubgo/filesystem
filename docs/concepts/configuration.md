# Configuration

An application rarely builds disks by hand. It describes each one as a DSN string (`file:///var/app/uploads`, `s3://media?region=auto&endpoint=…`), opens them through a `Mux` that knows the drivers it imports, and keeps them in a `Manager` under names like `media` and `backups`. Switching from local disk in development to S3 in production is then a configuration change. This page covers the Mux, the parameters every DSN accepts, building a whole Manager from environment variables, and the Manager's operations.

The snippets come from `examples/concepts-configuration`. Run it with `go run ./concepts-configuration` from the `examples` directory.

## Step 1 — Register the drivers you import

A `filesystem.Mux` maps URL schemes to openers. Each driver package exports its `Scheme` and an `Opener`; the application registers the ones it uses:

<!-- ds:block id=newmux-6gp3nw3a -->
**newMux** · [`examples/concepts-configuration/main.go:68-77`](../../examples/concepts-configuration/main.go#L68-L77)

```go
func newMux() (*filesystem.Mux, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		return nil, err
	}
	return mux, nil
}
```
<!-- /ds:block hash=98c64d -->

The Mux is a value, not a global registry filled by `init` functions. Importing a driver registers nothing, two parts of a program (or two tests running in parallel) can hold different sets of drivers, and the only way a scheme becomes available is a line you can read. Registering a scheme twice is `CodeExists`.

| Scheme | Package | Module | DSN shape |
|---|---|---|---|
| `file` | `filesystem/local` | core | `file:///absolute/path` or `file:relative/path` |
| `mem` | `filesystem/memory` | core | `mem://` (the host is ignored, so `mem://cache` names it for readers) |
| `s3` | `filesystem/adapters/s3` | own | `s3://bucket?region=…&endpoint=…` |
| `gs` | `filesystem/adapters/gcs` | own | `gs://bucket?…` |
| `azblob` | `filesystem/adapters/azure` | own | `azblob://container?…` |
| `sftp` | `filesystem/adapters/sftp` | own | `sftp://user@host:port/absolute/root?known_hosts=…` |

Each adapter's own parameters are in its page under [Adapters](../adapters/README.md) and all of them in [reference/dsn](../reference/dsn.md). The built-in DSNs are designed to hold no secret: S3 and GCS take credentials from the provider's default chain (environment, shared files, instance roles), Azure a connection string from the environment, and SFTP a password from the variable named by `password_env` or a key from the file named by `identity`.

## Step 2 — Open a disk from a DSN

`mux.Open(ctx, name, dsn, options...)` parses the DSN, builds the driver with the registered opener and returns a Disk named `name`. Four query parameters are handled by the Mux itself and work on every scheme:

| Parameter | Constant | Effect |
|---|---|---|
| `prefix` | `ParamPrefix` | scopes the disk to a key prefix, as `Disk.Scope` does: key `logo.png` is stored as `<prefix>/logo.png` |
| `visibility` | `ParamVisibility` | `public` or `private`: the disk's default visibility (`WithDefaultVisibility`) |
| `public_url` | `ParamPublicURL` | the base of public URLs (`WithPublicURL`); it gains the prefix when there is one |
| `readonly` | `ParamReadOnly` | `true` adds the `ReadOnly` interceptor |

Options passed to `Open` apply after the DSN's own settings, so code adds what a string cannot hold: interceptors, a URL signer.

<!-- ds:block id=openwithparams-e2jtzbhn -->
**openWithParams** · [`examples/concepts-configuration/main.go:83-101`](../../examples/concepts-configuration/main.go#L83-L101)

```go
func openWithParams(ctx context.Context, mux *filesystem.Mux, dir string, out io.Writer) error {
	dsn := "file://" + filepath.ToSlash(dir) + "?prefix=tenant-a&visibility=public&public_url=" + cdnBase
	disk, err := mux.Open(ctx, "media", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	e, err := disk.WriteBytes(ctx, "logo.png", []byte("png"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	u, err := disk.PublicURL(ctx, e.Key)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(filepath.Join(dir, "tenant-a", "logo.png"))
	fmt.Fprintf(out, "key %s, %s, url %s, on disk under tenant-a: %t\n", e.Key, e.Visibility, u, statErr == nil)
	return nil
}
```
<!-- /ds:block hash=71fca9 -->

Check: `key logo.png, public, url https://cdn.example.com/tenant-a/logo.png, on disk under tenant-a: true`. The disk's key is `logo.png`; the object is at `tenant-a/logo.png` in the directory; the public URL includes the prefix.

## Step 3 — Mistakes fail at start-up

Every opener rejects a parameter it does not understand, so a typo fails when the disk opens instead of being ignored. An unregistered scheme fails the same way. Neither error contains the DSN, because DSNs often carry credentials; the second line below opened a DSN with a user and password in it:

<!-- ds:block id=refusals-2dba5qe4 -->
**refusals** · [`examples/concepts-configuration/main.go:107-119`](../../examples/concepts-configuration/main.go#L107-L119)

```go
func refusals(ctx context.Context, mux *filesystem.Mux, out io.Writer) {
	_, err := mux.Open(ctx, "media", "mem://?visiblity=public")
	fmt.Fprintf(out, "typo: %v\n", err)
	_, err = mux.Open(ctx, "media", "s3://AKIA:topsecret@bucket?region=auto")
	fmt.Fprintf(out, "unregistered: %v\n", err)
	ro, err := mux.Open(ctx, "archive", "mem://?readonly=true")
	if err != nil {
		fmt.Fprintf(out, "readonly: %v\n", err)
		return
	}
	_, err = ro.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "readonly write: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=590db6 -->

```text
typo: filesystem: invalid_argument: disk media: unknown DSN parameter "visiblity" for scheme "mem"
unregistered: filesystem: invalid_argument: disk media: no opener registered for scheme "s3"
readonly write: read_only
```

A driver you write uses `filesystem.RejectParams(u, allowed...)` in its opener to give the same guarantee.

## Step 4 — A Manager from the environment

`mux.OpenManager(ctx, prefix, lookup, options...)` builds every disk from configuration, twelve-factor style. With the prefix `STORAGE_`:

```sh
STORAGE_DISKS=media,backups
STORAGE_MEDIA_DSN=s3://media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com&visibility=public&public_url=https://cdn.example.com
STORAGE_BACKUPS_DSN=file:///var/backups
STORAGE_DEFAULT_DISK=media
```

| Variable | Constant | Meaning |
|---|---|---|
| `<PREFIX>DISKS` | `EnvDisks` | the disk names, comma-separated; spaces around names are ignored |
| `<PREFIX><NAME>_DSN` | `EnvDSNSuffix` | each disk's DSN; the name is upper-cased with `-` and `.` as `_` (`filesystem.EnvName("STORAGE_", "media-cdn")` is `STORAGE_MEDIA_CDN_DSN`) |
| `<PREFIX>DEFAULT_DISK` | `EnvDefaultDisk` | the default disk; empty means the first listed |

`lookup` is a `filesystem.LookupFunc`: pass `os.LookupEnv` in production and a map in tests. Taking it as an argument instead of reading the process environment keeps the function free of hidden state. `options` apply to every disk.

<!-- ds:block id=fromenvironment-tvhw2dwd -->
**fromEnvironment** · [`examples/concepts-configuration/main.go:125-144`](../../examples/concepts-configuration/main.go#L125-L144)

```go
func fromEnvironment(ctx context.Context, mux *filesystem.Mux, dir string, out io.Writer) (*filesystem.Manager, error) {
	env := map[string]string{
		"STORAGE_DISKS":        "media, backups",
		"STORAGE_MEDIA_DSN":    "mem://media?visibility=public",
		"STORAGE_BACKUPS_DSN":  "file://" + filepath.ToSlash(filepath.Join(dir, "backups")),
		"STORAGE_DEFAULT_DISK": "media",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	mgr, err := mux.OpenManager(ctx, envPrefix, lookup)
	if err != nil {
		return nil, err
	}
	def, err := mgr.Default()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "disks %s, default %s\n", strings.Join(mgr.Names(), ","), def.Name())
	fmt.Fprintf(out, "variable for media-cdn: %s\n", filesystem.EnvName(envPrefix, "media-cdn"))
	return mgr, nil
}
```
<!-- /ds:block hash=d43311 -->

Check: `disks backups,media, default media` and `variable for media-cdn: STORAGE_MEDIA_CDN_DSN`.

A missing or empty variable, two names that map to the same variable (`media-cdn` and `media.cdn`), or a default that is not listed is `CodeInvalidArgument` naming the variable, never its value, and the disks opened so far are closed. A disk that fails to open (an unknown scheme, a bad parameter) keeps its error's code, and the message names the variable and the disk: `STORAGE_MEDIA_DSN: disk media: unknown DSN parameter …`.

<!-- ds:block id=missingvariable-fv6t3a7d -->
**missingVariable** · [`examples/concepts-configuration/main.go:178-183`](../../examples/concepts-configuration/main.go#L178-L183)

```go
func missingVariable(ctx context.Context, mux *filesystem.Mux, out io.Writer) {
	env := map[string]string{"STORAGE_DISKS": "media,backups", "STORAGE_MEDIA_DSN": "mem://"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	_, err := mux.OpenManager(ctx, envPrefix, lookup)
	fmt.Fprintf(out, "missing: %v\n", err)
}
```
<!-- /ds:block hash=f11242 -->

Check: `missing: filesystem: invalid_argument: STORAGE_BACKUPS_DSN is not set (disk "backups")`.

## Step 5 — The Manager

A `filesystem.Manager` holds named disks and a default. It is a value the application owns, safe for concurrent use; tests and several apps in one process never share disks.

| Method | What it does |
|---|---|
| `NewManager(disks...)` | a Manager holding the disks; the first is the default |
| `Add(d)` | registers a disk under its name; a duplicate name is `CodeExists`; the first added becomes the default |
| `SetDefault(name)`, `Default()` | choose and get the default disk; `CodeNotFound` when none |
| `Disk(name)` | the named disk, or `CodeNotFound` |
| `Names()` | the names, sorted |
| `Check(ctx)` | `Disk.Check` on every disk, failures joined: run it at start-up or in a health probe |
| `Close()` | closes every disk, failures joined |

`filesystem.CopyAcross(ctx, src, srcKey, dst, dstKey)` copies between two disks by streaming, keeping the content type, and the metadata, headers and visibility where the destination stores them; on the same disk it is `Disk.Copy`. `MoveAcross` is CopyAcross followed by deleting the source, not atomic: if the delete fails, the object exists on both.

<!-- ds:block id=managerops-tze3ff3x -->
**managerOps** · [`examples/concepts-configuration/main.go:149-173`](../../examples/concepts-configuration/main.go#L149-L173)

```go
func managerOps(ctx context.Context, mgr *filesystem.Manager, out io.Writer) error {
	if err := mgr.Check(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "check: every disk works")
	media, err := mgr.Disk("media")
	if err != nil {
		return err
	}
	backups, err := mgr.Disk("backups")
	if err != nil {
		return err
	}
	if _, err := media.WriteBytes(ctx, "report.csv", []byte("a,b\n1,2\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := filesystem.CopyAcross(ctx, media, "report.csv", backups, "2026/report.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "copied to backups:%s (%s)\n", e.Key, e.ContentType)
	_, err = mgr.Disk("nope")
	fmt.Fprintf(out, "unknown disk: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=b65cc2 -->

Check: `check: every disk works`, `copied to backups:2026/report.csv (text/csv; charset=utf-8)` and `unknown disk: not_found`.

The Manager is also what other parts of the library resolve disk names with: `fshttp.Signed` finds the disk a signed URL names through it, and the records Store finds a file's disk through it. A disk that serves signed links must therefore be in the Manager handed to `fshttp`, scoped disks included; see [Multi-tenancy](multi-tenancy.md).

## Typical set-ups

| Environment | `STORAGE_MEDIA_DSN` |
|---|---|
| Unit tests | `mem://` (or `filesystemtest.NewFake` without any configuration) |
| Local development | `file:./var/media?visibility=public&public_url=http://localhost:8080/media` with `fshttp.Public` mounted at `/media/` |
| Production on R2 | `s3://media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com&visibility=public&public_url=https://cdn.example.com` |
| A read-only view of production data | the production DSN plus `&readonly=true` |

## Sizes in configuration

`filesystem.ParseSize` reads a human size from configuration into bytes, for upload limits and the like: `ParseSize(os.Getenv("UPLOAD_MAX"))` with `UPLOAD_MAX=10MB`. Decimal units are powers of 1000 and binary units powers of 1024, as the SI and IEC define them, so `10MB` is 10,000,000 bytes and `10MiB` is 10,485,760; a bare number is bytes.

| Unit | Bytes | Unit | Bytes |
|---|---|---|---|
| `B` | 1 | | |
| `KB` | 1,000 | `KiB` | 1,024 |
| `MB` | 1,000,000 | `MiB` | 1,048,576 |
| `GB` | 10^9 | `GiB` | 2^30 |
| `TB` | 10^12 | `TiB` | 2^40 |

It is strict and refuses (with `invalid_argument`) rather than guess: a fraction is allowed (`1.5GiB`) only when the result is a whole number of bytes (`1.5B` is refused), units are case-sensitive (`10mb` and `10M` are refused), and no space, sign, exponent or digit separator is accepted. The same units are constants (`filesystem.MB`, `filesystem.GiB`, …) for limits written in code.

<!-- ds:block id=sizes-82ucppeh -->
**sizes** · [`examples/concepts-configuration/main.go:189-204`](../../examples/concepts-configuration/main.go#L189-L204)

```go
func sizes(out io.Writer) error {
	limit, err := filesystem.ParseSize("10MB")
	if err != nil {
		return err
	}
	binary, err := filesystem.ParseSize("1.5GiB")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "10MB = %d bytes, 1.5GiB = %d bytes, 10*MiB = %d\n", limit, binary, 10*filesystem.MiB)
	for _, typo := range []string{"10mb", "10 MB", "1.5B", "-1KB"} {
		_, err := filesystem.ParseSize(typo)
		fmt.Fprintf(out, "ParseSize(%q): %s\n", typo, filesystem.CodeOf(err))
	}
	return nil
}
```
<!-- /ds:block hash=dacbea -->

Check: `10MB = 10000000 bytes, 1.5GiB = 1610612736 bytes, 10*MiB = 10485760`, then `invalid_argument` for `10mb`, `10 MB`, `1.5B` and `-1KB`. Parse sizes at start-up with the rest of the configuration, so a typo stops the deployment instead of becoming an upload limit a thousand times too small or too large.

## Pitfalls

- Registering a driver in an `init` function of your own package to make it "automatic": it brings back the hidden global the Mux avoids. Register in the function that builds your application.
- Logging a DSN: the built-in ones hold no secret by design, but an operator can still paste a user and password into one, as the `refusals` demo does. The library's errors never print a DSN; your logs should not either.
- Forgetting that `prefix` changes keys: a disk opened with `prefix=tenant-a` writes `logo.png` to `tenant-a/logo.png`, and a second disk on the same bucket without the prefix sees it there.
- A relative `file:` path is relative to the process's working directory, which differs between `go run`, tests and a service manager. Use an absolute path outside development.

## Next

- [Disks and drivers](disks-and-drivers.md): the options `Open` and `OpenManager` accept.
- [Production](../start/production.md): checking disks at start-up and the settings to review before going live.
- [reference/dsn](../reference/dsn.md): every parameter of every adapter.
