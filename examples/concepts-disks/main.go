// Command concepts-disks tours the Disk API on the memory and local
// drivers: writing and reading, ranges, Stat and Exists, listings with
// pages and folded directories, copy, move and the three deletes, a
// server-side multipart upload, appends, conditional reads and writes, a
// local temporary copy, the conventions every call follows, the start-up
// Check and the As escape hatch.
//
//	go run ./concepts-disks          (from the examples directory)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// Names and keys of this tour. They are the example's choices, not the
// library's.
const (
	// diskName names the disk in errors, logs and signed URLs.
	diskName = "media"
	// greetingKey is the object most steps read and copy.
	greetingKey = "docs/hello.txt"
	// greeting is its content: 14 bytes of plain text.
	greeting = "hello, storage"
	// pageSize is deliberately small so the listing needs several pages.
	pageSize = 2
	// partSize is the size of each multipart part in this demo. Real
	// backends need at least 5 MiB for every part but the last; the memory
	// driver has no minimum.
	partSize = 4
	// clipKey and clip are the object the multipart step uploads in parts.
	clipKey = "video/clip.bin"
	clip    = "partpartpa"
	// wordOffset and wordLength select "storage" inside greeting.
	wordOffset = 7
	wordLength = 7
	// logKey is the object the append step grows line by line.
	logKey = "logs/2026-10-08.log"
	// logCap caps the log's size: an append past it is refused whole.
	logCap = 32
	// docKey is the object the conditional step reads and updates.
	docKey = "feeds/feed.json"
	// tempCap refuses to copy anything larger than this to a local file.
	tempCap = 1 << 20
)

// imported is the modification time the append step stamps on the log, as
// an importer that keeps the source file's time would.
var imported = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	disk, err := newMemoryDisk()
	if err != nil {
		return err
	}
	for _, step := range []func(context.Context, *filesystem.Disk, io.Writer) error{
		writeAndRead, readRange, statAndExists, listTree, copyAndMove, deleteThings, multipartUpload,
		appendAndModTime, conditionalReadAndWrite, tempCopy, conventions,
	} {
		if err := step(ctx, disk, out); err != nil {
			return err
		}
	}
	dir, err := os.MkdirTemp("", "concepts-disks-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return checkAndEscape(ctx, dir, out)
}

// newMemoryDisk builds a Disk over the memory driver. The driver only moves
// bytes; the Disk adds key cleaning, content-type detection, hashing, size
// limits, error classification and interceptors.
// ds:def id=newmemorydisk-7us7uxyf
func newMemoryDisk() (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New())
}

// writeAndRead stores an object and reads it back. The returned Entry
// carries the size and SHA-256 the Disk computed while streaming, and the
// content type it detected from the bytes because none was given.
// ds:def id=writeandread-sxu8j9t4
func writeAndRead(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Write(ctx, greetingKey, strings.NewReader(greeting), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: %d bytes, %s, sha256 %s…\n", e.Key, e.Size, e.ContentType, e.SHA256[:12])
	rc, err := disk.Read(ctx, greetingKey)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read %q\n", b)
	return nil
}

// readRange reads part of an object: Length 0 would mean "to the end".
// ds:def id=readrange-bg2wcst7
func readRange(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	rc, err := disk.ReadRange(ctx, greetingKey, filesystem.ByteRange{Offset: wordOffset, Length: wordLength})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "range %d+%d: %q\n", wordOffset, wordLength, b)
	return nil
}

