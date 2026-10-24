# Configuration: DSNs, the Mux and a Manager from the environment

How an application turns configuration into disks. This program registers the drivers it imports on a `Mux` (a value, not a global registry), opens a local disk from a DSN with the parameters every driver accepts (`prefix`, `visibility`, `public_url`, `readonly`), shows configuration mistakes failing at start-up, builds a `Manager` from `STORAGE_*` variables twelve-factor style, and uses the Manager's own operations: the default disk, `Check`, and a copy between two disks. It uses the core `filesystem` package with `local` and `memory`. The guide [Configuration](../../docs/concepts/configuration.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-configuration
go test ./concepts-configuration
```

It creates a temporary directory for the local disks and removes it on exit. The environment is a map inside the program, so it reads no real variables; in an application, pass `os.LookupEnv` instead. The `s3://` DSN in the refusals step is never opened, because no S3 driver is registered.

## What it does

```sh
go run ./concepts-configuration
```

```text
key logo.png, public, url https://cdn.example.com/tenant-a/logo.png, on disk under tenant-a: true
typo: filesystem: invalid_argument: disk media: unknown DSN parameter "visiblity" for scheme "mem"
unregistered: filesystem: invalid_argument: disk media: no opener registered for scheme "s3"
readonly write: read_only
disks backups,media, default media
variable for media-cdn: STORAGE_MEDIA_CDN_DSN
check: every disk works
copied to backups:2026/report.csv (text/csv; charset=utf-8)
unknown disk: not_found
missing: filesystem: invalid_argument: STORAGE_BACKUPS_DSN is not set (disk "backups")
10MB = 10000000 bytes, 1.5GiB = 1610612736 bytes, 10*MiB = 10485760
ParseSize("10mb"): invalid_argument
ParseSize("10 MB"): invalid_argument
ParseSize("1.5B"): invalid_argument
ParseSize("-1KB"): invalid_argument
```

1. A DSN with `prefix=tenant-a&visibility=public&public_url=…` scopes the disk under `tenant-a/`, makes writes public, and the public URL gains the prefix too.
2. A misspelt parameter and an unregistered scheme fail when the disk is opened; the error names the scheme and never prints the DSN, which can carry credentials.
3. A disk opened with `readonly=true` refuses writes with `read_only`.
4. `OpenManager` reads `STORAGE_DISKS`, one `STORAGE_<NAME>_DSN` per disk and `STORAGE_DEFAULT_DISK`; `EnvName` gives the variable for a disk name with a dash.
5. `Check` writes, reads back and deletes on every disk; `CopyAcross` streams an object from the memory disk to the local one; an unknown disk name is `not_found`.
6. A missing DSN variable fails with the variable's name.
7. `ParseSize` reads human sizes from configuration (decimal and binary units) and refuses lower-case units, spaces, fractions of a byte and signs.

## The code

The Mux holds exactly the drivers this program imports; importing a driver package registers nothing.

<!-- ds:block id=newmux-6gp3nw3a -->
**newMux** · [`examples/concepts-configuration/main.go:68-77`](main.go#L68-L77)

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

The parameters every scheme accepts, and what they change.

<!-- ds:block id=openwithparams-e2jtzbhn -->
**openWithParams** · [`examples/concepts-configuration/main.go:83-101`](main.go#L83-L101)

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

Configuration mistakes fail at start-up.

<!-- ds:block id=refusals-2dba5qe4 -->
**refusals** · [`examples/concepts-configuration/main.go:107-119`](main.go#L107-L119)

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

Every disk from variables, with a map standing in for the process environment.

<!-- ds:block id=fromenvironment-tvhw2dwd -->
**fromEnvironment** · [`examples/concepts-configuration/main.go:125-144`](main.go#L125-L144)

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

The Manager's start-up check and a copy across two disks.

<!-- ds:block id=managerops-tze3ff3x -->
**managerOps** · [`examples/concepts-configuration/main.go:149-173`](main.go#L149-L173)

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

A missing variable is named in the error.

<!-- ds:block id=missingvariable-fv6t3a7d -->
**missingVariable** · [`examples/concepts-configuration/main.go:178-183`](main.go#L178-L183)

```go
func missingVariable(ctx context.Context, mux *filesystem.Mux, out io.Writer) {
	env := map[string]string{"STORAGE_DISKS": "media,backups", "STORAGE_MEDIA_DSN": "mem://"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	_, err := mux.OpenManager(ctx, envPrefix, lookup)
	fmt.Fprintf(out, "missing: %v\n", err)
}
```
<!-- /ds:block hash=f11242 -->

Human-readable sizes from configuration.

<!-- ds:block id=sizes-82ucppeh -->
**sizes** · [`examples/concepts-configuration/main.go:189-204`](main.go#L189-L204)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the Mux, DSN parameters, refusals, the Manager from the environment and its operations, and sizes |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Configuration](../../docs/concepts/configuration.md), and [reference/dsn](../../docs/reference/dsn.md) for every parameter of every adapter.
- [Production](../../docs/start/production.md): checking disks at start-up; example [start-production](../start-production).
- [Disks and drivers](../../docs/concepts/disks-and-drivers.md) and example [concepts-disks](../concepts-disks).
