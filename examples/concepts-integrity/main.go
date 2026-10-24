// Command concepts-integrity shows how the library proves a stored file is
// the file that was sent: the SHA-256 every write computes while it
// streams, a declared digest and size held against the body before it
// replaces anything, ETags for cheap change checks, and content-addressed
// keys that store identical uploads once.
//
//	go run ./concepts-integrity          (from the examples directory)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// Contents and keys of the demo.
const (
	// reportKey is the object the write steps protect.
	reportKey = "reports/q3.csv"
	// report is its first, good version.
	report = "region,revenue\neu,120\nus,340\n"
	// pngFile sniffs as a PNG, for the upload step.
	pngFile = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR sample image"
	// tenant is the owner the content-addressed step uploads for.
	tenant = "acme"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("files", memory.New())
	if err != nil {
		return err
	}
	for _, step := range []func(context.Context, *filesystem.Disk, io.Writer) error{
		streamedDigest, declaredDigest, declaredSize, etags, contentAddressed, otherDigests,
	} {
		if err := step(ctx, disk, out); err != nil {
			return err
		}
	}
	return nil
}

// digest is the hex SHA-256 of s, as a client or an upstream store would
// compute it.
// ds:def id=digest-fqnk4769
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// streamedDigest writes a file and compares the SHA-256 the Disk returns
// with one computed independently. The Disk hashes every body while it
// streams, on every driver, so the digest costs no second read.
// ds:def id=streameddigest-898w4g6t
func streamedDigest(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Write(ctx, reportKey, strings.NewReader(report), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "write: %d bytes, sha256 matches an independent hash: %t\n", e.Size, e.SHA256 == digest(report))
	return nil
}

// declaredDigest writes with the digest the sender declared. A body that
// was corrupted on the way is refused before the driver commits, so the
// good version stored earlier stays in place.
// ds:def id=declareddigest-74fh5emm
func declaredDigest(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	next := "region,revenue\neu,125\nus,340\n"
	corrupted := strings.Replace(next, "125", "12\x00", 1)
	_, err := disk.Write(ctx, reportKey, strings.NewReader(corrupted), filesystem.WriteOptions{SHA256: digest(next)})
	fmt.Fprintf(out, "corrupted body with a declared sha256: %s\n", filesystem.CodeOf(err))
	b, err := disk.ReadAll(ctx, reportKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored version is still the first: %t\n", string(b) == report)
	if _, err := disk.Write(ctx, reportKey, strings.NewReader(next), filesystem.WriteOptions{SHA256: digest(next)}); err != nil {
		return err
	}
	fmt.Fprintln(out, "good body with a declared sha256: stored")
	return nil
}

// declaredSize holds a body to the length announced for it. A connection
// that drops near the end leaves a short body, which would otherwise be
// stored as if it were whole.
// ds:def id=declaredsize-d6thjej9
func declaredSize(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	truncated := report[:len(report)-5]
	_, err := disk.Write(ctx, "reports/q4.csv", strings.NewReader(truncated), filesystem.WriteOptions{SizeHint: int64(len(report))})
	ok, _ := disk.Exists(ctx, "reports/q4.csv")
	fmt.Fprintf(out, "truncated body with a size hint: %s, stored: %t\n", filesystem.CodeOf(err), ok)
	return nil
}

// etags compares versions without reading bytes: every write returns the
// object's ETag and Stat reports it. What an ETag is depends on the
// backend: the memory driver, like S3 for a single-part upload, uses the
// MD5 of the content, so rewriting the same bytes keeps it; the local
// driver changes it on every write. Treat it as "changed since I looked",
// and the SHA-256 as "same content".
// ds:def id=etags-g9xa5g2g
func etags(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	first, err := disk.WriteBytes(ctx, "a.txt", []byte("same"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	again, err := disk.WriteBytes(ctx, "a.txt", []byte("same"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	changed, err := disk.WriteBytes(ctx, "a.txt", []byte("different"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	now, err := disk.Stat(ctx, "a.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "etag: same bytes keep it (memory): %t, new bytes change it: %t, stat reports the latest: %t\n",
		first.ETag == again.ETag, changed.ETag != again.ETag, now.ETag == changed.ETag)
	return nil
}

// contentAddressed names uploads by their SHA-256, so two uploads of one
// file by one owner share one object and the key proves the content.
// Sharing stays within the owner unless the policy says Shared.
// ds:def id=contentaddressed-fnuean6r
func contentAddressed(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	owner := func(context.Context) (string, error) { return tenant, nil }
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Owner: owner})
	var keys []string
	for _, name := range []string{"logo.png", "logo-copy.png"} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), name, p)
		if err != nil {
			return err
		}
		keys = append(keys, res.Key)
	}
	n := 0
	for _, err := range disk.List(ctx, "blobs", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "content-addressed: same key twice: %t, key is the sha256: %t, objects: %d\n",
		keys[0] == keys[1], strings.TrimSuffix(path.Base(keys[0]), ".png") == digest(pngFile), n)
	return nil
}
