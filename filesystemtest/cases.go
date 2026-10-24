package filesystemtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// Cases returns every conformance case, in a stable order.
func Cases() []Case {
	return append([]Case{
		{Name: "WriteThenRead", Check: writeThenRead},
		{Name: "OverwriteReplaces", Check: overwriteReplaces},
		{Name: "EmptyObject", Check: emptyObject},
		{Name: "LargeObject", Check: largeObject},
		{Name: "WriteReportsSizeAndHash", Check: writeReportsSizeAndHash},
		{Name: "StatMissingIsNotFound", Check: statMissingIsNotFound},
		{Name: "ReadMissingIsNotFound", Check: readMissingIsNotFound},
		{Name: "StatReportsSizeAndTime", Check: statReportsSizeAndTime},
		{Name: "Exists", Check: exists},
		{Name: "DeleteRemoves", Check: deleteRemoves},
		{Name: "DeleteMissingSucceeds", Check: deleteMissingSucceeds},
		{Name: "ReadRanges", Check: readRanges},
		{Name: "UnicodeAndSpacesInKeys", Check: unicodeKeys},
		{Name: "FailingBodyStoresNothing", Check: failingBodyStoresNothing},
		{Name: "MaxSizeStoresNothing", Check: maxSizeStoresNothing},
		{Name: "DetectsContentType", Check: detectsContentType},
		{Name: "ListRecursive", Check: listRecursive},
		{Name: "ListFoldsDirectories", Check: listFoldsDirectories},
		{Name: "ListPaginates", Check: listPaginates},
		{Name: "ListEmptyDirectory", Check: listEmptyDirectory},
		{Name: "ListDoesNotMatchSiblingPrefix", Check: listSiblingPrefix},
		{Name: "CopyDuplicates", Check: copyDuplicates},
		{Name: "CopyMissingIsNotFound", Check: copyMissing},
		{Name: "MoveRenames", Check: moveRenames},
		{Name: "DeletePrefixScoped", Check: deletePrefixScoped},
		{Name: "DeleteKeys", Check: deleteKeys},
		{Name: "ConcurrentWrites", Check: concurrentWrites},
		{Name: "StoresContentType", Needs: func(c filesystem.Capabilities) bool { return c.ContentType }, Check: storesContentType},
		{Name: "StoresMetadata", Needs: func(c filesystem.Capabilities) bool { return c.Metadata }, Check: storesMetadata},
		{Name: "StoresHeaders", Needs: func(c filesystem.Capabilities) bool { return c.Headers }, Check: storesHeaders},
		{Name: "HeadersUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.Headers }, Check: headersRefused},
		{Name: "MetadataUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.Metadata }, Check: metadataRefused},
		{Name: "ConditionalWrite", Needs: func(c filesystem.Capabilities) bool { return c.ConditionalWrite }, Check: conditionalWrite},
		{Name: "ConditionalWriteUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.ConditionalWrite }, Check: conditionalWriteRefused},
		{Name: "Visibility", Needs: func(c filesystem.Capabilities) bool { return c.SetVisibility }, Check: visibility},
		{Name: "Multipart", Needs: func(c filesystem.Capabilities) bool { return c.Multipart }, Check: multipart},
		{Name: "MultipartAbort", Needs: func(c filesystem.Capabilities) bool { return c.Multipart }, Check: multipartAbort},
		{Name: "SweepAbandoned", Needs: func(c filesystem.Capabilities) bool { return c.SweepAbandoned && c.Multipart }, Check: sweepAbandoned},
		{Name: "SweepAbandonedKeepsObjects", Needs: func(c filesystem.Capabilities) bool { return c.SweepAbandoned && !c.Multipart }, Check: sweepAbandonedKeepsObjects},
		{Name: "PresignPostForm", Needs: func(c filesystem.Capabilities) bool { return c.PresignPost }, Check: presignPostForm},
		{Name: "PresignPostUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.PresignPost }, Check: presignPostRefused},
		{Name: "SweepAbandonedUnsupportedIsRefused", Needs: func(c filesystem.Capabilities) bool { return !c.SweepAbandoned }, Check: sweepAbandonedRefused},
	}, append(opCases(), dirCases()...)...)
}

// put writes content under key with default options.
func put(ctx context.Context, d *filesystem.Disk, key, content string) error {
	if _, err := d.WriteBytes(ctx, key, []byte(content), filesystem.WriteOptions{}); err != nil {
		return fmt.Errorf("write %q: %w", key, err)
	}
	return nil
}

// expect reads key and compares it with want.
func expect(ctx context.Context, d *filesystem.Disk, key, want string) error {
	got, err := d.ReadAll(ctx, key)
	if err != nil {
		return fmt.Errorf("read %q: %w", key, err)
	}
	if string(got) != want {
		return fmt.Errorf("read %q = %q, want %q", key, got, want)
	}
	return nil
}

// expectCode checks that err carries code.
func expectCode(what string, err error, code filesystem.Code) error {
	if got := filesystem.CodeOf(err); got != code {
		return fmt.Errorf("%s: error code %q (err %v), want %q", what, got, err, code)
	}
	return nil
}

// keys lists every key under dir (recursively unless folded) in order.
func keys(ctx context.Context, d *filesystem.Disk, dir string, opts filesystem.ListOptions) ([]string, error) {
	var out []string
	for e, err := range d.List(ctx, dir, opts) {
		if err != nil {
			return nil, err
		}
		name := e.Key
		if e.IsDir {
			name += "/"
		}
		out = append(out, name)
	}
	return out, nil
}

func expectKeys(ctx context.Context, d *filesystem.Disk, dir string, opts filesystem.ListOptions, want ...string) error {
	got, err := keys(ctx, d, dir, opts)
	if err != nil {
		return fmt.Errorf("list %q: %w", dir, err)
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("list %q (recursive=%v) = %q, want %q", dir, opts.Recursive, got, want)
	}
	return nil
}

func writeThenRead(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "docs/readme.txt", "hello"); err != nil {
		return err
	}
	return expect(ctx, d, "docs/readme.txt", "hello")
}

func overwriteReplaces(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "a.txt", "first version"); err != nil {
		return err
	}
	if err := put(ctx, d, "a.txt", "second"); err != nil {
		return err
	}
	return expect(ctx, d, "a.txt", "second")
}

func emptyObject(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "empty", ""); err != nil {
		return err
	}
	e, err := d.Stat(ctx, "empty")
	if err != nil {
		return fmt.Errorf("stat empty: %w", err)
	}
	if e.Size != 0 {
		return fmt.Errorf("empty object size %d", e.Size)
	}
	return expect(ctx, d, "empty", "")
}

// largeSize is above common internal buffer sizes (32 KiB copy buffers, 5 MiB
// would be multipart territory and is covered by the multipart case).
const largeSize = 3<<20 + 7

func largeObject(ctx context.Context, d *filesystem.Disk) error {
	data := bytes.Repeat([]byte("0123456789abcdef"), largeSize/16+1)[:largeSize]
	if _, err := d.Write(ctx, "big.bin", io.MultiReader(bytes.NewReader(data)), filesystem.WriteOptions{}); err != nil {
		return fmt.Errorf("write big: %w", err)
	}
	got, err := d.ReadAll(ctx, "big.bin")
	if err != nil {
		return fmt.Errorf("read big: %w", err)
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("big object read back %d bytes, differs from the %d written", len(got), len(data))
	}
	return nil
}

func writeReportsSizeAndHash(ctx context.Context, d *filesystem.Disk) error {
	e, err := d.WriteBytes(ctx, "h.txt", []byte("abc"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	const sha256OfABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if e.Size != 3 || e.SHA256 != sha256OfABC || e.Key != "h.txt" {
		return fmt.Errorf("write entry = %+v, want size 3, key h.txt and the SHA-256 of abc", e)
	}
	return nil
}

func statMissingIsNotFound(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.Stat(ctx, "missing/file")
	return expectCode("stat missing", err, filesystem.CodeNotFound)
}

func readMissingIsNotFound(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.Read(ctx, "missing")
	return expectCode("read missing", err, filesystem.CodeNotFound)
}

func statReportsSizeAndTime(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "s/t.txt", "12345"); err != nil {
		return err
	}
	e, err := d.Stat(ctx, "s/t.txt")
	if err != nil {
		return err
	}
	if e.Size != 5 || e.ModTime.IsZero() || e.IsDir || e.Key != "s/t.txt" {
		return fmt.Errorf("stat = %+v, want size 5, a mod time, key s/t.txt", e)
	}
	return nil
}

func exists(ctx context.Context, d *filesystem.Disk) error {
	ok, err := d.Exists(ctx, "x")
	if err != nil || ok {
		return fmt.Errorf("exists before write = %v, %v", ok, err)
	}
	if err := put(ctx, d, "x", "1"); err != nil {
		return err
	}
	ok, err = d.Exists(ctx, "x")
	if err != nil || !ok {
		return fmt.Errorf("exists after write = %v, %v", ok, err)
	}
	return nil
}

func deleteRemoves(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "dir/gone.txt", "x"); err != nil {
		return err
	}
	if err := d.Delete(ctx, "dir/gone.txt"); err != nil {
		return err
	}
	_, err := d.Stat(ctx, "dir/gone.txt")
	if err := expectCode("stat after delete", err, filesystem.CodeNotFound); err != nil {
		return err
	}
	return expectKeys(ctx, d, "", filesystem.ListOptions{})
}

func deleteMissingSucceeds(ctx context.Context, d *filesystem.Disk) error {
	if err := d.Delete(ctx, "never/existed"); err != nil {
		return fmt.Errorf("delete missing: %w", err)
	}
	return nil
}

func readRanges(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "r.txt", "0123456789"); err != nil {
		return err
	}
	cases := []struct {
		rng  filesystem.ByteRange
		want string
	}{
		{filesystem.ByteRange{}, "0123456789"},
		{filesystem.ByteRange{Offset: 3}, "3456789"},
		{filesystem.ByteRange{Offset: 2, Length: 4}, "2345"},
		{filesystem.ByteRange{Offset: 8, Length: 10}, "89"},
		{filesystem.ByteRange{Offset: 10}, ""},
		{filesystem.ByteRange{Offset: 50, Length: 5}, ""},
	}
	for _, c := range cases {
		rc, err := d.ReadRange(ctx, "r.txt", c.rng)
		if err != nil {
			return fmt.Errorf("range %+v: %w", c.rng, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("range %+v read: %w", c.rng, err)
		}
		if string(got) != c.want {
			return fmt.Errorf("range %+v = %q, want %q", c.rng, got, c.want)
		}
	}
	return nil
}

func unicodeKeys(ctx context.Context, d *filesystem.Disk) error {
	keys := []string{"naïve café/résumé.pdf", "spaces in/name with spaces.txt", "emoji/🎉.txt", "plus+and=equals&amp.txt"}
	for _, k := range keys {
		if err := put(ctx, d, k, k); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if err := expect(ctx, d, k, k); err != nil {
			return err
		}
	}
	return nil
}

// failingReader yields failAfter bytes, then fails. failAfter is larger than
// the Disk's content-sniffing window (sniff.HeadSize), so the failure
// reaches the driver mid-stream rather than the Disk's own head read: a
// case that failed inside the Disk would never test the driver at all.
//
// err is the failure. Bodies fail with ordinary errors and also with
// io.ErrUnexpectedEOF (a client upload cut short), which some libraries
// mistake for a normal end and so commit a truncated object.
type failingReader struct {
	sent int
	err  error
}

const failAfter = 4096

var errBodyFailed = errors.New("body failed on purpose")

func (f *failingReader) Read(p []byte) (int, error) {
	if f.sent >= failAfter {
		return 0, f.err
	}
	n := min(len(p), failAfter-f.sent)
	for i := range n {
		p[i] = 'p'
	}
	f.sent += n
	return n, nil
}

func failingBodyStoresNothing(ctx context.Context, d *filesystem.Disk) error {
	for _, cause := range []error{errBodyFailed, io.ErrUnexpectedEOF} {
		_, err := d.Write(ctx, "broken.txt", &failingReader{err: cause}, filesystem.WriteOptions{})
		if err == nil {
			return fmt.Errorf("write with a body failing with %q succeeded", cause)
		}
		_, err = d.Stat(ctx, "broken.txt")
		if err := expectCode("stat after failed write", err, filesystem.CodeNotFound); err != nil {
			return fmt.Errorf("body failing with %q: %w", cause, err)
		}
		if err := put(ctx, d, "kept.txt", "original"); err != nil {
			return err
		}
		if _, err := d.Write(ctx, "kept.txt", &failingReader{err: cause}, filesystem.WriteOptions{}); err == nil {
			return fmt.Errorf("overwrite with a body failing with %q succeeded", cause)
		}
		if err := expect(ctx, d, "kept.txt", "original"); err != nil {
			return fmt.Errorf("body failing with %q: %w", cause, err)
		}
	}
	return nil
}

func maxSizeStoresNothing(ctx context.Context, d *filesystem.Disk) error {
	body := strings.NewReader(strings.Repeat("x", 100))
	_, err := d.Write(ctx, "big.txt", io.MultiReader(body), filesystem.WriteOptions{MaxSize: 10})
	if err := expectCode("write over MaxSize", err, filesystem.CodeTooLarge); err != nil {
		return err
	}
	_, err = d.Stat(ctx, "big.txt")
	if err := expectCode("stat after oversize write", err, filesystem.CodeNotFound); err != nil {
		return err
	}
	_, err = d.WriteBytes(ctx, "fits.txt", []byte("0123456789"), filesystem.WriteOptions{MaxSize: 10})
	return err
}

// pngHeader is the eight-byte PNG signature plus an IHDR chunk start.
var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func detectsContentType(ctx context.Context, d *filesystem.Disk) error {
	e, err := d.WriteBytes(ctx, "image.bin", pngHeader, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	if e.ContentType != "image/png" {
		return fmt.Errorf("detected content type %q, want image/png", e.ContentType)
	}
	return nil
}

func storesContentType(ctx context.Context, d *filesystem.Disk) error {
	const ct = "application/vnd.example+json"
	if _, err := d.WriteBytes(ctx, "typed", []byte("{}"), filesystem.WriteOptions{ContentType: ct}); err != nil {
		return err
	}
	e, err := d.Stat(ctx, "typed")
	if err != nil {
		return err
	}
	if e.ContentType != ct {
		return fmt.Errorf("stored content type %q, want %q", e.ContentType, ct)
	}
	return nil
}

func storesMetadata(ctx context.Context, d *filesystem.Disk) error {
	md := map[string]string{"owner": "u1", "purpose": "avatar"}
	if _, err := d.WriteBytes(ctx, "m", []byte("x"), filesystem.WriteOptions{Metadata: md}); err != nil {
		return err
	}
	e, err := d.Stat(ctx, "m")
	if err != nil {
		return err
	}
	for k, v := range md {
		if e.Metadata[k] != v {
			return fmt.Errorf("metadata %q = %q, want %q (got %v)", k, e.Metadata[k], v, e.Metadata)
		}
	}
	return nil
}

func metadataRefused(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.WriteBytes(ctx, "m", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"a": "b"}})
	return expectCode("metadata on a driver without it", err, filesystem.CodeUnsupported)
}

