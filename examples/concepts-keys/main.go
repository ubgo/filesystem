// Command concepts-keys shows how the library treats keys: what CleanKey
// normalises, what it refuses and why, how a Disk applies it before any
// driver runs, the local driver's reserved directory and its
// file-or-directory limit, how keys are escaped for URLs, real
// directories, Windows-reserved names, Unicode normalisation and escaping
// characters a backend cannot store.
//
//	go run ./concepts-keys          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/keynorm"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// overlongKey is one byte past MaxKeyLength, to show the length limit.
var overlongKey = strings.Repeat("k", filesystem.MaxKeyLength+1)

// rawKeys are inputs a caller might pass, from tidy to hostile.
// ds:def id=rawkeys-8a3udqey
var rawKeys = []string{
	"/avatars//u1.png",
	`docs\2026\report.pdf`,
	"./a/./b.txt",
	"photos/",
	"a/../b.txt",
	"../../etc/passwd",
	"",
	"bad\x00name.txt",
	overlongKey,
}

func main() {
	dir, err := os.MkdirTemp("", "concepts-keys-")
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
	cleanKeys(out)
	cleanDirs(out)
	if err := diskCleansFirst(ctx, out); err != nil {
		return err
	}
	if err := localLimits(ctx, out, dir); err != nil {
		return err
	}
	escapeForURL(out)
	for _, step := range []func(context.Context, io.Writer, string) error{directories, reservedNames} {
		if err := step(ctx, out, dir); err != nil {
			return err
		}
	}
	if err := unicodeSpellings(ctx, out); err != nil {
		return err
	}
	return escapeUnstorable(out)
}

// cleanKeys runs CleanKey over rawKeys: separators and dots are
// normalised, while "..", control characters, empty keys and overlong
// keys are refused with CodeInvalidKey rather than repaired.
// ds:def id=cleankeys-je994xw5
func cleanKeys(out io.Writer) {
	for _, raw := range rawKeys {
		key, err := filesystem.CleanKey(raw)
		label := fmt.Sprintf("%q", raw)
		if len(raw) > filesystem.MaxKeyLength {
			label = fmt.Sprintf("<%d bytes>", len(raw))
		}
		if err != nil {
			fmt.Fprintf(out, "CleanKey(%s) -> %s\n", label, filesystem.CodeOf(err))
			continue
		}
		fmt.Fprintf(out, "CleanKey(%s) -> %q\n", label, key)
	}
}

// cleanDirs shows that a directory argument may be the root: "", "/" and
// "." all mean the whole disk, which CleanKey would refuse as empty.
// ds:def id=cleandirs-vhta3vw7
func cleanDirs(out io.Writer) {
	for _, raw := range []string{"", "/", ".", "/avatars/"} {
		dir, err := filesystem.CleanDir(raw)
		fmt.Fprintf(out, "CleanDir(%q) -> %q %v\n", raw, dir, err)
	}
}

// diskCleansFirst shows that every Disk method cleans its key before the
// driver sees it: a messy key reaches the same object, and a traversal
// attempt fails with the operation, disk and key named in the error.
// ds:def id=diskcleansfirst-khzhwncd
func diskCleansFirst(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, `\avatars\\u1.png`, []byte("png"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "written as a messy key, found as avatars/u1.png: %t\n", ok)
	_, err = disk.Read(ctx, "avatars/../../secrets.txt")
	fmt.Fprintf(out, "read with ..: %v\n", err)
	return nil
}

