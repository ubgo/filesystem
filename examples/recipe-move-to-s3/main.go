// Command recipe-move-to-s3 copies an existing local file store to an S3
// bucket: it walks the local disk, copies every object across with a
// bounded pool of workers, skips what an earlier run already copied, and
// verifies the result. Run it as often as needed; each run copies only what
// is missing or different, so a crashed or interrupted run is resumed by
// running it again.
//
//	go run ./recipe-move-to-s3                 demo: a seeded temp dir to an in-process fake S3
//	go run ./recipe-move-to-s3 -dry-run        report what would be copied
//	go run ./recipe-move-to-s3 -fake-s3 localhost:8116
//	                                           serve an empty fake S3 (bucket "media") to try fs cp against
//
// With MOVE_SOURCE and MOVE_TARGET set, it moves real disks:
//
//	MOVE_SOURCE='file:///var/app/uploads' \
//	MOVE_TARGET='s3://media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com' \
//	go run ./recipe-move-to-s3 -workers 16
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ubgo/filesystem"
	fss3 "github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/examples/internal/serve"
	"github.com/ubgo/filesystem/local"
)

// Decisions of this tool, not of the library.
const (
	// envSource and envTarget name the variables holding the two DSNs.
	envSource = "MOVE_SOURCE"
	envTarget = "MOVE_TARGET"
	// diskName is the name both disks carry. Keeping the application's disk
	// name when its backend changes means no database row has to change.
	diskName = "media"
	// demoBucket is the bucket the in-process fake S3 creates.
	demoBucket = "media"
	// defaultWorkers is how many objects are copied at once. Copies are
	// network-bound, so more workers than CPUs is normal; the backend's
	// request rate is the real limit.
	defaultWorkers = 8
	// readHeaderTimeout bounds a slow client's headers in -fake-s3 mode.
	readHeaderTimeout = 10 * time.Second
)

// Options control one run.
// ds:def id=options-rgyqvp8b
type Options struct {
	// Workers is the number of concurrent copies; values below 1 mean 1.
	Workers int
	// DryRun lists what would be copied and copies nothing.
	DryRun bool
}

// Report is what a run did. Failed maps a key to its error; a run with
// failures is safe to repeat.
// ds:def id=report-4jzszwze
type Report struct {
	Copied  int64
	Skipped int64
	Bytes   int64
	Failed  map[string]error
}

// needsCopy reports whether src's object e is missing on dst or differs in
// size. Size is what every backend reports on Stat; a same-size object with
// different bytes is caught by the verification pass, not here.
// ds:def id=needscopy-9f8fz5c9
func needsCopy(ctx context.Context, dst *filesystem.Disk, e filesystem.Entry) (bool, error) {
	got, err := dst.Stat(ctx, e.Key)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return true, nil
	case err != nil:
		return false, err
	}
	return got.Size != e.Size, nil
}

// copyOne copies one object if it needs copying and updates the report.
// CopyAcross streams the bytes and keeps the content type (detected from
// the bytes when the source does not store one), and metadata and
// visibility where the target stores them.
// ds:def id=copyone-75r2dq2z
func copyOne(ctx context.Context, src, dst *filesystem.Disk, e filesystem.Entry, o Options, r *Report) error {
	need, err := needsCopy(ctx, dst, e)
	if err != nil || !need {
		if err == nil {
			atomic.AddInt64(&r.Skipped, 1)
		}
		return err
	}
	if o.DryRun {
		fmt.Printf("would copy %s (%d bytes)\n", e.Key, e.Size)
		atomic.AddInt64(&r.Copied, 1)
		return nil
	}
	if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
		return err
	}
	atomic.AddInt64(&r.Copied, 1)
	atomic.AddInt64(&r.Bytes, e.Size)
	return nil
}