// statAndExists shows the two ways to ask about a key. Stat of a missing
// key is CodeNotFound; Exists turns exactly that code into false.
// ds:def id=statandexists-jr3aucg4
func statAndExists(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Stat(ctx, greetingKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat %s: %d bytes, %s, visibility %s\n", e.Key, e.Size, e.ContentType, e.Visibility)
	_, err = disk.Stat(ctx, "docs/missing.txt")
	fmt.Fprintf(out, "stat missing: %s\n", filesystem.CodeOf(err))
	ok, err := disk.Exists(ctx, "docs/missing.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "exists missing: %t\n", ok)
	return nil
}

// listTree writes a small tree, then lists it two ways: recursively with
// the List iterator, which fetches pages lazily, and one level at a time
// with ListPage, where sub-directories fold into IsDir entries.
// ds:def id=listtree-kzdkcv86
func listTree(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	for _, k := range []string{"docs/a.txt", "docs/b.txt", "docs/img/logo.png"} {
		if _, err := disk.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	var all []string
	for e, err := range disk.List(ctx, "docs", filesystem.ListOptions{Recursive: true, Limit: pageSize}) {
		if err != nil {
			return err
		}
		all = append(all, e.Key)
	}
	fmt.Fprintf(out, "recursive: %s\n", strings.Join(all, " "))
	opts := filesystem.ListOptions{Limit: pageSize}
	for page := 1; ; page++ {
		p, err := disk.ListPage(ctx, "docs", opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "page %d: %s next=%q\n", page, describe(p.Entries), p.NextCursor)
		if p.NextCursor == "" {
			return nil
		}
		opts.Cursor = p.NextCursor
	}
}

// describe renders entries, marking folded directories with a slash.
func describe(entries []filesystem.Entry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.Key
		if e.IsDir {
			parts[i] += "/"
		}
	}
	return strings.Join(parts, " ")
}

// copyAndMove duplicates and renames objects on one disk. The memory
// driver copies and moves natively; a driver without those capabilities
// gets the Disk's portable fallback with the same result.
// ds:def id=copyandmove-yksfax76
func copyAndMove(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if _, err := disk.Copy(ctx, greetingKey, "backup/hello.txt"); err != nil {
		return err
	}
	e, err := disk.Move(ctx, "backup/hello.txt", "archive/hello.txt")
	if err != nil {
		return err
	}
	moved, err := disk.Exists(ctx, "backup/hello.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "copied and moved to %s (old key exists: %t)\n", e.Key, moved)
	_, err = disk.Copy(ctx, greetingKey, greetingKey)
	fmt.Fprintf(out, "copy onto itself: %s\n", filesystem.CodeOf(err))
	return nil
}

// deleteThings shows the three deletes. Delete of a missing key succeeds,
// DeleteKeys removes a batch, DeletePrefix a whole directory, and the root
// of the disk is never a prefix that can be deleted.
// ds:def id=deletethings-a7pa7647
func deleteThings(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if err := disk.Delete(ctx, "never/written.txt"); err != nil {
		return err
	}
	fmt.Fprintln(out, "deleted a missing key: ok")
	if err := disk.DeleteKeys(ctx, []string{"docs/a.txt", "docs/b.txt", "docs/a.txt"}); err != nil {
		return err
	}
	if err := disk.DeletePrefix(ctx, "archive"); err != nil {
		return err
	}
	err := disk.DeletePrefix(ctx, "")
	fmt.Fprintf(out, "delete the root: %s\n", filesystem.CodeOf(err))
	var left []string
	for e, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		left = append(left, e.Key)
	}
	fmt.Fprintf(out, "left: %s\n", strings.Join(left, " "))
	return nil
}

// multipartUpload stores an object in parts through the server: create,
// upload each part, list what arrived, complete. A client resuming an
// interrupted upload would call ListParts and send only the missing parts.
// ds:def id=multipartupload-7z3ejr3z
func multipartUpload(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	key, body := clipKey, clip
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	var parts []filesystem.Part
	for n, off := filesystem.MinPartNumber, 0; off < len(body); n, off = n+1, off+partSize {
		chunk := body[off:min(off+partSize, len(body))]
		p, err := disk.UploadPart(ctx, key, id, n, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			_ = disk.AbortMultipart(ctx, key, id)
			return err
		}
		parts = append(parts, p)
	}
	listed, err := disk.ListParts(ctx, key, id)
	if err != nil {
		return err
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %d parts listed, %s is %d bytes\n", len(listed), e.Key, e.Size)
	return nil
}

// appendAndModTime grows a log with Append, which creates the object on
// the first call, and then stamps the modification time Stat reports with
// SetModTime. An append that would take the log past MaxSize is refused
// with too_large and appends nothing. Both need their capability; the
// memory, local and SFTP drivers have them.
// ds:def id=appendandmodtime-873u6nvw
func appendAndModTime(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	opts := filesystem.AppendOptions{MaxSize: logCap}
	for _, line := range []string{"started\n", "uploaded 3 files\n"} {
		if _, err := disk.Append(ctx, logKey, strings.NewReader(line), opts); err != nil {
			return err
		}
	}
	_, err := disk.Append(ctx, logKey, strings.NewReader("one line too many\n"), opts)
	fmt.Fprintf(out, "append past %d bytes: %s\n", logCap, filesystem.CodeOf(err))
	if err := disk.SetModTime(ctx, logKey, imported); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, logKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "log: %d bytes, modified %s\n", e.Size, e.ModTime.UTC().Format(time.RFC3339))
	return nil
}

// conditionalReadAndWrite keeps a cached copy current with the least work.
// ReadIf with the cached ETag answers not_modified and no body while the
// object is unchanged; a write with IfMatch succeeds only when nobody wrote
// in between, so the second writer holding the old ETag gets conflict.
// ds:def id=conditionalreadandwrite-8yeavz32
func conditionalReadAndWrite(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	cached, err := disk.WriteBytes(ctx, docKey, []byte(`{"v":1}`), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	_, _, err = disk.ReadIf(ctx, docKey, filesystem.ReadOptions{IfNoneMatch: cached.ETag})
	fmt.Fprintf(out, "read if changed: %s\n", filesystem.CodeOf(err))
	next, err := disk.WriteBytes(ctx, docKey, []byte(`{"v":2}`), filesystem.WriteOptions{IfMatch: cached.ETag})
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, docKey, []byte(`{"v":3}`), filesystem.WriteOptions{IfMatch: cached.ETag})
	fmt.Fprintf(out, "write with a stale etag: %s\n", filesystem.CodeOf(err))
	rc, e, err := disk.ReadIf(ctx, docKey, filesystem.ReadOptions{IfNoneMatch: cached.ETag})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read if changed: %s (etag is the new one: %t)\n", b, e.ETag == next.ETag)
	return nil
}

// tempCopy copies an object to a local temporary file for a tool that
// needs a path (ffmpeg, libvips, pdftotext). The file keeps the key's
// short extension, has mode 0600, and is the caller's to remove.
// ds:def id=tempcopy-v3f5qefn
func tempCopy(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	path, err := disk.TempFile(ctx, greetingKey, filesystem.TempFileOptions{MaxSize: tempCap})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "temp file: extension %s, %q\n", filepath.Ext(path), b)
	_, err = disk.TempFile(ctx, greetingKey, filesystem.TempFileOptions{MaxSize: wordLength})
	fmt.Fprintf(out, "temp file over its cap: %s\n", filesystem.CodeOf(err))
	return nil
}

// conventions shows three rules every call follows. The context is the
// first argument and an ended one stops the call, classified unavailable
// and still matching context.Canceled; the disk speaks standard library
// types, here io/fs through FS; and options are typed values checked when
// the disk is built, so a bad one fails at start-up instead of at first use.
// ds:def id=conventions-cfsevhds
func conventions(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := disk.WriteBytes(cancelled, "late.txt", []byte("x"), filesystem.WriteOptions{})
	stored, _ := disk.Exists(ctx, "late.txt")
	fmt.Fprintf(out, "cancelled write: %s, is context.Canceled: %t, stored: %t\n",
		filesystem.CodeOf(err), errors.Is(err, context.Canceled), stored)
	b, err := fs.ReadFile(disk.FS(ctx), greetingKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "fs.ReadFile: %q\n", b)
	_, err = filesystem.NewDisk(diskName, memory.New(), filesystem.WithPublicURL("cdn.example.com"))
	fmt.Fprintf(out, "bad option at start-up: %s\n", filesystem.CodeOf(err))
	return nil
}

// checkAndEscape opens a local disk, proves it works end to end with Check
// (write, read back, compare, delete), and reaches the driver's native
// *os.Root through the typed escape hatch As.
// ds:def id=checkandescape-bt2t74mr
func checkAndEscape(ctx context.Context, dir string, out io.Writer) error {
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
	if err := disk.Check(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "check: ok")
	root, ok := filesystem.As[*os.Root](disk)
	fmt.Fprintf(out, "as *os.Root: %t, rooted at the disk's directory: %t\n", ok, ok && root.Name() == dir)
	_, ok = filesystem.As[*strings.Builder](disk)
	fmt.Fprintf(out, "as *strings.Builder: %t\n", ok)
	return nil
}
