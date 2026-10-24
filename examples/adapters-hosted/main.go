// Command adapters-hosted walks through the two hosted-storage drivers
// that are not buckets: Vercel Blob (vercel.go) and Cloudinary
// (cloudinary.go). For each it opens a filesystem.Disk, writes, reads,
// stats, lists and deletes, shows the public URLs and the visibility each
// declares and refuses, the capabilities it has and lacks, how keys map to
// what the service stores, and how auth failures, rate limits and size
// limits come back as filesystem error codes.
//
// It runs against in-process fakes of both HTTP APIs
// (internal/fakes), so it needs no account and no network. Against the
// real services the endpoints are left empty and the credentials come from
// the environment.
//
//	go run ./adapters-hosted            (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ubgo/filesystem"
)

// pngHeader is the start of a PNG file: enough for the Disk's sniffing to
// call it image/png.
var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process: it starts the fakes, walks both
// drivers and prints what each step saw to out.
func run(ctx context.Context, out io.Writer) error {
	fmt.Fprintln(out, "== Vercel Blob")
	if err := runVercel(ctx, out); err != nil {
		return fmt.Errorf("vercel blob: %w", err)
	}
	fmt.Fprintln(out, "== Cloudinary")
	if err := runCloudinary(ctx, out); err != nil {
		return fmt.Errorf("cloudinary: %w", err)
	}
	return nil
}

// outcome names an error by its filesystem code, "ok" for nil, plus the
// Retry-After hint when the error carries one.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	s := string(filesystem.CodeOf(err))
	if d, ok := filesystem.RetryAfterOf(err); ok {
		s += fmt.Sprintf(" (retry after %s)", d)
	}
	return s
}

// retryHint is the Retry-After the fakes send with a rate-limit answer.
const retryHint = 30 * time.Second
