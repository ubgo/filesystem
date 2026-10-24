// Command concepts-errors shows how storage errors are classified and
// handled: branching on the Code, the Err* sentinels with errors.Is, the
// fields of *filesystem.Error, reaching the backend's native error, the
// per-key failures of a batch, and the HTTP status each code maps to.
//
//	go run ./concepts-errors          (from the examples directory)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// missingKey is a key no step ever writes.
const missingKey = "avatars/nobody.png"

func main() {
	dir, err := os.MkdirTemp("", "concepts-errors-")
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

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer, dir string) error {
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "avatar: %s\n", avatarOrDefault(ctx, disk, missingKey))
	_, err = disk.Stat(ctx, missingKey)
	fmt.Fprintf(out, "errors.Is ErrNotFound: %t\n", errors.Is(err, filesystem.ErrNotFound))
	wrapped := fmt.Errorf("load profile: %w", err)
	fmt.Fprintf(out, "wrapped, still %s\n", filesystem.CodeOf(wrapped))
	describe(out, wrapped)
	if err := nativeCause(ctx, out, dir); err != nil {
		return err
	}
	batch(ctx, disk, out)
	statuses(out)
	return envelope(ctx, out)
}

// avatarOrDefault branches on the Code, never on a vendor error type, so
// the same code works on local disk, S3 or any other backend. A missing
// object is an expected answer; anything else is a real failure.
// ds:def id=avatarordefault-e4fy6urs
func avatarOrDefault(ctx context.Context, disk *filesystem.Disk, key string) string {
	_, err := disk.Stat(ctx, key)
	switch filesystem.CodeOf(err) {
	case "":
		return key
	case filesystem.CodeNotFound:
		return "default.png"
	case filesystem.CodeUnavailable:
		return "retry later"
	default:
		return "error: " + err.Error()
	}
}

// describe reads the fields the Disk stamps on every error: the
// operation, the disk and the key, with the backend's cause underneath.
// ds:def id=describe-meqmwbzm
func describe(out io.Writer, err error) {
	var fe *filesystem.Error
	if !errors.As(err, &fe) {
		return
	}
	fmt.Fprintf(out, "code=%s op=%s disk=%s key=%s\n", fe.Code, fe.Op, fe.Disk, fe.Key)
	fmt.Fprintf(out, "message: %v\n", err)
}

// nativeCause reaches the backend's own error through errors.As: the local
// driver's *fs.PathError is kept as the cause of the classified error.
// This is the escape hatch for vendor-specific handling; portable code
// stops at the Code.
// ds:def id=nativecause-z956hs3j
func nativeCause(ctx context.Context, out io.Writer, dir string) error {
	drv, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		_ = drv.Close()
		return err
	}
	defer func() { _ = disk.Close() }()
	_, err = disk.Stat(ctx, missingKey)
	var pe *fs.PathError
	fmt.Fprintf(out, "local: %s, native *fs.PathError: %t\n", filesystem.CodeOf(err), errors.As(err, &pe))
	return nil
}

// batch shows DeleteKeys reporting per-key failures: an invalid key fails
// on its own while every other key is deleted, and errors.Is asks whether
// any key failed with a given code.
// ds:def id=batch-qkwf94nk
func batch(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	for _, k := range []string{"tmp/a", "tmp/b"} {
		_, _ = disk.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{})
	}
	err := disk.DeleteKeys(ctx, []string{"tmp/a", "../escape", "tmp/b"})
	var be *filesystem.BatchError
	if errors.As(err, &be) {
		for _, f := range be.Failures {
			fmt.Fprintf(out, "batch failure: %q %s\n", f.Key, filesystem.CodeOf(f.Err))
		}
	}
	fmt.Fprintf(out, "batch errors.Is ErrInvalidKey: %t\n", errors.Is(err, filesystem.ErrInvalidKey))
	left, _ := disk.Exists(ctx, "tmp/a")
	fmt.Fprintf(out, "tmp/a still there: %t\n", left)
}

// statuses prints the HTTP status fshttp.StatusOf gives every code, the
// mapping the library's own handlers answer with.
// ds:def id=statuses-hn9je66t
func statuses(out io.Writer) {
	for _, c := range filesystem.CodeValues {
		s := fshttp.StatusOf(filesystem.Errorf(c, "example"))
		fmt.Fprintf(out, "%s -> %d %s\n", c, s, http.StatusText(s))
	}
}
