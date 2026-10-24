// Command adapters-webdav walks through the WebDAV driver: opening a disk
// from a DSN whose password comes from the environment, a staged write,
// a server-side copy and move, a listing, what WebDAV cannot store, and a
// logged configuration that keeps the password out of the log.
//
// It serves a temporary directory over WebDAV in-process, with Basic
// authentication, laid out the way Nextcloud serves a user's files, so it
// needs no remote server.
//
//	go run ./adapters-webdav            (from the examples directory)
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	xwebdav "golang.org/x/net/webdav"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/webdav"
)

// The account the in-process server accepts, and the environment variable
// the DSN names for the password.
const (
	davUser     = "alice"
	davPassword = "app-password-123"
	passwordEnv = "NEXTCLOUD_APP_PASSWORD"
	davPrefix   = "/remote.php/dav/files/" + davUser
)

func main() {
	dir, err := os.MkdirTemp("", "adapters-webdav-")
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

// startServer serves dir over WebDAV under davPrefix, refusing requests
// without the account's Basic credentials.
func startServer(dir string) *httptest.Server {
	h := &xwebdav.Handler{Prefix: davPrefix, FileSystem: xwebdav.Dir(dir), LockSystem: xwebdav.NewMemLS()}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != davUser || p != davPassword {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
}

// openDisk opens the "app" folder of the account from a DSN. The password
// is read from the environment variable the DSN names; a webdavs:// DSN is
// the same over https.
// ds:def id=opendisk-nf626p2g
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

// run is main without the process: dir is the served directory, and every
// step prints what it saw to out.
func run(ctx context.Context, out io.Writer, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o700); err != nil {
		return err
	}
	srv := startServer(dir)
	defer srv.Close()
	if err := os.Setenv(passwordEnv, davPassword); err != nil {
		return err
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	disk, err := openDisk(ctx, host)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	for _, step := range []func(context.Context, io.Writer, *filesystem.Disk) error{storeReport, directories, refusedWrites} {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	if err := sweepStaged(ctx, out, disk, filepath.Join(dir, "app")); err != nil {
		return err
	}
	refusedDSNs(ctx, out, host)
	logConfig(out, srv.URL)
	return nil
}

// storeReport writes a report (staged, then moved into place), copies and
// moves it on the server, and lists what is left.
// ds:def id=storereport-mmrnx7vt
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

// directories makes an empty collection, shows that a listing never
// returns it, and that deleting the last file under a collection keeps it.
// ds:def id=directories-uqurjtmh
func directories(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if err := disk.MakeDir(ctx, "inbox/2026"); err != nil {
		return err
	}
	made, err := disk.DirExists(ctx, "inbox/2026")
	if err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "inbox", filesystem.ListOptions{Recursive: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "made inbox/2026: exists=%t, listed entries=%d\n", made, len(page.Entries))
	if err := disk.Delete(ctx, "shared/latest.csv"); err != nil {
		return err
	}
	kept, err := disk.DirExists(ctx, "shared")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after deleting its last file, shared exists=%t\n", kept)
	return disk.DeletePrefix(ctx, "shared")
}

// refusedWrites shows the writes WebDAV refuses rather than half-honours:
// metadata it cannot store, a create-only write it cannot make atomic, and
// a key inside the driver's staging collection.
// ds:def id=refusedwrites-k5jge7dv
func refusedWrites(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	body := func() io.Reader { return strings.NewReader("x") }
	_, err := disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, "x.txt", body(), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "if-not-exists: %s\n", filesystem.CodeOf(err))
	_, err = disk.Write(ctx, webdav.StagingDir+"/mine.txt", body(), filesystem.WriteOptions{})
	fmt.Fprintf(out, "reserved key: %s\n", filesystem.CodeOf(err))
	return nil
}

// sweepStaged plants the file a write leaves behind when its process dies
// between the PUT and the MOVE (a staged name, a day old), previews it
// with FindAbandoned and removes it with SweepAbandoned. appDir is the
// served folder of the disk.
// ds:def id=sweepstaged-8xge4twb
func sweepStaged(ctx context.Context, out io.Writer, disk *filesystem.Disk, appDir string) error {
	staged := filepath.Join(appDir, webdav.StagingDir, "tmp", strings.Repeat("a", 26))
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

// refusedDSNs opens three DSNs the opener refuses before any request: a
// password in the URL, plain http to a host that is not loopback, and
// password_env without a user.
// ds:def id=refuseddsns-2pq3n5d3
func refusedDSNs(ctx context.Context, out io.Writer, host string) {
	mux := filesystem.NewMux()
	_ = mux.Register(webdav.Scheme, webdav.Opener)
	_ = mux.Register(webdav.SchemeSecure, webdav.Opener)
	for _, c := range []struct{ why, dsn string }{
		{"password in the URL", "webdavs://alice:secret@cloud.example.com/remote.php/dav/files/alice"},
		{"plain http off loopback", "webdav://alice@nas.internal/dav?password_env=" + passwordEnv},
		{"password_env without a user", "webdav://" + host + davPrefix + "?password_env=" + passwordEnv},
	} {
		_, err := mux.Open(ctx, "refused", c.dsn)
		fmt.Fprintf(out, "refused DSN (%s): %s\n", c.why, filesystem.CodeOf(err))
	}
}

// logConfig logs a Config the way an application logs its settings: the
// password comes out redacted.
func logConfig(out io.Writer, serverURL string) {
	var log bytes.Buffer
	cfg := webdav.Config{Endpoint: serverURL + davPrefix + "/app", Username: davUser, Password: davPassword}
	slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{ReplaceAttr: dropTime})).Info("opening", "webdav", cfg)
	fmt.Fprintf(out, "log line: %s", strings.ReplaceAll(log.String(), serverURL, "http://SERVER"))
}

// dropTime removes the timestamp, so the printed log line is the same on
// every run.
func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}
