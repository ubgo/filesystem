# contrib/archive

`github.com/ubgo/filesystem/contrib/archive` (package `archive`) reads user-uploaded zip, tar, tar.gz and gzip files without trusting them: formats detected from content (a zip behind a PDF header included), limits on entries, total size, compression ratio, nesting depth and nested size enforced while the archive decompresses, every entry name cleaned with `filesystem.CleanKey`, and symlinks, hard links, devices, absolute, traversing and duplicate names (case-insensitively) refused. It imports nothing but the standard library (`archive/zip`, `archive/tar`, `compress/gzip`) and the core (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/archive
```

The guide, with output from a run: [accepting archives safely](../../docs/plugins/archive.md). The policy of [`examples/security-archive`](../../examples/security-archive/main.go):

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

## API

| Name | What it is |
|---|---|
| `Limits{MaxEntries, MaxTotal, MaxRatio, MaxDepth, MaxNested}` | bounds on one archive, nested archives included; a zero field means its default (10,000 entries, 1 GiB, 100 times, 2 levels, 64 MiB) |
| `List(ctx, r io.ReaderAt, size, limits) ([]Entry, error)` | walks the archive and returns every entry without storing anything |
| `Walk(ctx, r io.ReaderAt, size, limits, fn VisitFunc) error`, `VisitFunc` | the walk under `List` and `Extract`: calls `fn(ctx, entry, content)` for every entry, nested ones included, to send entries elsewhere (one records row each, an `upload.Policy` per entry). Every limit and name rule still runs inside the walk; `fn` may have seen earlier entries when it fails, so a storing `fn` cleans up |
| `ExtractOptions.Skip func(Entry) bool` | leaves matching top-level files out of the disk and the result (`__MACOSX/`, `.DS_Store`); a skipped entry is still read through the limits, so `Skip` never lets an unsafe archive through |
| `Extract(ctx, disk, prefix, r, size, ExtractOptions{Limits, Overwrite, Skip}) ([]Entry, error)` | stores each file at `JoinKey(prefix, Name)` while it decompresses; refuses existing keys (`CodeExists`) unless `Overwrite`; on failure deletes what it created |
| `Inspect(limits, check func(Entry) error) upload.Inspector` | walks each stored upload and runs `check` on every entry; uploads that are not archives pass |
| `DenyExecutables(Entry) error`, `DenyExecutablesWith(exts)`, `DefaultExecutableExtensions()` | entry checks refusing programs by content (ELF, PE, Mach-O, `#!`) and by extension, after trailing dots and spaces |
| `OpenReaderAt(ctx, disk, key) (*ReaderAt, error)` | an `io.ReaderAt` over a stored object in 1 MiB ranged reads, for archives already on a disk |
| `Entry{Name, In, Depth, Format, IsDir, Size, ModTime, Executable, Archive}`, `Entry.Path()` | one entry; `In` names the nested archive holding it |
| `ErrLimit`, `ErrUnsafeEntry`, `ErrFormat` | the sentinels every refusal wraps, inside a `CodeRejected` error |
