# Keys: cleaning, refusals and URL escaping

A key names one object on a disk, and every key passes the same rules before any driver sees it. This program runs `filesystem.CleanKey` over inputs from tidy to hostile, shows that `CleanDir` lets a directory argument mean the whole disk, proves that a `Disk` cleans keys itself (a messy key reaches the same object, a `..` key is refused with the operation, disk and key named), shows the two rules the local driver adds (its reserved staging directory, and a key cannot be both a file and a directory), escapes a key for a URL with `EscapeKey`, creates and checks a real directory, refuses Windows-reserved names, folds Unicode spellings with `contrib/keynorm`, and escapes characters a backend cannot store with `KeyEscaper`. It answers the question every app that builds keys from user input has: what reaches the backend, and what is refused rather than repaired. It uses the core `filesystem` package, `memory`, `local` and `contrib/keynorm`. The guide [Keys](../../docs/concepts/keys.md) explains each rule and why it exists.

## Run it

```sh
cd examples
go run ./concepts-keys
go test ./concepts-keys
```

It needs no network or environment variables. The local-driver step writes into a temporary directory, which the program removes before it exits.

## What it does

```sh
go run ./concepts-keys
```

```text
CleanKey("/avatars//u1.png") -> "avatars/u1.png"
CleanKey("docs\\2026\\report.pdf") -> "docs/2026/report.pdf"
CleanKey("./a/./b.txt") -> "a/b.txt"
CleanKey("photos/") -> "photos"
CleanKey("a/../b.txt") -> invalid_key
CleanKey("../../etc/passwd") -> invalid_key
CleanKey("") -> invalid_key
CleanKey("bad\x00name.txt") -> invalid_key
CleanKey(<1025 bytes>) -> invalid_key
CleanDir("") -> "" <nil>
CleanDir("/") -> "" <nil>
CleanDir(".") -> "" <nil>
CleanDir("/avatars/") -> "avatars" <nil>
written as a messy key, found as avatars/u1.png: true
read with ..: filesystem: read media:avatars/../../secrets.txt: invalid_key: key contains a ".." segment
local .filesystem/tmp/x: invalid_key
local report then report/2026.pdf: invalid_argument
memory report then report/2026.pdf: ok
EscapeKey -> na%C3%AFve%20caf%C3%A9/r%C3%A9sum%C3%A9%20%231.pdf
dirs capability: true, inbox exists: true, keys listed: 0
MakeDir over a file: exists
reserved allowed=false docs/con.txt: invalid_key
reserved allowed=false notes./a.txt: invalid_key
reserved allowed=true docs/con.txt: ok
reserved allowed=true notes./a.txt: ok
NFC normaliser false: composed spelling found: false
NFC normaliser true: composed spelling found: true
escaped "reports/Q3: draft?.txt" -> "reports/Q3%3A draft%3F.txt" -> "reports/Q3: draft?.txt"
decode a name never written: invalid_key
```

1. Leading and doubled slashes, backslashes, `.` segments and a trailing slash are normalised. A `..` segment, an empty key, a control character and a key longer than `MaxKeyLength` (1024 bytes) are refused with `invalid_key`: the library never guesses what a hostile key meant.
2. `CleanDir` accepts `""`, `/` and `.` as the root, which `CleanKey` would refuse as empty.
3. A disk cleans every key first: a write to `\avatars\\u1.png` is found at `avatars/u1.png`, and a read of `avatars/../../secrets.txt` fails before the driver runs.
4. The local driver reserves its staging directory `.filesystem/` and refuses a key under a path that is already a file; the memory driver, like object stores, allows both `report` and `report/2026.pdf`.
5. `EscapeKey` percent-escapes each segment and keeps the slashes, which is how public and signed URLs carry keys.
6. A local disk has real directories: `MakeDir` creates an empty one that `DirExists` reports and `List` does not show, and `MakeDir` over a file is `exists`.
7. The local driver refuses names Windows reserves (`con.txt`, a segment ending in a dot) on every system; `local.AllowReservedNames()` lifts the refusal.
8. Without a normaliser, `café` written decomposed (as macOS sends it) is not found spelled composed; with `keynorm.NFC` it is one key.
9. `KeyEscaper` percent-escapes characters a backend cannot store and decodes them back; a name it would never have written is refused.

## The code

The inputs, from tidy to hostile, including one key a byte past the length limit.

<!-- ds:block id=rawkeys-8a3udqey -->
**rawKeys** · [`examples/concepts-keys/main.go:30-40`](main.go#L30-L40)

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

`CleanKey` over each input: separators and dots are normalised, everything ambiguous is refused with `CodeInvalidKey`.

<!-- ds:block id=cleankeys-je994xw5 -->
**cleanKeys** · [`examples/concepts-keys/main.go:81-94`](main.go#L81-L94)

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

`CleanDir` for directory arguments, where the root is a valid answer.

<!-- ds:block id=cleandirs-vhta3vw7 -->
**cleanDirs** · [`examples/concepts-keys/main.go:99-104`](main.go#L99-L104)

```go
func cleanDirs(out io.Writer) {
	for _, raw := range []string{"", "/", ".", "/avatars/"} {
		dir, err := filesystem.CleanDir(raw)
		fmt.Fprintf(out, "CleanDir(%q) -> %q %v\n", raw, dir, err)
	}
}
```
<!-- /ds:block hash=dbd904 -->

A `Disk` cleans before its driver: the same object behind a messy key, and a traversal attempt refused with a descriptive error.

<!-- ds:block id=diskcleansfirst-khzhwncd -->
**diskCleansFirst** · [`examples/concepts-keys/main.go:110-126`](main.go#L110-L126)

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

The local driver's own rules, compared with the memory driver.

<!-- ds:block id=locallimits-mf4jxpp8 -->
**localLimits** · [`examples/concepts-keys/main.go:132-161`](main.go#L132-L161)

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

Escaping a key with non-ASCII characters, a space and `#` for a URL.

<!-- ds:block id=escapeforurl-aj8r565w -->
**escapeForURL** · [`examples/concepts-keys/main.go:166-168`](main.go#L166-L168)

```go
func escapeForURL(out io.Writer) {
	fmt.Fprintf(out, "EscapeKey -> %s\n", filesystem.EscapeKey("naïve café/résumé #1.pdf"))
}
```
<!-- /ds:block hash=9e61a8 -->

Real directories on a disk that has them.

<!-- ds:block id=directories-kvdjdjfk -->
**directories** · [`examples/concepts-keys/main.go:174-207`](main.go#L174-L207)

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

Names Windows reserves, refused by default on every system.

<!-- ds:block id=reservednames-zu8ydtag -->
**reservedNames** · [`examples/concepts-keys/main.go:213-241`](main.go#L213-L241)

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

Two Unicode spellings of one name, folded by a key normaliser.

<!-- ds:block id=unicodespellings-bxzjhnys -->
**unicodeSpellings** · [`examples/concepts-keys/main.go:255-276`](main.go#L255-L276)

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

Escaping characters a backend cannot store.

<!-- ds:block id=escapeunstorable-b2js8eq2 -->
**escapeUnstorable** · [`examples/concepts-keys/main.go:283-295`](main.go#L283-L295)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: CleanKey, CleanDir, a disk cleaning first, the local driver's limits, EscapeKey |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Keys](../../docs/concepts/keys.md), including how to choose keys and the pitfalls of checking a raw key.
- Related examples: [concepts-security](../concepts-security) for every default that protects keys and uploads, [concepts-tenancy](../concepts-tenancy) for prefixes and scoped disks, [concepts-disks](../concepts-disks) for the methods that take keys.