// sampleHeaders are the headers storesHeaders writes; gzip is honest here,
// because the body is gzip data.
var sampleHeaders = filesystem.HTTPHeaders{
	CacheControl:       "public, max-age=31536000, immutable",
	ContentDisposition: `attachment; filename="report.txt"`,
	ContentEncoding:    "gzip",
}

// gzipHello is "hello" compressed with gzip.
const gzipHello = "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff\xcaH\xcd\xc9\xc9\x07\x04\x00\x00\xff\xff\x86\xa6\x106\x05\x00\x00\x00"

// storesHeaders checks the headers survive Stat and Copy, and that a
// gzip-encoded object reads back as the stored bytes, not decompressed.
func storesHeaders(ctx context.Context, d *filesystem.Disk) error {
	if _, err := d.WriteBytes(ctx, "h.txt.gz", []byte(gzipHello), filesystem.WriteOptions{Headers: sampleHeaders}); err != nil {
		return err
	}
	for _, key := range []string{"h.txt.gz", "h-copy.txt.gz"} {
		if key == "h-copy.txt.gz" {
			if _, err := d.Copy(ctx, "h.txt.gz", key); err != nil {
				return fmt.Errorf("copy: %w", err)
			}
		}
		e, err := d.Stat(ctx, key)
		if err != nil {
			return err
		}
		if e.Headers != sampleHeaders {
			return fmt.Errorf("headers of %s = %+v, want %+v", key, e.Headers, sampleHeaders)
		}
		if err := expect(ctx, d, key, gzipHello); err != nil {
			return fmt.Errorf("an encoded object must read back as stored: %w", err)
		}
	}
	_, err := d.WriteBytes(ctx, "bad", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "max-age=1\r\nSet-Cookie: a=b"}})
	return expectCode("a header value with CR LF", err, filesystem.CodeInvalidArgument)
}

