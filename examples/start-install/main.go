// Command start-install is the program behind docs/start/install.md: a
// first Disk on a local directory, then the same code on any backend by
// changing a DSN.
//
//	go run ./start-install                                  (a local directory)
//	STORAGE_DSN=mem:// go run ./start-install               (memory)
//	STORAGE_DSN='s3://bucket?region=auto&endpoint=…' go run ./start-install
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/s3"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

const (
	// envStorageDSN names the variable holding the disk's DSN.
	envStorageDSN = "STORAGE_DSN"
	// defaultDSN is a directory next to where the program runs.
	defaultDSN = "file:storage"
	// diskName names the disk in errors, logs and signed URLs.
	diskName = "media"
	// helloKey is the object the program writes and reads back.
	helloKey = "hello/world.txt"
	// helloText is what it writes.
	helloText = "hello, storage"
)

func main() {
	dsn := os.Getenv(envStorageDSN)
	if dsn == "" {
		dsn = defaultDSN
	}
	if err := run(context.Background(), dsn, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// firstDisk builds a Disk over a local directory with no DSN at all: the
// driver, then the Disk around it. This is all the core module needs.
// ds:def id=install-firstdisk-knpegaay
func firstDisk(dir string) (*filesystem.Disk, error) {
	drv, err := local.New(dir)
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(diskName, drv)
}

// newMux knows the backends this program can open from a DSN. Nothing is
// registered by importing a package: an app lists the drivers it uses.
// ds:def id=install-newmux-rs6juqjr
func newMux() (*filesystem.Mux, error) {
	mux := filesystem.NewMux()
	return mux, errors.Join(
		mux.Register(local.Scheme, local.Opener),
		mux.Register(memory.Scheme, memory.Opener),
		mux.Register(s3.Scheme, s3.Opener),
	)
}

// run opens the disk named by dsn, proves it works, then writes a file and
// reads it back. The code is the same for every backend.
// ds:def id=install-run-zd8h2968
func run(ctx context.Context, dsn string, out io.Writer) error {
	mux, err := newMux()
	if err != nil {
		return err
	}
	disk, err := mux.Open(ctx, diskName, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	if err := disk.Check(ctx); err != nil {
		return err
	}
	e, err := disk.Write(ctx, helloKey, strings.NewReader(helloText), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, helloKey)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "wrote %s: %d bytes, %s\nread back %q\ncapabilities: %s\n", e.Key, e.Size, e.ContentType, got, disk.Capabilities())
	return err
}
