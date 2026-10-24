# FTP driver

The FTP driver stores files on FTP and FTPS servers: hosting accounts, devices and partner drop boxes. This program runs an FTP server in-process, opens a partner's `/exchange` directory from a DSN whose password comes from an environment variable, uploads an order file atomically, moves it into an archive and copies it back (streamed, since FTP has no copy), runs six uploads at once over a pool of three connections, lists the result, shows that a directory disappears with its last file, shows that create-only writes, metadata and reserved keys are refused, previews and sweeps a staged file a crashed write left behind, and shows the DSNs the opener refuses. It uses the core `filesystem` package, `github.com/ubgo/filesystem/adapters/ftp`, and `github.com/fclairamb/ftpserverlib` (with its filesystem dependency) for the in-process server only. The guide [FTP](../../docs/adapters/ftp.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-ftp
go test ./adapters-ftp
```

It needs no remote host or environment variables; the served directory (`adapters-ftp-*` under the system temp dir) is deleted when the program exits, and the server's passive ports are 32000-32999. Against a real server the DSN is `ftps://<user>@<host>/<absolute path>?password_env=<VAR>`.

## What it does

```text
capabilities: [move,delete-prefix,sweep-abandoned,find-abandoned,dirs,case-insensitive]
uploaded: outbound/orders.csv 11 bytes text/csv; charset=utf-8
copied by streaming through the client: outbound/orders-resend.csv 11 bytes
six concurrent uploads over a pool of three connections: ok
listed: archive/2026-10/orders.csv
objects: 8
made returns/2026: exists=true; after deleting its only file, returns exists=false
conditional write: unsupported
metadata: unsupported
reserved key: invalid_key
abandoned: staging bbbbbbbbbbbbbbbbbbbbbbbbbb
swept: 1
refused DSN (password in the URL): invalid_argument
refused DSN (max_conns=0): invalid_argument
refused DSN (no root directory): invalid_argument
```

`case-insensitive` appears on macOS, whose temp directory folds letter case; on Linux it does not.

## The code

<!-- ds:block id=opendisk-k9z3v2xh -->
**openDisk** · [`examples/adapters-ftp/main.go:104-113`](main.go#L104-L113)

```go
func openDisk(ctx context.Context, addr string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(ftp.Scheme, ftp.Opener); err != nil {
		return nil, err
	}
	if err := mux.Register(ftp.SchemeSecure, ftp.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "partner", "ftp://"+ftpUser+"@"+addr+"/exchange?password_env="+passwordEnv+"&max_conns=3")
}
```
<!-- /ds:block hash=418fa2 -->

The upload, the move into the archive and the streamed copy:

<!-- ds:block id=uploadandarchive-qepk3c5b -->
**uploadAndArchive** · [`examples/adapters-ftp/main.go:148-163`](main.go#L148-L163)

```go
func uploadAndArchive(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	e, err := disk.Write(ctx, "outbound/orders.csv", strings.NewReader("id,qty\n1,3\n"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "uploaded: %s %d bytes %s\n", e.Key, e.Size, e.ContentType)
	if _, err := disk.Move(ctx, "outbound/orders.csv", "archive/2026-10/orders.csv"); err != nil {
		return err
	}
	c, err := disk.Copy(ctx, "archive/2026-10/orders.csv", "outbound/orders-resend.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "copied by streaming through the client: %s %d bytes\n", c.Key, c.Size)
	return nil
}
```
<!-- /ds:block hash=095ed6 -->
