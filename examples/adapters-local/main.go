// Command adapters-local walks through the local-disk driver: opening a
// directory from a DSN and with New, visibility as file modes, atomic and
// conditional writes, a server-side multipart upload, the reserved staging
// directory, the names Windows reserves, signed URLs served by the
// application (signedurl + fshttp), public URLs from a base, what local disk
// cannot store, and the durability levels that trade fsync cost for safety.
//
//	go run ./adapters-local            (from the examples directory)
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/signedurl"
)

// Modes for a directory served by a web server running as another user in
// the same group: public files group- and world-readable, private files
// group-readable only. The public mode must keep the world-read bit and
// the private one must not, because Stat reads visibility from that bit.
const (
	filePublic  fs.FileMode = 0o644
	filePrivate fs.FileMode = 0o640
	dirMode     fs.FileMode = 0o750
)

// signingSecret is the HMAC key for signed URLs: at least
// signedurl.MinKeyBytes of random bytes, from a secret store in production.
var signingSecret = bytes.Repeat([]byte("k"), signedurl.MinKeyBytes)

// cdnBase is the public base URL the example's public disk serves from.
const cdnBase = "https://cdn.example.com/media"

// linkLifetime is how long the signed URLs in this program stay valid.
const linkLifetime = 5 * time.Minute

func main() {
	dir, err := os.MkdirTemp("", "adapters-local-")
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

// run is main without the process: dir is the directory the disks live
// in, and every step prints what it saw to out.
func run(ctx context.Context, out io.Writer, dir string) error {
	disk, err := openWithNew(filepath.Join(dir, "uploads"))
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	steps := []func(context.Context, io.Writer, *filesystem.Disk) error{
		modes, conditionalAndMove, serverMultipart, refusedKeysAndOptions, reservedNames,
	}
	for _, step := range steps {
		if err := step(ctx, out, disk); err != nil {
			return err
		}
	}
	if err := signedDownload(ctx, out, filepath.Join(dir, "private")); err != nil {
		return err
	}
	if err := publicURLs(ctx, out, filepath.Join(dir, "public")); err != nil {
		return err
	}
	if err := durabilityLevels(ctx, out, filepath.Join(dir, "cache")); err != nil {
		return err
	}
	return fixedTypes(ctx, out, filepath.Join(dir, "scans"))
}

// openWithNew opens (creating it if missing) a directory with custom file
// modes. Every operation then goes through an os.Root, so nothing outside
// the directory is reachable, symlinks included.
// ds:def id=openwithnew-nqjh7jsb
func openWithNew(dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir, local.WithPermissions(filePublic, filePrivate, dirMode))
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("uploads", drv)
}

// modes writes a public and a private file and shows the mode on disk and
// the visibility Stat reads back from it.
// ds:def id=modes-snss4cwu
func modes(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.WriteBytes(ctx, "site/logo.svg", []byte("<svg/>"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "users/u1/id.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	root, ok := filesystem.As[*os.Root](disk)
	if !ok {
		return fmt.Errorf("disk %s is not a local disk", disk.Name())
	}
	for _, k := range []string{"site/logo.svg", "users/u1/id.pdf"} {
		info, err := root.Stat(k)
		if err != nil {
			return err
		}
		e, err := disk.Stat(ctx, k)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: mode %v, %s, %s\n", k, info.Mode().Perm(), e.Visibility, e.ContentType)
	}
	return nil
}

// conditionalAndMove writes a file only if it is absent (a hard link,
// atomic), then renames it atomically.
// ds:def id=conditionalandmove-es5n5qrq
func conditionalAndMove(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	once := filesystem.WriteOptions{IfNotExists: true}
	if _, err := disk.WriteBytes(ctx, "locks/job-7", []byte("worker-a"), once); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "locks/job-7", []byte("worker-b"), once)
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	if _, err := disk.Move(ctx, "locks/job-7", "done/job-7"); err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "done/job-7")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "moved: done/job-7 holds %q\n", got)
	return nil
}

// serverMultipart uploads two parts through the server and assembles them.
// Parts live under the reserved staging directory until completion.
// ds:def id=servermultipart-cqu8hk6y
func serverMultipart(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	const key = "exports/data.csv"
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	var parts []filesystem.Part
	for i, chunk := range []string{"id,total\n", "1,42\n"} {
		p, err := disk.UploadPart(ctx, key, id, i+1, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			return err
		}
		parts = append(parts, p)
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %s is %d bytes\n", e.Key, e.Size)
	return nil
}

// refusedKeysAndOptions shows the reserved staging directory refused as a
// key, and the options local disk cannot store refused rather than lost.
// ds:def id=refusedkeysandoptions-2u29xn83
func refusedKeysAndOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, local.StagingDir+"/tmp/x", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "staging key: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"k": "v"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	return nil
}