func headersRefused(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.WriteBytes(ctx, "h", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	return expectCode("headers on a driver without them", err, filesystem.CodeUnsupported)
}

func conditionalWrite(ctx context.Context, d *filesystem.Disk) error {
	opts := filesystem.WriteOptions{IfNotExists: true}
	if _, err := d.WriteBytes(ctx, "once", []byte("first"), opts); err != nil {
		return fmt.Errorf("first conditional write: %w", err)
	}
	_, err := d.WriteBytes(ctx, "once", []byte("second"), opts)
	if err := expectCode("second conditional write", err, filesystem.CodeExists); err != nil {
		return err
	}
	return expect(ctx, d, "once", "first")
}

// conditionalWriteRefused checks that IfNotExists on a disk without atomic
// conditional writes is refused, never degraded to a plain overwrite.
func conditionalWriteRefused(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.WriteBytes(ctx, "once", []byte("x"), filesystem.WriteOptions{IfNotExists: true})
	return expectCode("IfNotExists on a driver without conditional writes", err, filesystem.CodeUnsupported)
}

func visibility(ctx context.Context, d *filesystem.Disk) error {
	if _, err := d.WriteBytes(ctx, "v", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	e, err := d.Stat(ctx, "v")
	if err != nil {
		return err
	}
	if e.Visibility != filesystem.VisibilityPublic {
		return fmt.Errorf("visibility after public write = %q", e.Visibility)
	}
	if err := d.SetVisibility(ctx, "v", filesystem.VisibilityPrivate); err != nil {
		return err
	}
	if e, err = d.Stat(ctx, "v"); err != nil {
		return err
	}
	if e.Visibility != filesystem.VisibilityPrivate {
		return fmt.Errorf("visibility after SetVisibility(private) = %q", e.Visibility)
	}
	err = d.SetVisibility(ctx, "missing", filesystem.VisibilityPublic)
	return expectCode("set visibility on missing key", err, filesystem.CodeNotFound)
}

// tree writes a fixed layout used by the listing cases.
func tree(ctx context.Context, d *filesystem.Disk) error {
	for _, k := range []string{"a/1.txt", "a/2.txt", "a/sub/3.txt", "a/sub/deep/4.txt", "b/5.txt", "root.txt"} {
		if err := put(ctx, d, k, k); err != nil {
			return err
		}
	}
	return nil
}

func listRecursive(ctx context.Context, d *filesystem.Disk) error {
	if err := tree(ctx, d); err != nil {
		return err
	}
	if err := expectKeys(ctx, d, "a", filesystem.ListOptions{Recursive: true}, "a/1.txt", "a/2.txt", "a/sub/3.txt", "a/sub/deep/4.txt"); err != nil {
		return err
	}
	return expectKeys(ctx, d, "", filesystem.ListOptions{Recursive: true}, "a/1.txt", "a/2.txt", "a/sub/3.txt", "a/sub/deep/4.txt", "b/5.txt", "root.txt")
}

func listFoldsDirectories(ctx context.Context, d *filesystem.Disk) error {
	if err := tree(ctx, d); err != nil {
		return err
	}
	if err := expectKeys(ctx, d, "", filesystem.ListOptions{}, "a/", "b/", "root.txt"); err != nil {
		return err
	}
	return expectKeys(ctx, d, "a", filesystem.ListOptions{}, "a/1.txt", "a/2.txt", "a/sub/")
}

func listPaginates(ctx context.Context, d *filesystem.Disk) error {
	if err := tree(ctx, d); err != nil {
		return err
	}
	page, err := d.ListPage(ctx, "", filesystem.ListOptions{Recursive: true, Limit: 4})
	if err != nil {
		return err
	}
	if len(page.Entries) != 4 || page.NextCursor == "" {
		return fmt.Errorf("first page of 4 = %d entries, cursor %q", len(page.Entries), page.NextCursor)
	}
	rest, err := d.ListPage(ctx, "", filesystem.ListOptions{Recursive: true, Limit: 4, Cursor: page.NextCursor})
	if err != nil {
		return err
	}
	if len(rest.Entries) != 2 || rest.NextCursor != "" {
		return fmt.Errorf("second page = %d entries, cursor %q; want 2 and no cursor", len(rest.Entries), rest.NextCursor)
	}
	return expectKeys(ctx, d, "", filesystem.ListOptions{Recursive: true, Limit: 1}, "a/1.txt", "a/2.txt", "a/sub/3.txt", "a/sub/deep/4.txt", "b/5.txt", "root.txt")
}

func listEmptyDirectory(ctx context.Context, d *filesystem.Disk) error {
	if err := expectKeys(ctx, d, "nothing/here", filesystem.ListOptions{Recursive: true}); err != nil {
		return err
	}
	return expectKeys(ctx, d, "", filesystem.ListOptions{})
}

func listSiblingPrefix(ctx context.Context, d *filesystem.Disk) error {
	for _, k := range []string{"photo/1.jpg", "photos/2.jpg", "photo.jpg"} {
		if err := put(ctx, d, k, "x"); err != nil {
			return err
		}
	}
	return expectKeys(ctx, d, "photo", filesystem.ListOptions{Recursive: true}, "photo/1.jpg")
}

func copyDuplicates(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "src.txt", "payload"); err != nil {
		return err
	}
	if _, err := d.Copy(ctx, "src.txt", "copies/dst.txt"); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := expect(ctx, d, "copies/dst.txt", "payload"); err != nil {
		return err
	}
	return expect(ctx, d, "src.txt", "payload")
}

func copyMissing(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.Copy(ctx, "nope", "dst")
	return expectCode("copy missing source", err, filesystem.CodeNotFound)
}

func moveRenames(ctx context.Context, d *filesystem.Disk) error {
	if err := put(ctx, d, "old/name.txt", "moving"); err != nil {
		return err
	}
	if _, err := d.Move(ctx, "old/name.txt", "new/name.txt"); err != nil {
		return fmt.Errorf("move: %w", err)
	}
	if err := expect(ctx, d, "new/name.txt", "moving"); err != nil {
		return err
	}
	_, err := d.Stat(ctx, "old/name.txt")
	if err := expectCode("source after move", err, filesystem.CodeNotFound); err != nil {
		return err
	}
	_, err = d.Move(ctx, "old/name.txt", "elsewhere")
	return expectCode("move missing source", err, filesystem.CodeNotFound)
}

func deletePrefixScoped(ctx context.Context, d *filesystem.Disk) error {
	for _, k := range []string{"tmp/a", "tmp/b/c", "tmpx/keep", "tmp.txt"} {
		if err := put(ctx, d, k, "x"); err != nil {
			return err
		}
	}
	if err := d.DeletePrefix(ctx, "tmp"); err != nil {
		return fmt.Errorf("delete prefix: %w", err)
	}
	if err := expectKeys(ctx, d, "", filesystem.ListOptions{Recursive: true}, "tmp.txt", "tmpx/keep"); err != nil {
		return err
	}
	err := d.DeletePrefix(ctx, "")
	return expectCode("delete prefix of the root", err, filesystem.CodeInvalidArgument)
}

func deleteKeys(ctx context.Context, d *filesystem.Disk) error {
	for _, k := range []string{"batch/a", "batch/b", "batch/c", "batch/keep"} {
		if err := put(ctx, d, k, "x"); err != nil {
			return err
		}
	}
	err := d.DeleteKeys(ctx, []string{"batch/a", "batch/b", "batch/a", "batch/missing", "../escape", "batch/c"})
	var batch *filesystem.BatchError
	if !errors.As(err, &batch) || len(batch.Failures) != 1 || batch.Failures[0].Key != "../escape" ||
		filesystem.CodeOf(batch.Failures[0].Err) != filesystem.CodeInvalidKey {
		return fmt.Errorf("delete keys = %v, want only the invalid key reported, as invalid_key", err)
	}
	if err := expectKeys(ctx, d, "batch", filesystem.ListOptions{Recursive: true}, "batch/keep"); err != nil {
		return err
	}
	if err := d.DeleteKeys(ctx, []string{"batch/keep"}); err != nil {
		return fmt.Errorf("delete one key: %w", err)
	}
	return d.DeleteKeys(ctx, nil)
}

// concurrentWriters is enough goroutines to expose a missing lock without
// slowing the suite down on remote backends.
const concurrentWriters = 16

func concurrentWrites(ctx context.Context, d *filesystem.Disk) error {
	var wg sync.WaitGroup
	errs := make(chan error, concurrentWriters)
	for i := range concurrentWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- put(ctx, d, fmt.Sprintf("c/%02d", i), fmt.Sprint(i))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	got, err := keys(ctx, d, "c", filesystem.ListOptions{Recursive: true})
	if err != nil {
		return err
	}
	if len(got) != concurrentWriters {
		return fmt.Errorf("after %d concurrent writes, %d keys listed", concurrentWriters, len(got))
	}
	return nil
}

// partSize is S3's minimum part size, so the case is valid on real S3.
const partSize = 5 << 20

func multipart(ctx context.Context, d *filesystem.Disk) error {
	id, err := d.CreateMultipart(ctx, "mp/file.bin", filesystem.WriteOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("create multipart: %w", err)
	}
	first := bytes.Repeat([]byte("a"), partSize)
	second := []byte("tail")
	p1, err := d.UploadPart(ctx, "mp/file.bin", id, 1, bytes.NewReader(first), int64(len(first)))
	if err != nil {
		return fmt.Errorf("upload part 1: %w", err)
	}
	p2, err := d.UploadPart(ctx, "mp/file.bin", id, 2, bytes.NewReader(second), int64(len(second)))
	if err != nil {
		return fmt.Errorf("upload part 2: %w", err)
	}
	listed, err := d.ListParts(ctx, "mp/file.bin", id)
	if err != nil {
		return fmt.Errorf("list parts: %w", err)
	}
	if len(listed) != 2 || listed[0].Number != 1 || listed[1].Number != 2 || listed[0].ETag != p1.ETag || listed[1].ETag != p2.ETag {
		return fmt.Errorf("list parts = %+v, want parts 1 and 2 with their ETags", listed)
	}
	if _, err := d.CompleteMultipart(ctx, "mp/file.bin", id, []filesystem.Part{p1, p2}); err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	got, err := d.ReadAll(ctx, "mp/file.bin")
	if err != nil {
		return err
	}
	if !bytes.Equal(got, append(first, second...)) {
		return fmt.Errorf("assembled object is %d bytes, want %d", len(got), len(first)+len(second))
	}
	return nil
}

func multipartAbort(ctx context.Context, d *filesystem.Disk) error {
	id, err := d.CreateMultipart(ctx, "mp/aborted.bin", filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	if _, err := d.UploadPart(ctx, "mp/aborted.bin", id, 1, strings.NewReader("data"), 4); err != nil {
		return err
	}
	if err := d.AbortMultipart(ctx, "mp/aborted.bin", id); err != nil {
		return fmt.Errorf("abort: %w", err)
	}
	if _, err := d.Stat(ctx, "mp/aborted.bin"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return fmt.Errorf("aborted upload left an object (stat err %v)", err)
	}
	if _, err := d.ListParts(ctx, "mp/aborted.bin", id); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return fmt.Errorf("aborted upload still lists parts (err %v)", err)
	}
	return nil
}

// The sweep cases abort every upload and remove every staging file on the
// backend that is older than their cutoff, not only their own: run the
// suite against a dedicated bucket, container or directory (see Run).
// Their assertions concern only what they created.
//
// sweepMargin puts the sweep cutoffs of the SweepAbandoned case far enough
// from now that clock skew between the test and a remote backend, and the
// time the case takes, cannot move an upload to the other side.
const sweepMargin = time.Hour

func sweepAbandoned(ctx context.Context, d *filesystem.Disk) error {
	const key, kept = "mp/abandoned.bin", "mp/kept.bin"
	if err := put(ctx, d, kept, "stored"); err != nil {
		return err
	}
	id, err := d.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	if _, err := d.UploadPart(ctx, key, id, 1, strings.NewReader("data"), 4); err != nil {
		return err
	}
	// A cutoff in the past leaves a fresh upload alone.
	if _, err := d.SweepAbandoned(ctx, time.Now().Add(-sweepMargin)); err != nil {
		return fmt.Errorf("sweep before the upload: %w", err)
	}
	if parts, err := d.ListParts(ctx, key, id); err != nil || len(parts) != 1 {
		return fmt.Errorf("a sweep with an older cutoff removed an upload in progress (parts %v, err %v)", parts, err)
	}
	// A cutoff after its last activity removes it, and only it.
	n, err := d.SweepAbandoned(ctx, time.Now().Add(sweepMargin))
	if err != nil {
		return fmt.Errorf("sweep after the upload: %w", err)
	}
	if n < 1 {
		return fmt.Errorf("sweep reported %d removed, want at least the abandoned upload", n)
	}
	if _, err := d.ListParts(ctx, key, id); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return fmt.Errorf("swept upload still lists parts (err %v)", err)
	}
	if _, err := d.Stat(ctx, key); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return fmt.Errorf("swept upload left an object (stat err %v)", err)
	}
	return expect(ctx, d, kept, "stored")
}