// Copy walks src recursively and copies every object to the same key on
// dst with o.Workers workers. A failed object is recorded and the others
// continue; a failed listing stops the run, since the rest is unknown.
// ds:def id=copy-fx35x5fx
func Copy(ctx context.Context, src, dst *filesystem.Disk, o Options) (Report, error) {
	r := Report{Failed: map[string]error{}}
	var mu sync.Mutex
	jobs := make(chan filesystem.Entry)
	var wg sync.WaitGroup
	for range max(o.Workers, 1) {
		wg.Go(func() {
			for e := range jobs {
				if err := copyOne(ctx, src, dst, e, o, &r); err != nil {
					mu.Lock()
					r.Failed[e.Key] = err
					mu.Unlock()
				}
			}
		})
	}
	var listErr error
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			listErr = err
			break
		}
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	return r, listErr
}

// digest returns the SHA-256 of an object, read in full.
// ds:def id=digest-kqeqrzyz
func digest(ctx context.Context, d *filesystem.Disk, key string) (string, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify compares every object of src with dst by size and SHA-256 and
// returns the keys that differ or are missing. It reads every byte on both
// sides, so run it once before the switch, not on every copy.
// ds:def id=verify-u9yjwc9b
func Verify(ctx context.Context, src, dst *filesystem.Disk) ([]string, error) {
	var bad []string
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return bad, err
		}
		want, err := digest(ctx, src, e.Key)
		if err != nil {
			return bad, err
		}
		got, err := digest(ctx, dst, e.Key)
		if err != nil || got != want {
			bad = append(bad, e.Key)
		}
	}
	return bad, nil
}

// fakeS3 starts an in-process S3 server with one bucket and returns a
// client for it and a function that stops it. It stands in for S3 or R2 so
// the demo runs offline; it accepts any credentials.
func fakeS3(ctx context.Context) (*s3.Client, func(), error) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("demo", "demo", ""),
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(demoBucket)}); err != nil {
		srv.Close()
		return nil, nil, err
	}
	return client, srv.Close, nil
}

// fakeTarget opens the fake bucket as the target disk.
func fakeTarget(ctx context.Context) (*filesystem.Disk, func(), error) {
	client, stop, err := fakeS3(ctx)
	if err != nil {
		return nil, nil, err
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: demoBucket})
	if err != nil {
		stop()
		return nil, nil, err
	}
	d, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return d, stop, nil
}

// demoFiles is what the demo seeds the source with: a few objects in
// nested directories, as an uploads directory looks after a few years.
var demoFiles = map[string]string{
	"avatars/u1.png":          "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-u1",
	"avatars/u2.png":          "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-u2",
	"invoices/2025/0001.pdf":  "%PDF-1.7\n% invoice 0001",
	"invoices/2025/0002.pdf":  "%PDF-1.7\n% invoice 0002",
	"exports/u1/2025-12.csv":  "id,amount\n1,10\n",
	"attachments/a/notes.txt": "plain text notes",
}

// seedSource creates a local disk in dir holding demoFiles.
func seedSource(ctx context.Context, dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir)
	if err != nil {
		return nil, err
	}
	d, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, err
	}
	for key, body := range demoFiles {
		if _, err := d.WriteBytes(ctx, key, []byte(body), filesystem.WriteOptions{}); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// openFromEnv opens both disks from MOVE_SOURCE and MOVE_TARGET. ok is
// false when neither is set (demo mode).
// ds:def id=openfromenv-ucj3efgh
func openFromEnv(ctx context.Context) (src, dst *filesystem.Disk, ok bool, err error) {
	srcDSN, dstDSN := os.Getenv(envSource), os.Getenv(envTarget)
	if srcDSN == "" && dstDSN == "" {
		return nil, nil, false, nil
	}
	if srcDSN == "" || dstDSN == "" {
		return nil, nil, true, fmt.Errorf("set both %s and %s", envSource, envTarget)
	}
	mux := filesystem.NewMux()
	if err := errors.Join(mux.Register(local.Scheme, local.Opener), mux.Register(fss3.Scheme, fss3.Opener)); err != nil {
		return nil, nil, true, err
	}
	if src, err = mux.Open(ctx, diskName, srcDSN, filesystem.WithInterceptors(filesystem.ReadOnly())); err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", envSource, err)
	}
	if dst, err = mux.Open(ctx, diskName, dstDSN); err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", envTarget, err)
	}
	return src, dst, true, nil
}

