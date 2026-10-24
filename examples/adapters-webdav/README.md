# WebDAV driver

The WebDAV driver stores files on Nextcloud, ownCloud, NAS boxes and any other WebDAV server. This program serves a temporary directory over WebDAV in-process (Basic authentication, Nextcloud's URL layout), opens the disk from a DSN whose password comes from an environment variable, stores a report, copies and moves it on the server, lists the result, makes a collection and shows that it outlives its files, shows that metadata, create-only writes and reserved keys are refused rather than dropped, previews and sweeps a staged file a crashed write left behind, shows the DSNs the opener refuses, and logs the driver's configuration with the password redacted. It uses the core `filesystem` package, `github.com/ubgo/filesystem/adapters/webdav`, and `golang.org/x/net/webdav` for the in-process server only. The guide [WebDAV](../../docs/adapters/webdav.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-webdav
go test ./adapters-webdav
```

It needs no remote server or environment variables; the served directory (`adapters-webdav-*` under the system temp dir) is deleted when the program exits. Against Nextcloud the DSN is `webdavs://<user>@<host>/remote.php/dav/files/<user>/<folder>?password_env=<VAR>`, with an app password in that variable.

## What it does

```text
capabilities: [copy,move,delete-prefix,sweep-abandoned,find-abandoned,dirs,case-insensitive]
stored: reports/2026-10.csv 18 bytes text/csv; charset=utf-8
listed: archive/2026/10.csv
listed: shared/latest.csv
made inbox/2026: exists=true, listed entries=0
after deleting its last file, shared exists=true
metadata: unsupported
if-not-exists: unsupported
reserved key: invalid_key
abandoned: staging aaaaaaaaaaaaaaaaaaaaaaaaaa
swept: 1
refused DSN (password in the URL): invalid_argument
refused DSN (plain http off loopback): invalid_argument
refused DSN (password_env without a user): invalid_argument
log line: level=INFO msg=opening webdav.endpoint=http://SERVER/remote.php/dav/files/alice/app webdav.username=alice webdav.password=[redacted]
```

`case-insensitive` appears on macOS, whose temp directory folds letter case; on Linux it does not.

## The code

<!-- ds:block id=opendisk-nf626p2g -->
**openDisk** · [`examples/adapters-webdav/main.go:71-80`](main.go#L71-L80)

```go
func openDisk(ctx context.Context, host string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(webdav.Scheme, webdav.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(webdav.SchemeSecure, webdav.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "cloud", "webdav://"+davUser+"@"+host+davPrefix+"/app?password_env="+passwordEnv)
}
```
<!-- /ds:block hash=c2c78a -->

The write, copy, move and listing:

<!-- ds:block id=storereport-mmrnx7vt -->
**storeReport** · [`examples/adapters-webdav/main.go:116-135`](main.go#L116-L135)

```go
func storeReport(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	e, err := disk.Write(ctx, "reports/2026-10.csv", strings.NewReader("month,total\n10,42\n"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored: %s %d bytes %s\n", e.Key, e.Size, e.ContentType)
	if _, err := disk.Copy(ctx, "reports/2026-10.csv", "shared/latest.csv"); err != nil {
		return err
	}
	if _, err := disk.Move(ctx, "reports/2026-10.csv", "archive/2026/10.csv"); err != nil {
		return err
	}
	for entry, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s\n", entry.Key)
	}
	return nil
}
```
<!-- /ds:block hash=32196c -->
