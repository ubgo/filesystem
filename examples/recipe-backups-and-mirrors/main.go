// Command recipe-backups-and-mirrors moves a live store from a local
// directory to S3 without downtime, and keeps a dated backup copy:
//
//  1. Mirror: the app writes through a disk that writes the local primary
//     and then S3, so every new or changed object reaches both.
//
//  2. Backfill: objects stored before the mirror existed are copied across.
//
//  3. Verify: every object is compared by size and SHA-256, both ways.
//
//  4. Switch: S3 becomes the primary, the old directory the mirror, so a
//     rollback is a configuration change.
//
//  5. Backup: objects changed since the last run are copied to a backup disk.
//
//     go run ./recipe-backups-and-mirrors
//
// The S3 side is an in-process fake unless MIRROR_DSN names a real bucket,
// e.g. MIRROR_DSN='s3://my-bucket?region=eu-west-1' (credentials from the
// AWS default chain).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// Decisions of this app, not of the library.
const (
	// appDisk is the name the application uses, whatever is behind it. The
	// mirror takes this name so nothing else in the app changes.
	appDisk = "media"
	// oldDisk and newDisk name the two backends during the move.
	oldDisk = "media-local"
	newDisk = "media-s3"
	// backupDisk names the backup target.
	backupDisk = "backups"
	// envMirrorDSN names a real S3 bucket to move to.
	envMirrorDSN = "MIRROR_DSN"
	// fakeBucket is the bucket of the in-process fake.
	fakeBucket = "media"
	// fakeRegion is any region; the fake ignores it.
	fakeRegion = "us-east-1"
	// existingObjects is how many objects the demo's old store holds.
	existingObjects = 5
	// signedLifetime is the lifetime of the signed URLs the demo asks for.
	signedLifetime = time.Minute
)

// fakeS3 starts an in-process S3 server with one bucket. The returned
// function stops it.
// ds:def id=fakes3-6jrsmx9s
func fakeS3(ctx context.Context) (*s3.Driver, func(), error) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Region:       fakeRegion,
		Credentials:  credentials.NewStaticCredentialsProvider("fake", "fake", ""),
	})
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(fakeBucket)}); err != nil {
		srv.Close()
		return nil, nil, err
	}
	drv, err := s3.New(s3.Config{Client: client, Bucket: fakeBucket})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return drv, srv.Close, nil
}

// openTarget opens the S3 side: the bucket MIRROR_DSN names, or the fake.
// ds:def id=opentarget-cs6qemev
func openTarget(ctx context.Context) (*filesystem.Disk, func(), error) {
	if dsn, ok := os.LookupEnv(envMirrorDSN); ok {
		mux := filesystem.NewMux()
		if err := mux.Register(s3.Scheme, s3.Opener); err != nil {
			return nil, nil, err
		}
		d, err := mux.Open(ctx, newDisk, dsn)
		return d, func() {}, err
	}
	drv, stop, err := fakeS3(ctx)
	if err != nil {
		return nil, nil, err
	}
	d, err := filesystem.NewDisk(newDisk, drv)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return d, stop, nil
}

// seed writes the objects an app stored before the move began.
func seed(ctx context.Context, d *filesystem.Disk) error {
	for i := range existingObjects {
		body := fmt.Appendf(nil, "invoice %d\n", i+1)
		if _, err := d.WriteBytes(ctx, fmt.Sprintf("invoices/2026/%03d.txt", i+1), body, filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// backfill copies every object of src that dst lacks, or holds with
// another size, and reports how many it copied and skipped. Rerunning it
// after a failure skips what is already there, so it resumes. Size is a
// cheap first check; verify compares content.
// ds:def id=backfill-4upds8sy
func backfill(ctx context.Context, src, dst *filesystem.Disk) (copied, skipped int, err error) {
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return copied, skipped, err
		}
		have, err := dst.Stat(ctx, e.Key)
		if err == nil && have.Size == e.Size {
			skipped++
			continue
		}
		if err != nil && filesystem.CodeOf(err) != filesystem.CodeNotFound {
			return copied, skipped, err
		}
		if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
			return copied, skipped, err
		}
		copied++
	}
	return copied, skipped, nil
}