// sweepAbandonedKeepsObjects is the sweep case for drivers whose only
// leftovers are staging files (no Multipart to create an abandoned upload
// with): a sweep that counts everything as abandoned must succeed, remove
// no stored object, and leave the disk writable.
func sweepAbandonedKeepsObjects(ctx context.Context, d *filesystem.Disk) error {
	const kept, after = "sweep/kept.txt", "sweep/after.txt"
	if err := put(ctx, d, kept, "stored"); err != nil {
		return err
	}
	if _, err := d.SweepAbandoned(ctx, time.Now().Add(sweepMargin)); err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	if err := expect(ctx, d, kept, "stored"); err != nil {
		return fmt.Errorf("sweep removed a stored object: %w", err)
	}
	if err := put(ctx, d, after, "written after the sweep"); err != nil {
		return fmt.Errorf("write after the sweep: %w", err)
	}
	return expect(ctx, d, after, "written after the sweep")
}

func sweepAbandonedRefused(ctx context.Context, d *filesystem.Disk) error {
	if _, err := d.SweepAbandoned(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		return fmt.Errorf("SweepAbandoned without the capability: err %v, want unsupported", err)
	}
	return nil
}

// postFormExpiry is the lifetime the PresignPostForm case asks for, and
// postClockSlack the clock difference it tolerates with the backend.
const (
	postFormExpiry = 10 * time.Minute
	postClockSlack = time.Minute
)

