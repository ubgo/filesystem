# Keys

A key names one object on a disk: `avatars/u1.png`, `tenants/acme/invoices/2026-10.pdf`. This page explains the rules every key passes before a driver sees it, why each rule exists, how directories work, and what the local driver adds. Anyone who builds keys from user input, or who moves data between backends, needs it.

The snippets come from `examples/concepts-keys`. Run it with `go run ./concepts-keys` from the `examples` directory.

## Step 1 — What CleanKey does

Every `Disk` method runs its key through `filesystem.CleanKey` before anything else. It normalises harmless variations and refuses dangerous ones:

| Input | Result | Rule |
|---|---|---|
| `/avatars//u1.png` | `avatars/u1.png` | leading slashes are dropped and runs of slashes collapse: keys are relative |
| `docs\2026\report.pdf` | `docs/2026/report.pdf` | backslashes become slashes, because Windows clients send them and a key must mean the same object on every backend |
| `./a/./b.txt` | `a/b.txt` | `.` segments are removed |
| `photos/` | `photos` | a trailing slash is dropped |
| `a/../b.txt` | `CodeInvalidKey` | any `..` segment is refused, never resolved: `a/../b.txt` is an error, not silently `b.txt` |
| `../../etc/passwd` | `CodeInvalidKey` | the same rule is what makes path traversal impossible |
| `""` | `CodeInvalidKey` | an empty key names nothing |
| `bad\x00name.txt` | `CodeInvalidKey` | control characters, NUL included, are refused |
| 1,025 bytes | `CodeInvalidKey` | at most `MaxKeyLength` (1,024) bytes, S3's limit, the strictest of the common stores |
| invalid UTF-8 | `CodeInvalidKey` | keys are text on every backend |

