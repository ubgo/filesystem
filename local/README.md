# local

`github.com/ubgo/filesystem/local` is a `filesystem.Driver` over a directory on local disk: the backend for development, single-server deployments and tests that need real files.

The full guide (deployment notes, backups, serving with a web server) is in [Local disk](../docs/adapters/local.md); the concepts are in [Disks and drivers](../docs/concepts/disks-and-drivers.md).

```sh
STORAGE_MEDIA_DSN=file:///var/app/uploads
STORAGE_MEDIA_DSN=file:uploads?visibility=public&public_url=http://localhost:8080/media
```

## Guarantees

- **Sandboxed.** Every operation goes through an `os.Root`, so no key, symlink or `..` can reach outside the directory even if the Disk's own key checks were bypassed.
- **Atomic writes.** Bytes go to a temporary file in the reserved `.filesystem/` directory, are synced, and are renamed into place, so readers never see a partial file and a failed write leaves nothing behind. A conditional write (`IfNotExists`) hard-links the staged file into place, which fails atomically if the key exists.
- **Durable writes.** By default (`DurabilityFull`) the directories a write changed are synced after the rename too, so a write that returned survives a crash or power cut; `WithDurability` trades that for speed (`DurabilityFile`: the bytes only; `DurabilityNone`: nothing). Windows cannot sync a directory and behaves as `DurabilityFile`.
- **Visibility as file modes.** Public files are world-readable (0644 by default) so a web server serving the directory can read them; private files are owner-only (0600). `Stat` reads visibility back from the world-read bit.
- **Tidy tree.** Deleting the last file of a directory removes the directories left empty, so the tree mirrors the key space.

The reserved directory and the file-or-directory limit, from the keys guide:

<!-- ds:block id=locallimits-mf4jxpp8 -->
**localLimits** · [`examples/concepts-keys/main.go:132-161`](../examples/concepts-keys/main.go#L132-L161)

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

## API

| Name | What it is |
|---|---|
| `New(dir, options...)` | opens the directory as a local disk, creating it if needed; must be closed with `Close` (or the Disk's `Close`) |
| `Driver` | the backend; safe for concurrent use |
| `Option` | configures a `Driver` |
| `AllowReservedNames()` | lifts the default refusal (`CodeInvalidKey`, on every system) of segments Windows reserves: `CON`, `PRN`, `AUX`, `NUL`, `COM1`–`9`, `LPT1`–`9` before the first dot, in any case, and names ending in a dot or space; no effect on Windows |
| `WithPermissions(filePublic, filePrivate, dir)` | the modes for public files, private files and directories; the public mode must have the world-read bit and the private mode must not, or `New` fails with `CodeInvalidArgument` |
| `WithContentType(fn)` | how `Stat` and `List` derive a file's `Content-Type` from its key, since a local file stores none: the default reads the host's MIME table (`mime.TypeByExtension`, so a key can report another type on a Mac, in CI and in production); pass `sniff.TypeByExtension` for a fixed table that answers the same everywhere, or an application's own map, for example `local.WithContentType(func(key string) string { if strings.HasSuffix(key, ".dcm") { return "application/dicom" }; return sniff.TypeByExtension(key) })`; nil keeps the default |
| `WithDurability(level)` | how writes are synced: `DurabilityFull` (default: the file before the rename and the changed directories after), `DurabilityFile` (the file only) or `DurabilityNone`; any other value fails `New` with `CodeInvalidArgument` |
| `Durability`, `DurabilityFull`, `DurabilityFile`, `DurabilityNone`, `DurabilityValues` | the closed set of levels |
| `DefaultFilePublic`, `DefaultFilePrivate`, `DefaultDirMode` | 0644, 0600, 0755 |
| `StagingDir` | `.filesystem`, the reserved directory for staged writes and multipart parts; keys inside it are `CodeInvalidKey` and listings skip it |
| `Scheme` | `"file"`, for `Mux.Register`; `file:///absolute/path`, `file:relative/path`, or on Windows `file:///C:/path` |
| `Opener` | builds a driver from a `file` DSN; accepts no parameters beyond the Mux's own (`prefix`, `visibility`, `public_url`, `readonly`) |
| `Driver.Native()` | the `*os.Root`, for `filesystem.As` |
| `Driver.Close()` | releases the directory handle |

The `Driver` methods (`Stat`, `Read`, `Write`, `Delete`, `List`, `Move`, `SetVisibility`, `DeletePrefix`, `CreateMultipart`, `UploadPart`, `ListParts`, `CompleteMultipart`, `AbortMultipart`, `SweepAbandoned`, `FindAbandoned`, `MakeDir`, `DirExists`, `Capabilities`) implement the `filesystem.Driver` contract and its optional interfaces; applications call them through a `*filesystem.Disk`.

## Capabilities

`Move` (atomic rename), `SetVisibility`, `DeletePrefix` (one `RemoveAll`), `Multipart`, `SweepAbandoned` (removes uploads under `.filesystem/multipart` and staging files under `.filesystem/tmp` idle since the cutoff), `FindAbandoned` (the same scan, removing nothing, with each upload's key), `Dirs` (`MakeDir` creates real directories, `DirExists` reports them; deleting the last key in a directory removes it again), `ConditionalWrite`, `Append` (staged, then appended with `O_APPEND` under the key's lock, so a long append holds up no other key), `SetModTime` (`Chtimes`), `CompareAndSwap` (the ETag is the modification time, change time, size and inode; every change holds its key's lock (one of 64, picked by hashing the key) and, before it is made, waits if it must until the new file's change time passes every earlier version of that key, so no version's ETag ever repeats on any timestamp precision, a stale `IfMatch` always loses, and a change time that will not move fails the change before anything is replaced; changes to other keys never wait on it; the locks coordinate this process only, and the capability is not declared on systems without inodes and change times, such as Windows), and `CaseInsensitive` when the probe at `New` finds the filesystem folds case. Not declared: `Copy` (a local copy would stream bytes exactly as the Disk's fallback does), `DeleteKeys` (the Disk deletes one by one), `Presign` (use a `signedurl.Signer` and `fshttp.Signed`), `PublicURL` (use `WithPublicURL` and `fshttp.Public` or your web server), `Metadata`, `ContentType` and `Headers` (not stored; `Stat` derives the content type from the extension, see `WithContentType`). See [Capabilities](../docs/concepts/capabilities.md).

## Limits

- A key cannot be both a file and a directory: `report` and `report/2026.pdf` cannot both exist (`CodeInvalidArgument`). Object stores allow it; portable applications should not rely on it.
- `List` walks the directory: O(n) in the files under the listed directory per page. It suits development and moderate trees; large trees belong on an object store.
- Visibility is enforced by file modes, which matter only to other local users and to a web server reading the directory. Private files are protected from the internet by not serving them, not by the mode.
- On case-insensitive filesystems (macOS, Windows by default) `a.PNG` and `a.png` are one file.