// run copies, prints the report, and verifies when the copy succeeded.
// ds:def id=run-eqa8vn2g
func run(ctx context.Context, src, dst *filesystem.Disk, o Options) error {
	start := time.Now()
	r, err := Copy(ctx, src, dst, o)
	verb := "copied"
	if o.DryRun {
		verb = "to copy"
	}
	fmt.Printf("%s %d, skipped %d, failed %d, %d bytes in %s\n", verb, r.Copied, r.Skipped, len(r.Failed), r.Bytes, time.Since(start).Round(time.Millisecond))
	for key, ferr := range r.Failed {
		fmt.Printf("  failed %s: %v\n", key, ferr)
	}
	if err != nil || len(r.Failed) > 0 || o.DryRun {
		return errors.Join(err, failedErr(len(r.Failed)))
	}
	bad, err := Verify(ctx, src, dst)
	if err != nil {
		return err
	}
	fmt.Printf("verified: %d objects differ\n", len(bad))
	if len(bad) > 0 {
		return fmt.Errorf("objects differ: %v", bad)
	}
	return nil
}

// failedErr is an error when n objects failed, else nil.
func failedErr(n int) error {
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d objects failed; run again to retry them", n)
}

// demo seeds a temp dir, copies it to the fake bucket twice (the second run
// skips everything), and shows what a dry run reports after a change. The
// two copies always run for real, whatever -dry-run says: the demo's point
// is the resume, and its last step is the dry run.
func demo(ctx context.Context, o Options) error {
	dir, err := os.MkdirTemp("", "recipe-move-to-s3-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src, err := seedSource(ctx, filepath.Join(dir, "uploads"))
	if err != nil {
		return err
	}
	dst, stop, err := fakeTarget(ctx)
	if err != nil {
		return err
	}
	defer stop()
	copying := Options{Workers: o.Workers}
	fmt.Println("first run:")
	if err := run(ctx, src, dst, copying); err != nil {
		return err
	}
	if e, err := dst.Stat(ctx, "invoices/2025/0001.pdf"); err == nil {
		fmt.Printf("on the bucket: %s %s %d bytes\n", e.Key, e.ContentType, e.Size)
	}
	fmt.Println("second run (resumes, nothing to do):")
	if err := run(ctx, src, dst, copying); err != nil {
		return err
	}
	if _, err := src.WriteBytes(ctx, "avatars/u3.png", []byte(demoFiles["avatars/u1.png"]+"-u3"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Println("after a new upload, dry run:")
	return run(ctx, src, dst, Options{Workers: o.Workers, DryRun: true})
}

// serveFakeS3 serves an empty fake S3 with bucket demoBucket on addr until
// ctx is cancelled, for trying fs cp and this tool by DSN.
// ds:def id=servefakes3-7btu4b8k
func serveFakeS3(ctx context.Context, addr string) error {
	backend := s3mem.New()
	if err := backend.CreateBucket(demoBucket); err != nil {
		return err
	}
	fmt.Printf("fake S3 on http://%s, bucket %q (any credentials)\n", addr, demoBucket)
	srv := &http.Server{Addr: addr, Handler: gofakes3.New(backend).Server(), ReadHeaderTimeout: readHeaderTimeout}
	return serve.UntilDone(ctx, srv)
}

// start parses the flags and picks the mode.
func start(ctx context.Context) error {
	o := Options{}
	fakeAddr := flag.String("fake-s3", "", "serve an empty fake S3 on this address and do nothing else")
	flag.IntVar(&o.Workers, "workers", defaultWorkers, "concurrent copies")
	flag.BoolVar(&o.DryRun, "dry-run", false, "report what would be copied, copy nothing")
	flag.Parse()
	if *fakeAddr != "" {
		return serveFakeS3(ctx, *fakeAddr)
	}
	src, dst, ok, err := openFromEnv(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return demo(ctx, o)
	}
	return run(ctx, src, dst, o)
}

func main() {
	// Ctrl+C or SIGTERM stops the fake S3 server cleanly.
	ctx, stop := serve.Context()
	defer stop()
	if err := start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
