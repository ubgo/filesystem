// Command adapters-consumer-drives walks through the three consumer-drive
// drivers, Google Drive, Dropbox and OneDrive, the backends an application
// uses when files belong in the customer's own storage rather than in a
// bucket the application owns. For each drive it opens a filesystem.Disk,
// writes, reads, stats and lists a file, tries the conditional writes the
// drive declares, copies and moves, streams a large body through the
// drive's upload session, shows what the drive refuses (signed URLs,
// visibility, metadata), and maps a failing token source and a rate limit
// to the library's error codes.
//
// It runs against small in-process fakes of the three HTTP APIs
// (internal/fakes), so it needs no account, no OAuth flow and no network.
//
//	go run ./adapters-consumer-drives            (from the examples directory)
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/dropbox"
	"github.com/ubgo/filesystem/adapters/gdrive"
	"github.com/ubgo/filesystem/adapters/onedrive"

	"github.com/ubgo/filesystem/examples/adapters-consumer-drives/internal/fakes"
)

// largeSize is the size of the body that goes through an upload session:
// more than one chunk on every drive at the small chunk sizes set below.
const largeSize = 800 << 10

// rateLimitWait is the Retry-After, in seconds, the throttled transport
// answers with.
const rateLimitWait = 7

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// tokenFunc is the function every driver's TokenFunc adapts: it returns the
// current OAuth access token, refreshing it when needed.
type tokenFunc = func(context.Context) (string, error)

// validToken returns the token the fakes accept. A real application returns
// its oauth2.TokenSource's current AccessToken here.
func validToken(context.Context) (string, error) { return fakes.Token, nil }

// drive is one consumer drive under test: how to open a disk on it with a
// given token source and HTTP client (nil means the driver's default), and
// how many upload-session requests its fake has seen.
type drive struct {
	name   string
	open   func(tok tokenFunc, client *http.Client) (*filesystem.Disk, error)
	chunks func() int
}

// run is main without the process: it starts the three fakes, walks each
// drive and prints what it saw to out.
func run(ctx context.Context, out io.Writer) error {
	gd, db, od := fakes.NewGDrive(), fakes.NewDropbox(), fakes.NewOneDrive()
	defer gd.Close()
	defer db.Close()
	defer od.Close()
	drives := []drive{
		{"gdrive", func(t tokenFunc, c *http.Client) (*filesystem.Disk, error) { return openGDrive(gd, t, c) }, gd.Chunks},
		{"dropbox", func(t tokenFunc, c *http.Client) (*filesystem.Disk, error) { return openDropbox(db, t, c) }, db.Chunks},
		{"onedrive", func(t tokenFunc, c *http.Client) (*filesystem.Disk, error) { return openOneDrive(od, t, c) }, od.Chunks},
	}
	for _, d := range drives {
		if err := walk(ctx, out, d); err != nil {
			return fmt.Errorf("%s: %w", d.name, err)
		}
	}
	return nil
}

