# SFTP driver

The SFTP driver stores files in a directory on an SSH server: the backend for partner exchanges, legacy hosts and anywhere a file drop is the integration. This program opens a disk from a DSN that names a `known_hosts` file and a private key, runs the partner-exchange flow (an atomic once-only write, a stat, a move into an archive, a listing), shows the options SFTP cannot store, and opens the same directory again from a Config over an SSH connection the application dialled itself. It uses the core `filesystem` package and `github.com/ubgo/filesystem/adapters/sftp` (built on `github.com/pkg/sftp` and `golang.org/x/crypto/ssh`). The guide [SFTP](../../docs/adapters/sftp.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-sftp
go test ./adapters-sftp
```

It starts an in-process SSH server with the SFTP subsystem on a random loopback port, serving a temporary directory (`adapters-sftp-*` under the system temp dir). Fresh ed25519 host and client keys are generated per run; the client key is written to `id_ed25519` and the host key line to `known_hosts` in that directory, exactly the two files a deployment keeps outside the DSN. It needs no remote host or environment variables; the server stops and the directory is deleted when the program exits. Against a real server, the DSN is `sftp://<user>@<host>:22/<absolute path>?known_hosts=<path>&identity=<path>`.

## What it does

```text
capabilities: [move,set-visibility,delete-prefix,sweep-abandoned,find-abandoned,dirs,conditional-write]
second conditional write: exists
stat: outbound/orders.csv 14 bytes private (type from extension: text/csv; charset=utf-8)
listed: archive/2026-10/orders.csv
metadata: unsupported
headers: unsupported
via Config: archived file exists=true
```

1. The disk opens from a DSN: the host key is verified against `known_hosts` and the login uses the private key file. There is no copy, multipart or signed URL over SFTP.
2. A CSV is dropped in `outbound/` with `IfNotExists`; the second attempt fails with `exists`.
3. `Stat` reports the size, visibility from the file mode (private), and a content type guessed from the extension, since SFTP stores none.
4. The file is moved into `archive/2026-10/` with an atomic rename, and the recursive listing shows only the archived copy.
5. Metadata and headers are `unsupported`, refused rather than dropped.
6. A second disk built from a Config over a hand-dialled SSH connection sees the archived file.

## The code

The DSN carries no secret, only the paths of the files that hold them; the path after the host is absolute on the server.

<!-- ds:block id=sftpdsn-3chvpnn6 -->
**sftpDSN** · [`examples/adapters-sftp/main.go:96-101`](main.go#L96-L101)

```go
func sftpDSN(addr, root string, creds credentials) string {
	q := url.Values{}
	q.Set(fssftp.ParamKnownHosts, creds.KnownHosts)
	q.Set(fssftp.ParamIdentity, creds.Identity)
	return fssftp.Scheme + "://" + sftpUser + "@" + addr + root + "?" + q.Encode()
}
```
<!-- /ds:block hash=5751e7 -->

Dial, verify the host key, log in and open the directory as a disk.

<!-- ds:block id=openwithdsn-n7y5ygk9 -->
**openWithDSN** · [`examples/adapters-sftp/main.go:106-112`](main.go#L106-L112)

```go
func openWithDSN(ctx context.Context, addr, root string, creds credentials) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fssftp.Scheme, fssftp.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "partner", sftpDSN(addr, root, creds))
}
```
<!-- /ds:block hash=2dd9bf -->

The partner-exchange flow: drop atomically, refuse to overwrite, stat, archive with a rename, list.

<!-- ds:block id=dropandpickup-hrcthtrj -->
**dropAndPickUp** · [`examples/adapters-sftp/main.go:118-140`](main.go#L118-L140)

```go
func dropAndPickUp(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	report := []byte("sku,qty\nA-1,4\n")
	if _, err := disk.WriteBytes(ctx, "outbound/orders.csv", report, filesystem.WriteOptions{IfNotExists: true}); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "outbound/orders.csv", report, filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	e, err := disk.Stat(ctx, "outbound/orders.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %d bytes %s (type from extension: %s)\n", e.Key, e.Size, e.Visibility, e.ContentType)
	if _, err := disk.Move(ctx, "outbound/orders.csv", "archive/2026-10/orders.csv"); err != nil {
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
<!-- /ds:block hash=91df28 -->

What SFTP cannot store, as error codes.

<!-- ds:block id=refusedoptions-kha3m599 -->
**refusedOptions** · [`examples/adapters-sftp/main.go:145-151`](main.go#L145-L151)

```go
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=b6fce9 -->

For an application that manages its own SSH connection (a jump host, an agent, keep-alives): hand the SFTP client to `New` with a `Closer`, so `Disk.Close` closes the connection.

<!-- ds:block id=openwithconfig-k4zbz2j8 -->
**openWithConfig** · [`examples/adapters-sftp/main.go:157-186`](main.go#L157-L186)

```go
func openWithConfig(ctx context.Context, out io.Writer, addr, root string, creds credentials) error {
	hostKeys, err := knownhosts.New(creds.KnownHosts)
	if err != nil {
		return err
	}
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            sftpUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(creds.clientKey)},
		HostKeyCallback: hostKeys,
	})
	if err != nil {
		return err
	}
	client, err := sftp.NewClient(conn)
	if err != nil {
		return errors.Join(err, conn.Close())
	}
	drv, err := fssftp.New(fssftp.Config{Client: client, Root: root, Closer: conn})
	if err != nil {
		return errors.Join(err, client.Close(), conn.Close())
	}
	disk, err := filesystem.NewDisk("partner-config", drv)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	ok, err := disk.Exists(ctx, "archive/2026-10/orders.csv")
	fmt.Fprintf(out, "via Config: archived file exists=%t\n", ok)
	return err
}
```
<!-- /ds:block hash=7f517c -->

The `known_hosts` line `ssh-keyscan` would print for the server; without a matching line the driver refuses to connect.

<!-- ds:block id=writeknownhosts-jg44gy3b -->
**writeKnownHosts** · [`examples/adapters-sftp/main.go:216-219`](main.go#L216-L219)

```go
func writeKnownHosts(c credentials, addr string) error {
	line := knownhosts.Line([]string{addr}, c.hostKey.PublicKey())
	return os.WriteFile(c.KnownHosts, []byte(line+"\n"), 0o600)
}
```
<!-- /ds:block hash=5220d7 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: key generation, the in-process SSH/SFTP server, DSN and Config disks, the exchange flow, refusals |
| `main_test.go` | Runs the program against a fresh server and checks each line the guide quotes, including that the moved file is gone from `outbound/` |

## Next

- The guide: [SFTP](../../docs/adapters/sftp.md), and the driver reference [adapters/sftp/README.md](../../adapters/sftp/README.md).
- Other backends: [local](../adapters-local), [memory](../adapters-memory), [S3](../adapters-s3), [GCS](../adapters-gcs), [Azure](../adapters-azure).
- [Errors](../concepts-errors) for the codes such as `exists` and `unsupported`.
