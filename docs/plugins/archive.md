# Accepting archives safely

You will have zip, tar, tar.gz and gzip uploads checked before they are reported stored, and extracted onto a disk without a zip bomb filling it or a zip-slip entry writing outside the folder you chose. The package is `contrib/archive`: a walker on the standard library's `archive/zip`, `archive/tar` and `compress/gzip`, an upload inspector built on it, and `Extract`.

## Before you start

- `go get github.com/ubgo/filesystem/contrib/archive`. It is its own module, needs no program and no cgo, and imports nothing but the standard library and the core (`deps_test.go` enforces it).
- The example is [`examples/security-archive`](../../examples/security-archive/main.go): `go run ./security-archive` from `examples` uploads a clean zip, a bomb, a zip-slip archive and an archive hiding a program, and prints what happened to each.

## What the walk refuses

Every function walks the archive the same way, and every refusal is `CodeRejected` wrapping a sentinel, so `errors.Is` tells them apart:

| Input | Error |
|---|---|
| More entries than `MaxEntries` (10,000), counted at every depth | `ErrLimit` |
| More decompressed bytes than `MaxTotal` (1 GiB), nested archives included | `ErrLimit` |
| More than `MaxRatio` (100) decompressed bytes per byte of the upload, once past `RatioGrace` (1 MiB) | `ErrLimit` |
| An archive inside an archive inside an archive, beyond `MaxDepth` (2 levels; a gzip file is a level) | `ErrLimit` |
| A nested archive larger than `MaxNested` (64 MiB), which the walk holds in memory | `ErrLimit` |
| A name with a `..` segment, an absolute name (`/etc/x`, `C:\x`, `\x`), a control character or invalid UTF-8 | `ErrUnsafeEntry` |
| A symlink, a hard link, a device, a FIFO, a socket, any tar type other than file and directory | `ErrUnsafeEntry` |
| Two entries with the same name after cleaning (`a.txt` and `./a.txt`), or names that differ only in case (`A.txt` and `a.txt`, one file on a case-insensitive filesystem), the trick that hides a second file behind a first | `ErrUnsafeEntry` |
| A file whose last bytes hold a zip directory it does not start with (a zip hidden behind other content inside the archive) | `ErrUnsafeEntry` |
| Input that is not a zip, tar, tar.gz or gzip file, or a corrupt one | `ErrFormat` |

A zero `Limits` field means its default, so `archive.Limits{}` is the safe value; there is no "unlimited", and an application that takes backups sets larger numbers. The limits are enforced while the archive decompresses, never after: zip declared sizes are checked before anything is read (the reader then holds each entry to its declared size), tar headers before each entry, and every decompressed byte, tar headers and padding included, is counted as it is produced, so the walk stops at the first byte past a limit. Every name goes through `filesystem.CleanKey`, the same check every key on a Disk gets.

Formats are recognised by their content, never their name: gzip and tar by their magic at the start, and zip by its end-of-central-directory record wherever it sits, the way `archive/zip` finds it. A polyglot, a zip behind a PDF header that one program opens as a PDF and another as a zip, is walked like any zip. A gzip file holding something other than a tar is walked as an archive of one entry, so a program or a bomb compressed alone is checked too. PAX global headers (which `git archive` writes) are metadata and are skipped; every other special tar type is refused. Nested archives are recognised the same way. Each is read into memory (bounded by the same limits), walked one level deeper, and reported with `Entry.In` naming it.

## Step 1 — Inspect uploads

`archive.Inspect(limits, check)` is an `upload.Inspector`: it walks the stored upload in ranged reads (through `archive.ReaderAt`, so the archive is never held in memory) and runs `check` on every entry, nested ones included. `archive.DenyExecutables` is a ready check that refuses programs by content (ELF, PE, Mach-O, `#!`) and by extension (`archive.DefaultExecutableExtensions()`: `.exe`, `.scr`, `.pif`, `.msi`, `.bat`, `.ps1`, `.vbs`, `.hta`, `.lnk`, `.reg`, `.iso`, `.jar`, `.apk`, …), after removing the trailing dots and spaces Windows strips (`setup.exe. `). To refuse more, pass your list to `archive.DenyExecutablesWith(append(archive.DefaultExecutableExtensions(), ".py"))`. A refused upload is deleted by the upload, on every upload path:

<!-- ds:block id=newpolicy-3jkc4cve -->
**newPolicy** · [`examples/security-archive/main.go:44-50`](../../examples/security-archive/main.go#L44-L50)

```go
func newPolicy() upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadSize,
		Allowed: []string{sniff.Zip},
		Inspect: archive.Inspect(limits, archive.DenyExecutables),
	}
}
```
<!-- /ds:block hash=efd50c -->

An upload that is not an archive passes `Inspect`, so the inspector can sit in a policy that also accepts images; a policy that must receive archives says so in `Allowed`, as above.

## Step 2 — Extract

`archive.Extract(ctx, disk, prefix, r, size, archive.ExtractOptions{Limits: limits})` stores each file at `filesystem.JoinKey(prefix, Entry.Name)` while it decompresses. Directories create nothing, and a nested archive is stored as the file it is, after its own entries passed the limits. A key that already exists is refused with `CodeExists`, so an archive can never replace a file the application stored under the prefix by naming it: atomically through `IfNotExists` on disks with `ConditionalWrite`, by a `Stat` before each write on others. `ExtractOptions.Overwrite` lifts that. When the walk fails part-way, the objects Extract created are deleted before the error returns, so a bomb or an unsafe entry found late leaves nothing new behind; an object it overwrote under `Overwrite` keeps the archive's bytes. `archive.List` is the same walk without storing, for a preview of an archive's contents.

<!-- ds:block id=importzip-9tm4vbxg -->
**importZip** · [`examples/security-archive/main.go:57-68`](../../examples/security-archive/main.go#L57-L68)

```go
func importZip(ctx context.Context, disk *filesystem.Disk, body io.Reader, name string) ([]archive.Entry, error) {
	res, err := upload.Receive(ctx, disk, body, name, newPolicy())
	if err != nil {
		return nil, err
	}
	defer func() { _ = disk.Delete(context.WithoutCancel(ctx), res.Key) }()
	src, err := archive.OpenReaderAt(ctx, disk, res.Key)
	if err != nil {
		return nil, err
	}
	return archive.Extract(ctx, disk, importPrefix, src, src.Size(), archive.ExtractOptions{Limits: limits})
}
```
<!-- /ds:block hash=e259cd -->

Output of `go run ./security-archive`:

```text
documents.zip  stored users/42/imported/contracts/acme.txt (6 bytes)
documents.zip  stored users/42/imported/notes.md (7 bytes)
bomb.zip       refused: filesystem: rejected: archive: limit exceeded: decompresses to more than 100 times its size
slip.zip       refused: filesystem: rejected: archive: unsafe entry: name "../../etc/cron.d/job": filesystem: invalid_key: key contains a ".." segment
invoice.zip    refused: filesystem: rejected: archive entry "invoice.pdf.exe" is a program
preview        contracts/acme.txt (6 bytes)
preview        notes.md (7 bytes)
on the disk: [users/42/imported/contracts/acme.txt users/42/imported/notes.md]
```

## Step 3 — Preview before extracting

An import screen often shows what an archive holds and lets the user confirm, or pick entries, before anything is written. Store the upload under the same policy, then `archive.List` it: the same walk and limits as `Extract`, reading only the central directory and storing nothing:

<!-- ds:block id=preview-r3w8k6tn -->
**preview** · [`examples/security-archive/main.go:77-92`](../../examples/security-archive/main.go#L77-L92)

```go
func preview(ctx context.Context, disk *filesystem.Disk, body io.Reader, name string) (string, []archive.Entry, error) {
	res, err := upload.Receive(ctx, disk, body, name, newPolicy())
	if err != nil {
		return "", nil, err
	}
	src, err := archive.OpenReaderAt(ctx, disk, res.Key)
	if err != nil {
		return "", nil, err
	}
	entries, err := archive.List(ctx, src, src.Size(), limits)
	if err != nil {
		_ = disk.Delete(context.WithoutCancel(ctx), res.Key)
		return "", nil, err
	}
	return res.Key, entries, nil
}
```
<!-- /ds:block hash=68adf8 -->

Keep the returned key with the pending import and pass it to `Extract` when the user confirms; delete it if they cancel, and sweep stored zips nobody confirmed (`upload.Sweep` on their prefix). A bomb is refused at the upload, before `List` runs, and leaves nothing on the disk (the test `TestPreviewRefusesBomb` checks it). Check: the example previews `documents.zip` and prints `preview        contracts/acme.txt (6 bytes)` and `preview        notes.md (7 bytes)`.

## Your own handling of each entry

`Extract` writes every file to one disk under one prefix. To do something else per entry (a records row with its quota and scan, an `upload.Policy` per file), walk the archive yourself; every limit and name rule still runs inside the walk:

```go
err := archive.Walk(ctx, r, size, archive.Limits{}, func(ctx context.Context, e archive.Entry, content io.Reader) error {
	if e.IsDir || e.Depth > 1 {
		return nil
	}
	_, err := store.Upload(ctx, scope, "media", content, e.Name, policy)
	return err
})
```

The walk may already have handed you earlier entries when it fails on a later one (a bomb found late), so undo what you stored on error, as `Extract` does. To drop archive debris while keeping `Extract`, pass `ExtractOptions.Skip`:

```go
stored, err := archive.Extract(ctx, disk, "imports/42/", r, size, archive.ExtractOptions{
	Skip: func(e archive.Entry) bool {
		return strings.HasPrefix(e.Name, "__MACOSX/") || path.Base(e.Name) == ".DS_Store"
	},
})
```

A skipped entry is still read through the limits, so `Skip` can only store less.

## Try it

```sh
cd examples
go run ./security-archive
go test ./security-archive
```

## Pitfalls

- `ReaderAt` fetches 1 MiB per ranged read and keeps the last block. Zip needs random access (its directory is at the end), so an archive on an object store costs a few requests per mebibyte; a tar.gz streams front to back.
- `ModTime` comes from the archive and is untrusted; `Extract` does not apply it.
- Extract writes with the Disk's defaults. To store extracted files with a visibility, a content type or metadata, walk with `List` first and write each file yourself.