// digest streams an object and returns its SHA-256 and size. Stat does not
// report a SHA-256 on the built-in drivers, so verification reads the bytes.
// ds:def id=digest-wfjmcum7
func digest(ctx context.Context, d *filesystem.Disk, key string) (string, int64, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Report is the outcome of a verification.
type Report struct {
	Checked   int
	Different []string // on both, with other content
	Missing   []string // on src only
	Extra     []string // on dst only: deleted on src while a backfill ran
}

// OK reports whether dst matches src exactly.
func (r Report) OK() bool { return len(r.Different)+len(r.Missing)+len(r.Extra) == 0 }

// verify compares every object of src with dst by size and SHA-256, then
// lists what dst has that src does not.
// ds:def id=verify-4ca52aq5
func verify(ctx context.Context, src, dst *filesystem.Disk) (Report, error) {
	var r Report
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return r, err
		}
		r.Checked++
		want, wantSize, err := digest(ctx, src, e.Key)
		if err != nil {
			return r, err
		}
		got, gotSize, err := digest(ctx, dst, e.Key)
		switch {
		case filesystem.CodeOf(err) == filesystem.CodeNotFound:
			r.Missing = append(r.Missing, e.Key)
		case err != nil:
			return r, err
		case got != want || gotSize != wantSize:
			r.Different = append(r.Different, e.Key)
		}
	}
	for e, err := range dst.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return r, err
		}
		if ok, err := src.Exists(ctx, e.Key); err != nil {
			return r, err
		} else if !ok {
			r.Extra = append(r.Extra, e.Key)
		}
	}
	return r, nil
}

// backupSince copies to dst every object of src modified after since, and
// returns how many it copied. Run it on a schedule, passing the start time
// of the previous successful run, so each run copies only the changes.
// Deletions are not propagated: a backup keeps what the app deleted until
// the backup's own retention removes it.
// ds:def id=backupsince-5maq5zbp
func backupSince(ctx context.Context, src, dst *filesystem.Disk, since time.Time) (int, error) {
	n := 0
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return n, err
		}
		if !e.ModTime.After(since) {
			continue
		}
		if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Disks are the backends of the demo.
type Disks struct {
	Old, New, Backup *filesystem.Disk
}

// openDisks opens the old local store, the S3 target and a backup disk
// (memory here; another bucket or region in production).
func openDisks(ctx context.Context, dataDir string) (Disks, func(), error) {
	drv, err := local.New(filepath.Join(dataDir, "media"))
	if err != nil {
		return Disks{}, nil, err
	}
	old, err := filesystem.NewDisk(oldDisk, drv)
	if err != nil {
		return Disks{}, nil, err
	}
	target, stop, err := openTarget(ctx)
	if err != nil {
		return Disks{}, nil, err
	}
	backup, err := filesystem.NewDisk(backupDisk, memory.New())
	if err != nil {
		stop()
		return Disks{}, nil, err
	}
	return Disks{Old: old, New: target, Backup: backup}, stop, nil
}

// startMirror is phase 1: the app's disk becomes a mirror that writes the
// old store first and S3 second, under the name the app already uses.
// Reads, listings and URLs still come from the old store.
// ds:def id=startmirror-f4by8yz3
func startMirror(ctx context.Context, w io.Writer, d Disks) (*filesystem.Disk, error) {
	app, err := filesystem.Mirror(appDisk, d.Old, d.New)
	if err != nil {
		return nil, err
	}
	if _, err := app.WriteBytes(ctx, "invoices/2026/new.txt", []byte("written during the move\n"), filesystem.WriteOptions{}); err != nil {
		return nil, err
	}
	onNew, err := d.New.Exists(ctx, "invoices/2026/new.txt")
	fmt.Fprintf(w, "mirror: a new write reached %s=true %s=%v\n", oldDisk, newDisk, onNew)
	return app, err
}

