// Command adapters-memory walks through the in-memory driver: opening it
// from a DSN and with New, a fixed clock for exact timestamps, the
// capabilities it has, a clock that makes uploads look abandoned, version
// history, and the one thing every memory disk lacks: sharing. Each DSN
// opens a fresh, empty store that disappears with the process.
//
//	go run ./adapters-memory            (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// cacheDSN opens a memory disk; the host part ("cache") is only a label.
const cacheDSN = "mem://cache"

// frozenTime is the clock the test disk reports for every write.
var frozenTime = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process: every step prints what it saw to out.
func run(ctx context.Context, out io.Writer) error {
	disk, err := openWithClock()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	if err := storeEverything(ctx, out, disk); err != nil {
		return err
	}
	if err := separateStores(ctx, out); err != nil {
		return err
	}
	if err := sweepWithClock(ctx, out); err != nil {
		return err
	}
	return versionHistory(ctx, out)
}

// openWithClock builds a memory disk whose modification times come from a
// fixed clock, so a test can assert them exactly.
// ds:def id=openwithclock-d37tffgx
func openWithClock() (*filesystem.Disk, error) {
	return filesystem.NewDisk("cache", memory.New(memory.WithClock(func() time.Time { return frozenTime })))
}

// storeEverything writes with every option a backend can store: visibility,
// metadata, headers, a conditional write, and reads them back.
// ds:def id=storeeverything-cywhsa7y
func storeEverything(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "thumbs/1.json", []byte(`{"w":64}`), filesystem.WriteOptions{
		Visibility:  filesystem.VisibilityPublic,
		Metadata:    map[string]string{"source": "1.png"},
		Headers:     filesystem.HTTPHeaders{CacheControl: "public, max-age=60"},
		IfNotExists: true,
	})
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "thumbs/1.json")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %s %s source=%s cache-control=%q at %s\n",
		e.Key, e.ContentType, e.Visibility, e.Metadata["source"], e.Headers.CacheControl, e.ModTime.Format(time.RFC3339))
	_, err = disk.WriteBytes(ctx, "thumbs/1.json", []byte("{}"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "thumbs/1.json", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	fmt.Fprintf(out, "signed URL without a signer: %s\n", filesystem.CodeOf(err))
	return nil
}

// separateStores opens the same DSN twice: each Open is a new, empty
// store, so a file written through one disk is not seen by the other.
// ds:def id=separatestores-s9sudcqc
func separateStores(ctx context.Context, out io.Writer) error {
	mux := filesystem.NewMux()
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		return err
	}
	a, err := mux.Open(ctx, "a", cacheDSN)
	if err != nil {
		return err
	}
	b, err := mux.Open(ctx, "b", cacheDSN)
	if err != nil {
		return err
	}
	if _, err := a.WriteBytes(ctx, "k", []byte("v"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	inA, err := a.Exists(ctx, "k")
	if err != nil {
		return err
	}
	inB, err := b.Exists(ctx, "k")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "same DSN opened twice: in a=%t, in b=%t\n", inA, inB)
	return nil
}

// abandonAfter is how long an unfinished multipart upload may sit idle
// before the sweep in sweepWithClock drops it.
const abandonAfter = 24 * time.Hour

// sweepWithClock starts a multipart upload, moves the driver's clock two
// days forward, and previews then runs the sweep. The clock is the only
// time the memory driver knows, so a test needs no sleep to make an upload
// look old.
// ds:def id=sweepwithclock-8b8vvdut
func sweepWithClock(ctx context.Context, out io.Writer) error {
	now := frozenTime
	disk, err := filesystem.NewDisk("uploads", memory.New(memory.WithClock(func() time.Time { return now })))
	if err != nil {
		return err
	}
	id, err := disk.CreateMultipart(ctx, "videos/big.mp4", filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	now = now.Add(2 * abandonAfter)
	cutoff := now.Add(-abandonAfter)
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "would sweep: %s upload for %s\n", a.Kind, a.Key)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	_, err = disk.ListParts(ctx, "videos/big.mp4", id)
	fmt.Fprintf(out, "swept %d; the upload is now %s\n", n, filesystem.CodeOf(err))
	return nil
}

// versionHistory keeps every version of a key, like a versioned bucket,
// and reads the first one back after it was overwritten.
// ds:def id=versionhistory-sgnt5kvr
func versionHistory(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("history", memory.New(memory.WithVersions()))
	if err != nil {
		return err
	}
	for _, body := range []string{"draft", "final"} {
		if _, err := disk.WriteBytes(ctx, "notes.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	vs, err := disk.Versions(ctx, "notes.txt") // newest first
	if err != nil {
		return err
	}
	rc, err := disk.ReadVersion(ctx, "notes.txt", vs[len(vs)-1].ID)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var first strings.Builder
	if _, err := io.Copy(&first, rc); err != nil {
		return err
	}
	fmt.Fprintf(out, "versions: %d, oldest holds %q\n", len(vs), first.String())
	return nil
}