// localLimits shows two rules of the local driver: keys under its staging
// directory are reserved, and a key cannot be both a file and a directory,
// which object stores allow.
// ds:def id=locallimits-mf4jxpp8
func localLimits(ctx context.Context, out io.Writer, dir string) error {
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
	_, err = disk.WriteBytes(ctx, local.StagingDir+"/tmp/x", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "local %s/tmp/x: %s\n", local.StagingDir, filesystem.CodeOf(err))
	if _, err := disk.WriteBytes(ctx, "report", []byte("file"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "report/2026.pdf", []byte("nested"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "local report then report/2026.pdf: %s\n", filesystem.CodeOf(err))
	mem, err := filesystem.NewDisk("cache", memory.New())
	if err != nil {
		return err
	}
	for _, k := range []string{"report", "report/2026.pdf"} {
		if _, err := mem.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "memory report then report/2026.pdf: ok")
	return nil
}

// escapeForURL shows EscapeKey, which public and signed URLs use:
// each segment is percent-escaped and the slashes between them are kept.
// ds:def id=escapeforurl-aj8r565w
func escapeForURL(out io.Writer) {
	fmt.Fprintf(out, "EscapeKey -> %s\n", filesystem.EscapeKey("naïve café/résumé #1.pdf"))
}

// directories creates an empty directory on a local disk, which has real
// directories (Capabilities().Dirs), asks whether it exists, and shows that
// List still returns keys only. MakeDir over an existing object is exists.
// ds:def id=directories-kvdjdjfk
func directories(ctx context.Context, out io.Writer, dir string) error {
	root := filepath.Join(dir, "dirs")
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	drv, err := local.New(root)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		_ = drv.Close()
		return err
	}
	defer func() { _ = disk.Close() }()
	if err := disk.MakeDir(ctx, "inbox/2026"); err != nil {
		return err
	}
	ok, err := disk.DirExists(ctx, "inbox")
	if err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dirs capability: %t, inbox exists: %t, keys listed: %d\n",
		disk.Capabilities().Dirs, ok, len(page.Entries))
	if _, err := disk.WriteBytes(ctx, "notes.txt", []byte("n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Fprintf(out, "MakeDir over a file: %s\n", filesystem.CodeOf(disk.MakeDir(ctx, "notes.txt")))
	return nil
}

// reservedNames shows the local driver refusing names Windows reserves, on
// every system, so a directory written on Linux can be copied to Windows;
// local.AllowReservedNames lifts the refusal where that does not matter.
// ds:def id=reservednames-zu8ydtag
func reservedNames(ctx context.Context, out io.Writer, dir string) error {
	for _, allow := range []bool{false, true} {
		root := filepath.Join(dir, fmt.Sprintf("reserved-%t", allow))
		if err := os.Mkdir(root, 0o700); err != nil {
			return err
		}
		var opts []local.Option
		if allow {
			opts = append(opts, local.AllowReservedNames())
		}
		drv, err := local.New(root, opts...)
		if err != nil {
			return err
		}
		disk, err := filesystem.NewDisk("uploads", drv)
		if err != nil {
			_ = drv.Close()
			return err
		}
		for _, key := range []string{"docs/con.txt", "notes./a.txt"} {
			_, err := disk.WriteBytes(ctx, key, []byte("x"), filesystem.WriteOptions{})
			fmt.Fprintf(out, "reserved allowed=%t %s: %s\n", allow, key, codeOrOK(err))
		}
		if err := disk.Close(); err != nil {
			return err
		}
	}
	return nil
}

// codeOrOK names an error by its code, or "ok" when there is none.
func codeOrOK(err error) string {
	if err == nil {
		return "ok"
	}
	return string(filesystem.CodeOf(err))
}

// unicodeSpellings writes "café" spelled decomposed (as macOS sends it)
// and reads it back spelled composed. With keynorm.NFC both spellings are
// one key; without a normaliser they are two.
// ds:def id=unicodespellings-bxzjhnys
func unicodeSpellings(ctx context.Context, out io.Writer) error {
	const decomposed, composed = "cafe\u0301.txt", "caf\u00e9.txt"
	for _, normalise := range []bool{false, true} {
		var opts []filesystem.Option
		if normalise {
			opts = append(opts, filesystem.WithKeyNormalizer(keynorm.NFC))
		}
		disk, err := filesystem.NewDisk("media", memory.New(), opts...)
		if err != nil {
			return err
		}
		if _, err := disk.WriteBytes(ctx, decomposed, []byte("menu"), filesystem.WriteOptions{}); err != nil {
			return err
		}
		ok, err := disk.Exists(ctx, composed)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "NFC normaliser %t: composed spelling found: %t\n", normalise, ok)
	}
	return nil
}

// escapeUnstorable is what a driver for a backend that refuses some
// characters does with every key: KeyEscaper percent-escapes them on the
// way in and DecodeKey restores the key from a listed name. Decoding
// refuses a name the escaper would never have written.
// ds:def id=escapeunstorable-b2js8eq2
func escapeUnstorable(out io.Writer) error {
	esc := filesystem.KeyEscaper{Chars: `:*?<>|"`, SegmentEnd: ". "}
	key := "reports/Q3: draft?.txt"
	name := esc.EncodeKey(key)
	back, err := esc.DecodeKey(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "escaped %q -> %q -> %q\n", key, name, back)
	_, err = esc.DecodeKey("reports/%41.txt")
	fmt.Fprintf(out, "decode a name never written: %s\n", filesystem.CodeOf(err))
	return nil
}