// copyExisting is phases 2 and 3: backfill, then verify both ways.
// ds:def id=copyexisting-h3m6e8hd
func copyExisting(ctx context.Context, w io.Writer, d Disks) error {
	copied, skipped, err := backfill(ctx, d.Old, d.New)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "backfill: copied %d, skipped %d already there\n", copied, skipped)
	copied, skipped, err = backfill(ctx, d.Old, d.New)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "backfill again: copied %d, skipped %d\n", copied, skipped)
	r, err := verify(ctx, d.Old, d.New)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "verify: %d checked, %d different, %d missing, %d extra, ok=%v\n", r.Checked, len(r.Different), len(r.Missing), len(r.Extra), r.OK())
	if !r.OK() {
		return filesystem.Errorf(filesystem.CodeUnavailable, "the copy does not match: %+v", r)
	}
	return nil
}

// switchPrimary is phase 4: S3 becomes the primary and the old store the
// mirror, still written, so going back is one more swap. Once you trust
// S3, open it alone under the app's name and retire the old store.
// ds:def id=switchprimary-cb2vhdjw
func switchPrimary(ctx context.Context, w io.Writer, d Disks) (*filesystem.Disk, error) {
	app, err := filesystem.Mirror(appDisk, d.New, d.Old)
	if err != nil {
		return nil, err
	}
	if _, err := app.WriteBytes(ctx, "invoices/2026/after-switch.txt", []byte("written after the switch\n"), filesystem.WriteOptions{}); err != nil {
		return nil, err
	}
	onOld, err := d.Old.Exists(ctx, "invoices/2026/after-switch.txt")
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "switch: primary %s, a new write also reached %s=%v\n", newDisk, oldDisk, onOld)
	_, err = app.SignedURL(ctx, "invoices/2026/upload.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: signedLifetime})
	fmt.Fprintf(w, "switch: signed PUT on the mirror: %s\n", filesystem.CodeOf(err))
	get, err := app.SignedURL(ctx, "invoices/2026/001.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: signedLifetime})
	if err == nil {
		fmt.Fprintf(w, "switch: signed GET is signed by %s: %v\n", newDisk, get.URL != "")
	}
	return app, err
}

// backup is phase 5: a first full run, then an incremental one that copies
// only what changed since the first started.
// ds:def id=backup-vv56hxqn
func backup(ctx context.Context, w io.Writer, app *filesystem.Disk, d Disks) error {
	first := time.Time{}
	n, err := backupSince(ctx, app, d.Backup, first)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "backup: full run copied %d\n", n)
	second := time.Now()
	time.Sleep(time.Second) // S3 reports modification times in whole seconds
	if _, err := app.WriteBytes(ctx, "invoices/2026/001.txt", []byte("invoice 1, corrected\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if n, err = backupSince(ctx, app, d.Backup, second); err != nil {
		return err
	}
	fmt.Fprintf(w, "backup: incremental run copied %d\n", n)
	return nil
}

// run is the whole move, phase by phase.
// ds:def id=run-8cmqhc4s
func run(ctx context.Context, w io.Writer, dataDir string) error {
	d, stop, err := openDisks(ctx, dataDir)
	if err != nil {
		return err
	}
	defer stop()
	if err := seed(ctx, d.Old); err != nil {
		return err
	}
	if _, err := startMirror(ctx, w, d); err != nil {
		return err
	}
	if err := copyExisting(ctx, w, d); err != nil {
		return err
	}
	app, err := switchPrimary(ctx, w, d)
	if err != nil {
		return err
	}
	return backup(ctx, w, app, d)
}

func main() {
	dir, err := os.MkdirTemp("", "recipe-backups-and-mirrors-")
	if err == nil {
		err = run(context.Background(), os.Stdout, dir)
		_ = os.RemoveAll(dir) // best effort: it is a temporary directory
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