<!-- ds:block id=rawkeys-8a3udqey -->
**rawKeys** · [`examples/concepts-keys/main.go:30-40`](../../examples/concepts-keys/main.go#L30-L40)

```go
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
```
<!-- /ds:block hash=127fdb -->

<!-- ds:block id=cleankeys-je994xw5 -->
**cleanKeys** · [`examples/concepts-keys/main.go:81-94`](../../examples/concepts-keys/main.go#L81-L94)

```go
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
```
<!-- /ds:block hash=32f632 -->

Why refuse `..` instead of resolving it: resolving would make `uploads/../config.yml` quietly mean `config.yml`, and every check the application made on the raw string (does it start with the user's folder?) would be checking a different key from the one written. An error is the only answer that cannot be misread. Why 1,024 bytes everywhere: a key that is valid on local disk but too long for S3 would break the day you switch backends, so the strictest limit applies to every disk.

Check: the program prints the table above, line by line: `CleanKey("a/../b.txt") -> invalid_key`, `CleanKey(<1025 bytes>) -> invalid_key`.

## Step 2 — Directories

Directories do not exist as objects. A "directory" is the set of keys sharing a prefix, which is how every object store behaves: there is nothing to create before writing `docs/2026/report.pdf`, and nothing left to delete after its last object is gone. `List` and `DeletePrefix` take a directory, cleaned by `CleanDir`, which follows the same rules except that `""`, `/` and `.` are valid and mean the root of the disk. The result never has a trailing slash.

<!-- ds:block id=cleandirs-vhta3vw7 -->
**cleanDirs** · [`examples/concepts-keys/main.go:99-104`](../../examples/concepts-keys/main.go#L99-L104)

```go
func cleanDirs(out io.Writer) {
	for _, raw := range []string{"", "/", ".", "/avatars/"} {
		dir, err := filesystem.CleanDir(raw)
		fmt.Fprintf(out, "CleanDir(%q) -> %q %v\n", raw, dir, err)
	}
}
```
<!-- /ds:block hash=dbd904 -->

A directory matches whole segments only: listing `photo` returns `photo/1.jpg` but not `photos/2.jpg` or `photo.jpg`. The conformance suite checks this on every driver.

`disk.DirExists(ctx, dir)` reports whether a directory exists, and `disk.MakeDir(ctx, dir)` creates one (with its parents, succeeding when it exists). What they mean depends on the backend, and `Capabilities().Dirs` says which:

- **With `Dirs`** (local disk, memory, SFTP, FTP, WebDAV): the backend has real directories. `MakeDir` creates an empty one, `DirExists` reports it, and `MakeDir` over an existing object is `exists`. `List` still shows keys only, never an empty directory. The local, SFTP and FTP drivers remove a directory again when the last key under it is deleted, as they always prune empty directories; the memory and WebDAV drivers keep it until `DeletePrefix` removes it or a parent. `DeletePrefix` removes the directory itself on both.
- **Without `Dirs`** (S3, GCS, Azure and every other object store): `MakeDir` checks the directory and does nothing else, and `DirExists` lists it with a limit of one entry, so a directory exists exactly while a key is under it. No marker object is written: a zero-byte `dir/` marker object would be seen by `List`, `DeletePrefix`, quotas, mirrors and the records layer, and taken for a file by other tools reading the bucket. Code that needs empty directories checks `Capabilities().Dirs`.

`MakeDir` runs as one `make_dir` operation either way, so a read-only disk refuses it on every backend. `DirExists("")` is always true; an object's own key is never a directory.

On a local disk, which has `Dirs`, the program creates an empty directory, finds it, and shows that `List` still returns no keys; `MakeDir` over an existing file is `exists`:

<!-- ds:block id=directories-kvdjdjfk -->
**directories** · [`examples/concepts-keys/main.go:174-207`](../../examples/concepts-keys/main.go#L174-L207)

```go
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
```
<!-- /ds:block hash=c4b79b -->

Check: `dirs capability: true, inbox exists: true, keys listed: 0` and `MakeDir over a file: exists`.

Check: `CleanDir("/") -> "" <nil>` and `CleanDir("/avatars/") -> "avatars" <nil>`.

## Step 3 — The Disk cleans first

Because the Disk cleans every key, a messy key reaches the same object as its clean form, and a traversal attempt fails before any driver code runs. The error names the operation, the disk and the key as the caller passed it, so logs show what was attempted.

<!-- ds:block id=diskcleansfirst-khzhwncd -->
**diskCleansFirst** · [`examples/concepts-keys/main.go:110-126`](../../examples/concepts-keys/main.go#L110-L126)

```go
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
```
<!-- /ds:block hash=08ca9c -->

Check: `written as a messy key, found as avatars/u1.png: true` and `read with ..: filesystem: read media:avatars/../../secrets.txt: invalid_key: key contains a ".." segment`.

Every method cleans: the source and destination of `Copy` and `Move`, each key of `DeleteKeys` (an invalid key fails alone, the rest are deleted), the directory of `List` and `DeletePrefix`, and the key of a signed or public URL.

## Step 4 — The local driver's own rules

The local driver adds a second line of defence and two rules of its own:

- Every operation goes through an `os.Root` opened on the disk's directory, so no key, symlink or `..` can reach outside it even if the Disk's checks were bypassed.
- Keys under `.filesystem/` (`local.StagingDir`) are reserved: the driver stages atomic writes and multipart parts there. Such a key is `CodeInvalidKey`, and listings skip the directory.
- A key cannot be both a file and a directory, because a real filesystem cannot hold `report` and `report/2026.pdf` at once. The second write fails with `CodeInvalidArgument`. Object stores, and the memory driver, allow both.

<!-- ds:block id=locallimits-mf4jxpp8 -->
**localLimits** · [`examples/concepts-keys/main.go:132-161`](../../examples/concepts-keys/main.go#L132-L161)

```go
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
```
<!-- /ds:block hash=c39352 -->

Check: `local .filesystem/tmp/x: invalid_key`, `local report then report/2026.pdf: invalid_argument`, `memory report then report/2026.pdf: ok`.

A portable application never relies on a key being both. The simplest rule that guarantees it: objects have an extension, directories never do.

### Letter case, reserved names and portable keys

Keys are case-sensitive: `A.txt` and `a.txt` are two objects on every backend, except where the filesystem underneath folds case. The local driver probes its directory when it opens and reports `Capabilities.CaseInsensitive` when it does (macOS and Windows by default), so code that must keep both can refuse such pairs itself; the conformance suite's `LetterCase` case holds each driver to what it declares.

So that a directory written on Linux can be copied to Windows, the local driver also refuses, on every system, the names Windows reserves: a segment that is `CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9` or `LPT1`–`LPT9` before its first dot, in any case (`con.txt`, `docs/Nul.tar.gz`), or that ends in a dot or a space, with `invalid_key`. Only creating such a name is refused (a write, an append, a move's destination, a multipart upload): a file that already has one, copied in from elsewhere, can still be read, renamed away and deleted. `local.AllowReservedNames()` lifts the refusal where portability does not matter (it has no effect on Windows).

<!-- ds:block id=reservednames-zu8ydtag -->
**reservedNames** · [`examples/concepts-keys/main.go:213-241`](../../examples/concepts-keys/main.go#L213-L241)

```go
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
```
<!-- /ds:block hash=a06e7f -->

Check: `reserved allowed=false docs/con.txt: invalid_key` and `reserved allowed=false notes./a.txt: invalid_key`, then `ok` for both once the option is set. Set the option only for a disk whose files never leave a Unix machine: the refusal costs nothing, and a backup restored to Windows later is the case it exists for.

### Unicode spellings

The same visible name can arrive as two byte sequences: macOS sends `café` decomposed (`e` plus a combining accent, NFD), most other systems composed (NFC). They are two keys unless the disk normalises them. `filesystem.WithKeyNormalizer(fn)` passes every key and directory through `fn` before cleaning, so the traversal check runs on the result; `contrib/keynorm.NFC` is the stock normaliser (its own module, since it needs `golang.org/x/text`). `disk.CleanKey(key)` returns a key as that disk addresses it, for comparing keys you build with keys it lists. Set the normaliser on an empty disk: every key given to it is normalised, so objects already stored in another spelling cannot be reached through it, not even to rename them. To migrate, rename the old keys first through a second `Disk` over the same driver without the normaliser. A scope's or a DSN's `prefix` is normalised the same way.

The program writes `café` decomposed and asks for it composed, first on a disk without a normaliser and then on one with `keynorm.NFC`:

<!-- ds:block id=unicodespellings-bxzjhnys -->
**unicodeSpellings** · [`examples/concepts-keys/main.go:255-276`](../../examples/concepts-keys/main.go#L255-L276)

```go
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
```
<!-- /ds:block hash=604b3f -->

Check: `NFC normaliser false: composed spelling found: false`, then `NFC normaliser true: composed spelling found: true`. NFC is the right form for keys: it is what browsers, Windows and Linux send, and it is shorter. Add `github.com/ubgo/filesystem/contrib/keynorm` to your `go.mod` to use it.

### Characters a backend cannot store

Some backends refuse characters a key may hold: SFTP servers on Windows refuse `: * ? < > | "`, Azure a name ending in a dot. A driver for such a backend holds a `filesystem.KeyEncoder` and encodes every key it sends and decodes every name it lists, so applications see the same keys on every backend. `filesystem.KeyEscaper{Chars, SegmentEnd}` is the stock one: it percent-escapes (`%3A`) each character in `Chars` anywhere, each in `SegmentEnd` at the end of a segment, and `%` itself. Decoding accepts only names it would have written (no `%41` for `A`, no lower-case hex, no `%2F` for `/`), so two stored names can never decode to one key. The conformance case `KeyCharactersRoundTrip` writes and lists a key holding all of them.

Applications never call the escaper themselves: the driver does, on every key it sends and every name it lists. This is what that looks like, with the set a driver for a Windows SFTP server would use:

<!-- ds:block id=escapeunstorable-b2js8eq2 -->
**escapeUnstorable** · [`examples/concepts-keys/main.go:283-295`](../../examples/concepts-keys/main.go#L283-L295)

```go
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
```
<!-- /ds:block hash=cfde82 -->

Check: `escaped "reports/Q3: draft?.txt" -> "reports/Q3%3A draft%3F.txt" -> "reports/Q3: draft?.txt"`, and `decode a name never written: invalid_key` for `%41`, which the escaper would have written as `A`. A name refused on decoding (a file someone created on the server by hand) is reported by `List` as an error rather than skipped or guessed. If you write a driver for such a backend, hold a `KeyEncoder` in it and run the conformance suite: see [Write a driver](../extending/write-a-driver.md).

## Step 5 — Keys in URLs

`filesystem.EscapeKey` percent-escapes each segment of a clean key and keeps the slashes. Public URLs (`WithPublicURL`) and signed URLs use it, so a key with spaces, `#`, `?` or non-ASCII characters produces a working link.

<!-- ds:block id=escapeforurl-aj8r565w -->
**escapeForURL** · [`examples/concepts-keys/main.go:166-168`](../../examples/concepts-keys/main.go#L166-L168)

```go
func escapeForURL(out io.Writer) {
	fmt.Fprintf(out, "EscapeKey -> %s\n", filesystem.EscapeKey("naïve café/résumé #1.pdf"))
}
```
<!-- /ds:block hash=9e61a8 -->

Check: `EscapeKey -> na%C3%AFve%20caf%C3%A9/r%C3%A9sum%C3%A9%20%231.pdf`.

## Helpers for wrapper code

Code that builds keys or wraps drivers uses three helpers that do not clean their input: `JoinKey(parts...)` joins already-clean parts and skips empty ones (`JoinKey("tenants", "acme", "logo.png")`), `KeyInDir(key, dir)` reports whether a key lies under a clean directory (a key equal to the directory is not inside it), and `RelativeKey(key, dir)` strips the directory prefix. Scoped disks and the tenant guard in [Multi-tenancy](multi-tenancy.md) are built on them.

## Choosing keys

The library cleans keys; it does not choose them. Rules that hold up:

- Never put a client's file name in a key. Two users upload `photo.jpg`, and names carry path tricks. `upload.RandomKey(dir)` names files with 128 random bits and the extension of the detected type, and the client's name goes to a database column for display. See [Uploads](uploads.md).
- Put the tenant or owner first (`tenants/acme/…`) so a whole tenant can be scoped, listed and deleted by prefix. See [Multi-tenancy](multi-tenancy.md).
- Content-addressed keys (a hash in the name) never change content, so they can be cached forever with `Cache-Control: public, max-age=31536000, immutable`.
- Keep keys lowercase when they may land on a case-insensitive filesystem (macOS, Windows): `a.PNG` and `a.png` are one file there and two objects on S3. `upload.RandomKey` already uses lowercase.

## Pitfalls

- Checking a raw key with `strings.HasPrefix(key, "tenants/acme/")` before the Disk cleans it: `tenants/acme/../globex/x` passes the prefix check. The Disk refuses that key, but code that builds a path for something else from the raw string does not get that protection. Clean first with `CleanKey`, then check.
- Expecting `CodeNotFound` from `List` on an empty directory: it returns an empty page, because a directory with no keys is not an error.
- Using `DeletePrefix("")` to empty a disk: refused with `CodeInvalidArgument`. List and delete deliberately if you really mean it.

## Next

- [Security](security.md): every default that protects keys, uploads and links.
- [Multi-tenancy](multi-tenancy.md): prefixes, scoped disks and guards.
- [Disks and drivers](disks-and-drivers.md): the methods that take keys.