// presignPostForm checks the form a driver builds (the real POST is each
// adapter's own test, against its backend): an absolute URL, the key or
// key prefix, the content type and stored headers as fields under the
// core's names, and an expiry that matches the request.
func presignPostForm(ctx context.Context, d *filesystem.Disk) error {
	var headers filesystem.HTTPHeaders
	if d.Capabilities().Headers {
		headers = filesystem.HTTPHeaders{CacheControl: "public, max-age=60", ContentDisposition: "attachment"}
	}
	start := time.Now()
	post, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "post/a.png", ContentType: "image/png", MinSize: 1, MaxSize: 1 << 20, Expires: postFormExpiry, Headers: headers})
	if err != nil {
		return fmt.Errorf("presign post: %w", err)
	}
	if u, err := url.Parse(post.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("form URL %q is not an absolute http(s) URL", post.URL)
	}
	if !strings.HasSuffix(post.Fields[filesystem.PostFieldKey], "post/a.png") {
		return fmt.Errorf("key field %q does not name post/a.png", post.Fields[filesystem.PostFieldKey])
	}
	if post.Fields[filesystem.PostFieldContentType] != "image/png" {
		return fmt.Errorf("content type field %q, want image/png", post.Fields[filesystem.PostFieldContentType])
	}
	if got := filesystem.HTTPHeadersFrom(fieldsAsHeader(post.Fields)); got != headers {
		return fmt.Errorf("stored header fields %+v, want %+v", got, headers)
	}
	if post.ExpiresAt.Before(start.Add(postFormExpiry-postClockSlack)) || post.ExpiresAt.After(time.Now().Add(postFormExpiry+postClockSlack)) {
		return fmt.Errorf("form expires at %v, want about %v from now", post.ExpiresAt, postFormExpiry)
	}
	post, err = d.PresignPost(ctx, filesystem.PostPolicy{KeyPrefix: "post/in", MaxSize: 1 << 20, Expires: postFormExpiry})
	if err != nil {
		return fmt.Errorf("presign post under a prefix: %w", err)
	}
	if !strings.HasSuffix(post.Fields[filesystem.PostFieldKey], "post/in/"+filesystem.PostFilenamePlaceholder) {
		return fmt.Errorf("prefix key field %q, want post/in/%s", post.Fields[filesystem.PostFieldKey], filesystem.PostFilenamePlaceholder)
	}
	if ct, ok := post.Fields[filesystem.PostFieldContentType]; ok {
		return fmt.Errorf("a form accepting any type fixes Content-Type %q", ct)
	}
	return nil
}

// fieldsAsHeader reads form fields as an http.Header, so HTTPHeadersFrom
// finds the stored-header fields.
func fieldsAsHeader(fields map[string]string) http.Header {
	h := http.Header{}
	for k, v := range fields {
		h.Set(k, v)
	}
	return h
}

func presignPostRefused(ctx context.Context, d *filesystem.Disk) error {
	_, err := d.PresignPost(ctx, filesystem.PostPolicy{Key: "a.png", MaxSize: 1, Expires: time.Minute})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		return fmt.Errorf("PresignPost without the capability: err %v, want unsupported", err)
	}
	return nil
}
