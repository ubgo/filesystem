// Command adapters-ftp walks through the FTP driver: opening a disk from a
// DSN whose password comes from the environment, an atomic upload into a
// partner's drop folder, a move into an archive, a listing, what FTP
// cannot store, and the connection pool serving concurrent calls.
//
// It runs an FTP server in-process (github.com/fclairamb/ftpserverlib over
// a temporary directory), so it needs no remote host.
//
//	go run ./adapters-ftp            (from the examples directory)
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/ftp"
)

// The account the in-process server accepts, and the environment variable
// the DSN names for the password.
const (
	ftpUser     = "partner"
	ftpPassword = "drop-box-pass"
	passwordEnv = "PARTNER_FTP_PASSWORD"
)

// Passive data ports of the in-process server, below the ephemeral range.
const (
	passivePortStart = 32000
	passivePortEnd   = 32999
)

func main() {
	dir, err := os.MkdirTemp("", "adapters-ftp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// server is the in-process FTP server's configuration: one account whose
// files live in dir.
type server struct {
	dir      string
	settings *ftpserver.Settings
}

func (s *server) GetSettings() (*ftpserver.Settings, error)               { return s.settings, nil }
func (s *server) ClientConnected(ftpserver.ClientContext) (string, error) { return "drop box", nil }
func (s *server) ClientDisconnected(ftpserver.ClientContext)              {}
func (s *server) GetTLSConfig() (*tls.Config, error)                      { return nil, errors.New("no TLS") }

func (s *server) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != ftpUser || pass != ftpPassword {
		return nil, errors.New("bad credentials")
	}
	return afero.NewBasePathFs(afero.NewOsFs(), s.dir), nil
}

// startServer serves dir over FTP on a loopback port and returns its
// address and how to stop it.
func startServer(dir string) (string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := ftpserver.NewFtpServer(&server{dir: dir, settings: &ftpserver.Settings{
		Listener:                 ln,
		PublicHost:               "127.0.0.1",
		DefaultTransferType:      ftpserver.TransferTypeBinary,
		PassiveTransferPortRange: &ftpserver.PortRange{Start: passivePortStart, End: passivePortEnd},
	}})
	srv.Logger = slog.New(slog.DiscardHandler)
	if err := srv.Listen(); err != nil {
		return "", nil, err
	}
	go func() { _ = srv.Serve() }()
	return ln.Addr().String(), func() { _ = srv.Stop() }, nil
}

// openDisk opens the partner's /exchange directory from a DSN. The
// password is read from the environment variable the DSN names; ftps://
// is the same over explicit TLS.
// ds:def id=opendisk-k9z3v2xh
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

// run is main without the process: dir is the served directory, and every
// step prints what it saw to out.
func run(ctx context.Context, out io.Writer, dir string) error {
	addr, stop, err := startServer(dir)
	if err != nil {
		return err
	}
	defer stop()
	if err := os.Setenv(passwordEnv, ftpPassword); err != nil {
		return err
	}
	disk, err := openDisk(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	for _, step := range []func(context.Context, io.Writer, *filesystem.Disk) error{uploadAndArchive, concurrentUploads, listArchive, directories, refusedWrites} {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	if err := sweepStaged(ctx, out, disk, filepath.Join(dir, "exchange")); err != nil {
		return err
	}
	refusedDSNs(ctx, out, addr)
	return nil
}

// uploadAndArchive uploads an order file into the partner's outbound
// folder (staged, then renamed into place), moves it into the archive with
// a rename, and copies it back, which FTP cannot do on the server.
// ds:def id=uploadandarchive-qepk3c5b
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

// concurrentUploads runs six writes at once over the disk's pool of three
// connections; each call waits for a free connection.
// ds:def id=concurrentuploads-v6mkjfcz
func concurrentUploads(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = disk.Write(ctx, fmt.Sprintf("inbound/batch-%d.txt", i), strings.NewReader("line\n"), filesystem.WriteOptions{})
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Fprintln(out, "six concurrent uploads over a pool of three connections: ok")
	return nil
}

// listArchive walks the whole disk, printing the archive and counting the
// rest.
func listArchive(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	n := 0
	for entry, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Key, "archive/") {
			fmt.Fprintf(out, "listed: %s\n", entry.Key)
		}
		n++
	}
	fmt.Fprintf(out, "objects: %d\n", n)
	return nil
}

// directories makes a directory, then shows that FTP behaves like local
// disk: deleting the last file under a directory removes the directory.
// ds:def id=directories-7d7jwn8h
func directories(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if err := disk.MakeDir(ctx, "returns/2026"); err != nil {
		return err
	}
	made, err := disk.DirExists(ctx, "returns/2026")
	if err != nil {
		return err
	}
	if _, err := disk.Write(ctx, "returns/2026/r1.csv", strings.NewReader("id\n7\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if err := disk.Delete(ctx, "returns/2026/r1.csv"); err != nil {
		return err
	}
	kept, err := disk.DirExists(ctx, "returns")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "made returns/2026: exists=%t; after deleting its only file, returns exists=%t\n", made, kept)
	return nil
}

// refusedWrites shows the writes FTP refuses rather than half-honours: a
// create-only write (no FTP command creates a file only if absent),
// metadata it cannot store, and a key inside the staging directory.
// ds:def id=refusedwrites-7qbzqw3p
func refusedWrites(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	body := func() io.Reader { return strings.NewReader("x") }
	_, err := disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "conditional write: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, ftp.StagingDir+"/mine.txt", body(), filesystem.WriteOptions{})
	fmt.Fprintf(out, "reserved key: %s\n", filesystem.CodeOf(err))
	return nil
}

// sweepStaged plants the file a write leaves behind when its process dies
// between the STOR and the rename (a staged name, a day old), previews it
// with FindAbandoned and removes it with SweepAbandoned. rootDir is the
// served directory of the disk's root.
// ds:def id=sweepstaged-9ux5wq6y
func sweepStaged(ctx context.Context, out io.Writer, disk *filesystem.Disk, rootDir string) error {
	staged := filepath.Join(rootDir, ftp.StagingDir, "tmp", strings.Repeat("b", 26))
	if err := os.WriteFile(staged, []byte("half an upload"), 0o600); err != nil {
		return err
	}
	dayAgo := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(staged, dayAgo, dayAgo); err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Hour)
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "abandoned: %s %s\n", a.Kind, a.ID)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept: %d\n", n)
	return nil
}

// refusedDSNs opens DSNs the opener refuses before it dials: a password
// in the URL, a pool of zero connections, and no root directory.
// ds:def id=refuseddsns-j46sn7gr
func refusedDSNs(ctx context.Context, out io.Writer, addr string) {
	mux := filesystem.NewMux()
	_ = mux.Register(ftp.Scheme, ftp.Opener)
	_ = mux.Register(ftp.SchemeSecure, ftp.Opener)
	for _, c := range []struct{ why, dsn string }{
		{"password in the URL", "ftp://" + ftpUser + ":secret@" + addr + "/exchange"},
		{"max_conns=0", "ftp://" + ftpUser + "@" + addr + "/exchange?max_conns=0&password_env=" + passwordEnv},
		{"no root directory", "ftp://" + ftpUser + "@" + addr + "?password_env=" + passwordEnv},
	} {
		_, err := mux.Open(ctx, "refused", c.dsn)
		fmt.Fprintf(out, "refused DSN (%s): %s\n", c.why, filesystem.CodeOf(err))
	}
}
