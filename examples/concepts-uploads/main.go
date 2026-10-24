// Command concepts-uploads walks every way bytes arrive through the
// upload package, all under one Policy: Receive for a stream, the
// multipart form Handler (all or nothing, and Partial), presigned direct
// uploads checked by Confirm, the Uppy endpoints, imports with FromURL,
// Process and Inspect hooks, Sweep for uploads nobody finished, key
// layouts, base64 bodies, size limits, per-request limits, declared
// checksums, cache then promote, and abandoned multipart uploads.
//
//	go run ./concepts-uploads          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// Sample files whose first bytes sniff recognises. Real uploads are
// larger; detection reads only the first 512 bytes.
const (
	pngFile  = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR sample image"
	pdfFile  = "%PDF-1.7 sample document"
	htmlFile = "<!DOCTYPE html><script>steal(document.cookie)</script>"
)

// maxUpload is the policy's per-file limit: 1 MiB.
const maxUpload = 1 << 20

// uploadDir is where accepted files land, under a random name.
const uploadDir = "uploads"

// policy accepts images and PDFs up to maxUpload. Allowed is required:
// there is no accept-everything default, and HTML, SVG, XML, scripts and
// executables stay refused even if a pattern here matched them.
// ds:def id=policy-e9b5m967
var policy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"image/*", "application/pdf"},
	Key:     upload.RandomKey(uploadDir),
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{
		receive, form, formFailures, direct, uppy, fromURL, processAndInspect, sweep,
		layouts, encoded, sizeLimits, perRequest, declared, cacheThenPromote, abandoned,
		keyFromRequest, throughProxy, envelope, publicUploads,
	} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// newDisk returns an empty memory disk for one step.
func newDisk() (*filesystem.Disk, error) {
	return filesystem.NewDisk("uploads", memory.New())
}

// count returns how many objects the disk holds, to show what a refused
// upload left behind (nothing).
func count(ctx context.Context, disk *filesystem.Disk) int {
	n := 0
	for _, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err == nil {
			n++
		}
	}
	return n
}

// receive stores one stream under the policy. The client's file name is
// reduced to a display name and never reaches the key; the type comes
// from the bytes; refused and oversized files store nothing.
// ds:def id=receive-eszpqjvz
func receive(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), `..\..\Holiday Photo.PNG`, policy)
	if err != nil {
		return err
	}
	shape := path.Dir(res.Key) + "/<random>" + path.Ext(res.Key)
	fmt.Fprintf(out, "receive: key %s name %q type %s size %d\n", shape, res.Filename, res.ContentType, res.Size)
	_, err = upload.Receive(ctx, disk, strings.NewReader(htmlFile), "photo.png", policy)
	fmt.Fprintf(out, "receive html named photo.png: %s\n", filesystem.CodeOf(err))
	big := strings.NewReader(pngFile + strings.Repeat("x", maxUpload))
	_, err = upload.Receive(ctx, disk, big, "big.png", policy)
	fmt.Fprintf(out, "receive over MaxSize: %s, objects stored: %d\n", filesystem.CodeOf(err), count(ctx, disk))
	return nil
}