// openSigned opens a local disk from a DSN with a URL signer, so
// SignedURL works: the URLs point at the application, where fshttp checks
// the signature and streams the file.
// ds:def id=opensigned-8jywgss3
func openSigned(ctx context.Context, dir, base string) (*filesystem.Disk, *signedurl.Signer, error) {
	signer, err := signedurl.New(base, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, nil, err
	}
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return nil, nil, err
	}
	disk, err := mux.Open(ctx, "private", local.Scheme+"://"+filepath.ToSlash(dir), filesystem.WithURLSigner(signer))
	return disk, signer, err
}

// signedDownload serves fshttp.Signed under /files and downloads a private
// file through a signed URL.
// ds:def id=signeddownload-373dhs6j
func signedDownload(ctx context.Context, out io.Writer, dir string) error {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	disk, signer, err := openSigned(ctx, dir, srv.URL+"/files")
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	srv.Config.Handler = http.StripPrefix("/files", fshttp.Signed(mgr, signer))
	if _, err := disk.WriteBytes(ctx, "contracts/c1.txt", []byte("signed copy"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	link, err := disk.SignedURL(ctx, "contracts/c1.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, err := http.Get(link.URL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "signed GET: %d %q\n", resp.StatusCode, body)
	return err
}

// publicURLs opens a disk whose public files are served by a web server or
// CDN at cdnBase: the public_url DSN parameter makes PublicURL return
// base + key without touching the disk.
// ds:def id=publicurls-t4f5xshw
func publicURLs(ctx context.Context, out io.Writer, dir string) error {
	mux := filesystem.NewMux()
	if err := mux.Register(local.Scheme, local.Opener); err != nil {
		return err
	}
	q := url.Values{}
	q.Set(filesystem.ParamPublicURL, cdnBase)
	q.Set(filesystem.ParamVisibility, string(filesystem.VisibilityPublic))
	disk, err := mux.Open(ctx, "public", local.Scheme+"://"+filepath.ToSlash(dir)+"?"+q.Encode())
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	u, err := disk.PublicURL(ctx, "img/a b.png")
	fmt.Fprintf(out, "public URL: %s\n", u)
	return err
}

// reservedNames shows a key Windows reserves (a device name such as CON,
// whatever its extension) refused on every system by default, so that a
// directory written on Linux or macOS can be copied to Windows; a name that
// merely starts like one is fine.
// ds:def id=reservednames-jp8rg5h9
func reservedNames(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	for _, k := range []string{"exports/con.txt", "exports/console.txt"} {
		_, err := disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{})
		code := filesystem.CodeOf(err)
		if err == nil {
			code = "ok"
		}
		fmt.Fprintf(out, "write %s: %s\n", k, code)
	}
	return nil
}

// cacheDurability is the level for a directory whose files can be rebuilt
// (thumbnails, a download cache): no fsync at all, so writes cost only the
// copy, and a crash may leave a recent file missing, empty or truncated.
const cacheDurability = local.DurabilityNone

// openCache opens a cache directory that skips every fsync. Keep the
// default DurabilityFull for anything you cannot rebuild; DurabilityFile
// sits between the two (the file is synced, its directory entry is not).
// ds:def id=opencache-ymvzq9g7
func openCache(dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir, local.WithDurability(cacheDurability))
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("cache", drv)
}

// durabilityLevels lists the levels, writes through a disk that syncs
// nothing, and shows that New refuses a level outside the closed set.
// ds:def id=durabilitylevels-x36jn8tm
func durabilityLevels(ctx context.Context, out io.Writer, dir string) error {
	fmt.Fprintf(out, "durability levels: %v\n", local.DurabilityValues)
	cache, err := openCache(dir)
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()
	e, err := cache.WriteBytes(ctx, "thumbs/p1.txt", []byte("rebuildable"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cache write (%s): %s is %d bytes\n", cacheDurability, e.Key, e.Size)
	_, err = local.New(dir, local.WithDurability("fast"))
	fmt.Fprintf(out, "unknown durability: %s\n", filesystem.CodeOf(err))
	return nil
}
