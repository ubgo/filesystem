// Command concepts-visibility shows public and private files: private by
// default, per-object visibility where the backend has it (file modes on
// local disk), whole-disk visibility where it does not, and the
// two-bucket layout records uses to keep public files behind a CDN and
// private ones behind signed URLs.
//
//	go run ./concepts-visibility          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

func main() {
	dir, err := os.MkdirTemp("", "concepts-visibility-")
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
	if err := privateByDefault(ctx, out); err != nil {
		return err
	}
	if err := fileModes(ctx, out, dir); err != nil {
		return err
	}
	if err := wholeDisk(ctx, out); err != nil {
		return err
	}
	return twoDisks(ctx, out)
}

// privateByDefault writes without choosing a visibility, then publishes
// the object. Exposing a file by accident is worse than opting in.
// ds:def id=privatebydefault-xxynnbq9
func privateByDefault(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	e, err := disk.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "default: %s\n", e.Visibility)
	if err := disk.SetVisibility(ctx, "report.pdf", filesystem.VisibilityPublic); err != nil {
		return err
	}
	e, err = disk.Stat(ctx, "report.pdf")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after SetVisibility: %s\n", e.Visibility)
	return nil
}

// fileModes shows how the local driver stores visibility: a public file is
// world-readable (0644) so a web server can serve the directory, a private
// one is readable by the owner only (0600).
// ds:def id=filemodes-3vmcc4tw
func fileModes(ctx context.Context, out io.Writer, dir string) error {
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
	for _, v := range filesystem.VisibilityValues {
		key := string(v) + ".txt"
		if _, err := disk.WriteBytes(ctx, key, []byte(key), filesystem.WriteOptions{Visibility: v}); err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(dir, key))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "local %s: %v\n", v, info.Mode().Perm())
	}
	return nil
}

// bucketWide stands in for a bucket without per-object ACLs: an S3 bucket
// with ACLs disabled (the AWS default), R2, GCS with uniform access, or an
// Azure container. It keeps the memory driver's methods but reports no
// SetVisibility capability, so the Disk never calls it.
// ds:def id=bucketwide-2wfzq3qx
type bucketWide struct{ *memory.Driver }

// Capabilities hides SetVisibility, as those backends would.
func (b bucketWide) Capabilities() filesystem.Capabilities {
	c := b.Driver.Capabilities()
	c.SetVisibility = false
	return c
}

// wholeDisk shows visibility as a property of the whole disk: the disk
// default describes every object, and asking for another visibility on
// one object is CodeUnsupported instead of being silently ignored.
// ds:def id=wholedisk-p2thdqbt
func wholeDisk(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("cdn", bucketWide{memory.New()}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "logo.png", []byte("png"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "secret.pdf", []byte("pdf"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	fmt.Fprintf(out, "private write on a public bucket: %s\n", filesystem.CodeOf(err))
	err = disk.SetVisibility(ctx, "logo.png", filesystem.VisibilityPrivate)
	fmt.Fprintf(out, "SetVisibility on a public bucket: %s\n", filesystem.CodeOf(err))
	return nil
}
