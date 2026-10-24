// Command concepts-capabilities shows how capabilities work: what the
// local and memory drivers declare, the portable fallbacks a Disk uses
// when a capability is missing, the CodeUnsupported error where no
// fallback exists, checking up front, and NewDisk refusing a driver that
// declares a capability it does not implement.
//
//	go run ./concepts-capabilities          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// linkLifetime is how long the signed link the demo asks for would live.
const linkLifetime = 5 * time.Minute

func main() {
	dir, err := os.MkdirTemp("", "concepts-capabilities-")
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
	fmt.Fprintf(out, "local:  %s\n", disk.Capabilities())
	fmt.Fprintf(out, "memory: %s\n", memory.New().Capabilities())
	if err := fallbacks(ctx, disk, out); err != nil {
		return err
	}
	if err := unsupported(ctx, disk, out); err != nil {
		return err
	}
	checkFirst(ctx, disk, out)
	refuseLiar(out)
	return nil
}

// fallbacks uses three operations the local driver has no capability for.
// Copy streams the object through the application, DeleteKeys deletes one
// key at a time, and both still behave exactly as on a backend that does
// them natively, visibility included.
// ds:def id=fallbacks-njuba3a9
func fallbacks(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("public file"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	e, err := disk.Copy(ctx, "a.txt", "b.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "streamed copy: %s %d bytes, %s\n", e.Key, e.Size, e.Visibility)
	if err := disk.DeleteKeys(ctx, []string{"a.txt", "b.txt"}); err != nil {
		return err
	}
	fmt.Fprintln(out, "deleted two keys one by one: ok")
	return nil
}

// unsupported asks for things local disk cannot store or sign. Each is
// refused with CodeUnsupported before anything is written, rather than
// the option being dropped and the call reporting success.
// ds:def id=unsupported-kra8uej8
func unsupported(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	_, err := disk.WriteBytes(ctx, "m.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"owner": "u1"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "h.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "m.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	fmt.Fprintf(out, "signed URL without a signer: %s\n", filesystem.CodeOf(err))
	_, err = disk.PublicURL(ctx, "m.txt")
	fmt.Fprintf(out, "public URL without a base: %s\n", filesystem.CodeOf(err))
	ok, err := disk.Exists(ctx, "m.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "m.txt stored anyway: %t\n", ok)
	return nil
}

// checkFirst branches on a capability before calling, which is how an
// application offers a feature only where the backend has it: metadata
// goes on the object when the disk stores it, and into the app's own
// database otherwise.
// ds:def id=checkfirst-u4pgcann
func checkFirst(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	opts := filesystem.WriteOptions{}
	if disk.Capabilities().Metadata {
		opts.Metadata = map[string]string{"owner": "u1"}
	}
	_, err := disk.WriteBytes(ctx, "report.txt", []byte("x"), opts)
	fmt.Fprintf(out, "write after checking Metadata: %v\n", err)
}

// liarDriver declares server-side copy but has no Copy method. It forwards
// the six Driver methods to a memory driver.
// ds:def id=liardriver-gzywh4kk
type liarDriver struct{ inner *memory.Driver }

func (l liarDriver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	return l.inner.Stat(ctx, key)
}

func (l liarDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	return l.inner.Read(ctx, key, rng)
}

func (l liarDriver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	return l.inner.Write(ctx, key, body, opts)
}

func (l liarDriver) Delete(ctx context.Context, key string) error { return l.inner.Delete(ctx, key) }

func (l liarDriver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	return l.inner.List(ctx, dir, opts)
}

// Capabilities claims Copy, which this type does not implement.
func (l liarDriver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{Copy: true}
}

// refuseLiar shows NewDisk catching the false claim at start-up, instead
// of the first Copy failing in production.
// ds:def id=refuseliar-wra6kbkq
func refuseLiar(out io.Writer) {
	_, err := filesystem.NewDisk("liar", liarDriver{inner: memory.New()})
	fmt.Fprintf(out, "NewDisk(liar): %v\n", err)
}