// walk runs every step on one drive.
func walk(ctx context.Context, out io.Writer, d drive) error {
	disk, err := d.open(validToken, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s capabilities: %s\n", d.name, disk.Capabilities())
	steps := []func(context.Context, io.Writer, string, *filesystem.Disk) error{basics, conditional, copyAndMove, refusals}
	for _, step := range steps {
		if err := step(ctx, out, d.name, disk); err != nil {
			return err
		}
	}
	if err := largeBody(ctx, out, d, disk); err != nil {
		return err
	}
	return failures(ctx, out, d)
}

// openGDrive opens a disk on a Google Drive folder.
// ds:def id=opengdrive-g6g8ay6b
func openGDrive(f *fakes.GDrive, tok tokenFunc, client *http.Client) (*filesystem.Disk, error) {
	drv, err := gdrive.New(gdrive.Config{
		// The folder the application created for itself; with the
		// drive.file scope it sees nothing else.
		FolderID: fakes.GDriveFolderID,
		Token:    gdrive.TokenFunc(tok),
		// Small chunks so the large body below spans several requests;
		// leave ChunkSize at 0 (8 MiB) in production.
		ChunkSize: gdrive.ChunkUnit,
		Client:    client,
		// Point at the fake; omit both for the real API.
		APIURL:    f.APIURL(),
		UploadURL: f.UploadURL(),
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("gdrive", drv)
}

// openDropbox opens a disk on a folder of a Dropbox account.
// ds:def id=opendropbox-4bkc6hzd
func openDropbox(f *fakes.Dropbox, tok tokenFunc, client *http.Client) (*filesystem.Disk, error) {
	drv, err := dropbox.New(dropbox.Config{
		// Keys live under this folder; empty means the account root (or the
		// app folder, for an app-folder app).
		Root:  "/Apps/Invoices",
		Token: dropbox.TokenFunc(tok),
		// Bodies longer than one chunk go through an upload session; leave
		// ChunkSize at 0 (8 MiB) in production.
		ChunkSize: gdrive.ChunkUnit,
		Client:    client,
		// Point at the fake; omit both for api.dropboxapi.com and
		// content.dropboxapi.com.
		APIURL:     f.APIURL(),
		ContentURL: f.APIURL(),
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("dropbox", drv)
}

// openOneDrive opens a disk on a folder of the signed-in user's OneDrive.
// ds:def id=openonedrive-4xmy77th
func openOneDrive(f *fakes.OneDrive, tok tokenFunc, client *http.Client) (*filesystem.Disk, error) {
	drv, err := onedrive.New(onedrive.Config{
		// Any Graph drive: /me/drive, /drives/{id}, /sites/{id}/drive.
		Drive: "/me/drive",
		Root:  "Apps/Invoices",
		Token: onedrive.TokenFunc(tok),
		// Fragments must be multiples of FragmentUnit; leave ChunkSize at
		// 0 (10 MiB) in production.
		ChunkSize: onedrive.FragmentUnit,
		Client:    client,
		// Point at the fake; omit for graph.microsoft.com.
		BaseURL: f.BaseURL(),
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("onedrive", drv)
}

// outcome names an error's code, or "ok" for none.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	return string(filesystem.CodeOf(err))
}

// keys lists every key under dir.
func keys(ctx context.Context, disk *filesystem.Disk, dir string) ([]string, error) {
	var out []string
	for e, err := range disk.List(ctx, dir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return nil, err
		}
		out = append(out, e.Key)
	}
	return out, nil
}

// basics writes, reads, stats and lists one file, then stats it in
// another letter case.
// ds:def id=basics-bcgfabev
func basics(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "notes/todo.txt", []byte("buy milk\n"), filesystem.WriteOptions{ContentType: "text/plain"})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "notes/todo.txt")
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "notes/todo.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: read %q, stat size=%d etag set=%t type=%q\n", name, got, e.Size, e.ETag != "", e.ContentType)
	_, err = disk.Stat(ctx, "NOTES/TODO.TXT")
	fmt.Fprintf(out, "%s: stat NOTES/TODO.TXT: %s\n", name, outcome(err))
	listed, err := keys(ctx, disk, "notes")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: list notes: %v\n", name, listed)
	return nil
}

// conditional tries create-only and compare-and-swap writes. A drive that
// does not declare one refuses it with CodeUnsupported before any request.
// ds:def id=conditional-n68fgurx
func conditional(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "notes/todo.txt", []byte("overwrite?"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "%s: IfNotExists on an existing key: %s\n", name, outcome(err))
	e, err := disk.Stat(ctx, "notes/todo.txt")
	if err != nil {
		return err
	}
	_, current := disk.WriteBytes(ctx, "notes/todo.txt", []byte("buy milk and eggs\n"), filesystem.WriteOptions{IfMatch: e.ETag})
	_, stale := disk.WriteBytes(ctx, "notes/todo.txt", []byte("lost update\n"), filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "%s: IfMatch current etag: %s, same etag again: %s\n", name, outcome(current), outcome(stale))
	return nil
}

// copyAndMove copies a file into another folder, then renames the copy.
// Without the Copy capability the Disk streams the bytes through the
// application.
// ds:def id=copyandmove-ydqujfaq
func copyAndMove(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	if _, err := disk.Copy(ctx, "notes/todo.txt", "archive/todo.txt"); err != nil {
		return err
	}
	if _, err := disk.Move(ctx, "archive/todo.txt", "archive/2026/todo.txt"); err != nil {
		return err
	}
	listed, err := keys(ctx, disk, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: after copy and move: %v\n", name, listed)
	return nil
}

// refusals shows what no consumer drive offers through the Disk.
// ds:def id=refusals-jn55w6u7
func refusals(ctx context.Context, out io.Writer, name string, disk *filesystem.Disk) error {
	_, signed := disk.SignedURL(ctx, "notes/todo.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	_, public := disk.WriteBytes(ctx, "share.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	_, meta := disk.WriteBytes(ctx, "tagged.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"owner": "42"}})
	fmt.Fprintf(out, "%s: signed URL: %s, public write: %s, metadata: %s\n", name, outcome(signed), outcome(public), outcome(meta))
	return nil
}

// largeBody streams a body longer than one chunk, with no size hint,
// through the drive's upload session, then tries a compare-and-swap of a
// body that long.
// ds:def id=largebody-ktqgtqdb
func largeBody(ctx context.Context, out io.Writer, d drive, disk *filesystem.Disk) error {
	body := bytes.Repeat([]byte("0123456789abcdef"), largeSize/16)
	// io.MultiReader hides the length, as an HTTP request body does, so
	// the driver cannot know the size up front.
	before := d.chunks()
	e, err := disk.Write(ctx, "backups/large.bin", io.MultiReader(bytes.NewReader(body)), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "backups/large.bin")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: large body %d bytes in %d session requests, read back identical=%t\n", d.name, e.Size, d.chunks()-before, bytes.Equal(got, body))
	_, err = disk.Write(ctx, "backups/large.bin", io.MultiReader(bytes.NewReader(body)), filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "%s: IfMatch on a large body: %s\n", d.name, outcome(err))
	return nil
}

// failures opens the same drive with a token source that fails, one
// whose token the API refuses, and a transport that is rate limited.
// ds:def id=failures-bq6t3tw6
func failures(ctx context.Context, out io.Writer, d drive) error {
	revoked := func(context.Context) (string, error) { return "", errors.New("refresh token revoked") }
	wrong := func(context.Context) (string, error) { return "expired-token", nil }
	results := make([]string, 0, 2)
	for _, tok := range []tokenFunc{revoked, wrong} {
		disk, err := d.open(tok, nil)
		if err != nil {
			return err
		}
		_, err = disk.Stat(ctx, "notes/todo.txt")
		results = append(results, outcome(err))
	}
	fmt.Fprintf(out, "%s: failing token source: %s, refused token: %s\n", d.name, results[0], results[1])
	disk, err := d.open(validToken, &http.Client{Transport: throttled{}})
	if err != nil {
		return err
	}
	_, err = disk.Stat(ctx, "notes/todo.txt")
	wait, _ := filesystem.RetryAfterOf(err)
	fmt.Fprintf(out, "%s: rate limited: %s, retry after %s\n", d.name, outcome(err), wait)
	return nil
}

// throttled is a transport that answers every request with 429 Too Many
// Requests and a Retry-After, as each API does when an app exceeds its
// quota.
type throttled struct{}

// RoundTrip implements http.RoundTripper.
// ds:def id=throttled-roundtrip-jfxbpmmb
func (throttled) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Header:     http.Header{"Retry-After": {fmt.Sprint(rateLimitWait)}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    r,
	}, nil
}
