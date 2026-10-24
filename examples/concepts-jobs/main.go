// Command concepts-jobs runs maintenance jobs by hand, the way a test or
// an admin button does: accept uploads fast and promote them to the slow
// store later, preview a sweep before letting it delete, wrap a function
// of your own as a job, and see a misconfigured job refuse to run.
//
//	go run ./concepts-jobs          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// Settings of the demo.
const (
	// pendingDir is where uploads wait on the fast disk.
	pendingDir = "pending"
	// incomingDir holds uploads nobody confirmed, for the sweep.
	incomingDir = "incoming"
	// minAge leaves objects younger than this alone: an upload still
	// being written is never moved or swept.
	minAge = time.Hour
	// pngFile sniffs as a PNG.
	pngFile = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR sample image"
)

// later is the "now" the demo runs its jobs at: past minAge, as a
// scheduler would be an hour after the uploads.
var later = time.Now().Add(2 * minAge)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{
		acceptFastStoreLater, previewThenSweep, customJob, refusedConfig,
	} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// mem returns an empty memory disk.
func mem(name string) (*filesystem.Disk, error) {
	return filesystem.NewDisk(name, memory.New())
}

// readEither reads a key from the durable store, falling back to the
// staging disk while the file has not been promoted yet. It returns the
// bytes and the name of the disk that had them.
// ds:def id=readeither-zfzw99jw
func readEither(ctx context.Context, store, staging *filesystem.Disk, key string) ([]byte, string, error) {
	b, err := store.ReadAll(ctx, key)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		b, err = staging.ReadAll(ctx, filesystem.JoinKey(pendingDir, key))
		return b, staging.Name(), err
	}
	return b, store.Name(), err
}

// acceptFastStoreLater stores an upload on a fast disk and answers at
// once; the Promote job later moves it to the durable store under the
// same key without the staging prefix.
// ds:def id=acceptfaststorelater-ezzbbkyu
func acceptFastStoreLater(ctx context.Context, out io.Writer) error {
	staging, err := mem("staging")
	if err != nil {
		return err
	}
	store, err := mem("store")
	if err != nil {
		return err
	}
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Key: upload.RandomKey(pendingDir)}
	res, err := upload.Receive(ctx, staging, strings.NewReader(pngFile), "photo.png", p)
	if err != nil {
		return err
	}
	key := strings.TrimPrefix(res.Key, pendingDir+"/")
	_, before, err := readEither(ctx, store, staging, key)
	if err != nil {
		return err
	}
	promote := jobs.Promote(staging, pendingDir, store, minAge)
	r, err := promote.Run(ctx, later)
	if err != nil {
		return err
	}
	_, after, err := readEither(ctx, store, staging, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "before promote: read from %s; %s moved %d; after: read from %s\n", before, promote.Name, r.Count, after)
	return nil
}

// previewThenSweep runs a sweep's dry run first, which names what the
// real run would delete and deletes nothing, then the real run.
// ds:def id=previewthensweep-n9pdx253
func previewThenSweep(ctx context.Context, out io.Writer) error {
	disk, err := mem("uploads")
	if err != nil {
		return err
	}
	for _, k := range []string{"incoming/a.png", "incoming/b.png", "files/kept.png"} {
		if _, err := disk.WriteBytes(ctx, k, []byte(pngFile), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	sweep := jobs.UploadSweep(disk, incomingDir, minAge)
	preview, err := sweep.DryRun(ctx, later)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dry run: would delete %d: %s\n", preview.Count, strings.Join(preview.Planned, " "))
	done, err := sweep.Run(ctx, later)
	if err != nil {
		return err
	}
	left, err := disk.Exists(ctx, "files/kept.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sweep: deleted %d, files/kept.png still there: %t\n", done.Count, left)
	return nil
}

// customJob wraps a function of your own as a job, so the scheduler and
// the other runners treat it like the built-in ones, and renames a
// built-in job to schedule two of one kind.
// ds:def id=customjob-eyspfne3
func customJob(ctx context.Context, out io.Writer) error {
	cache, err := mem("cache")
	if err != nil {
		return err
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := cache.WriteBytes(ctx, k, []byte("12345"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	trim := jobs.Func("trim-media-cache", func(ctx context.Context, _ time.Time) (jobs.Result, error) {
		n, err := filesystem.TrimCache(ctx, cache, 10)
		return jobs.Result{Count: int64(n)}, err
	})
	r, err := trim.Run(ctx, later)
	if err != nil {
		return err
	}
	exports := jobs.UploadSweep(cache, "exports", minAge).Named("sweep-exports")
	fmt.Fprintf(out, "%s: %d trimmed; a second sweep named %s\n", trim.Name, r.Count, exports.Name)
	return nil
}

// refusedConfig shows that a zero minimum age is refused rather than read
// as "sweep everything now", and that a scheduler validates every
// schedule before it starts any of them.
// ds:def id=refusedconfig-wbkq5hk3
func refusedConfig(ctx context.Context, out io.Writer) error {
	disk, err := mem("uploads")
	if err != nil {
		return err
	}
	_, err = jobs.UploadSweep(disk, incomingDir, 0).Run(ctx, later)
	fmt.Fprintf(out, "sweep with minAge 0: %s\n", filesystem.CodeOf(err))
	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	sweep := jobs.UploadSweep(disk, incomingDir, minAge)
	err = s.Run(ctx, jobs.Schedule{Job: sweep, Every: time.Hour}, jobs.Schedule{Job: sweep, Every: time.Hour})
	fmt.Fprintf(out, "two schedules with one name: %s\n", filesystem.CodeOf(err))
	return nil
}
