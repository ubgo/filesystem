# contrib/ocr

`github.com/ubgo/filesystem/contrib/ocr` (package `ocr`) reads the text in images with Tesseract's `tesseract` program: `Tool.Recognize` for one image, a records plugin that stores the text of every PNG, JPEG and GIF file beside it, and a [contrib/pdf](../pdf/README.md) `Recognizer` that reads scanned PDFs page by page (`pdf.WithOCR`). Inputs are bounded in bytes and pixels, only real images reach the program (a text file would be read as a list of paths), languages are checked against a strict pattern, runs are timed out and their text capped. It is its own module because it runs an external program; the core never does, and this module imports nothing but the standard library and the library's own packages (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/ocr
```

Needs `tesseract` on `PATH` (or a configured path) with the language models you read. The guide: [OCR](../../docs/plugins/ocr.md). The example: [`examples/documents/search`](../../examples/documents/search/main.go).

## API

| Name | What it is |
|---|---|
| `Tool` | runs the program; the zero value reads English with the defaults. Fields: `Tesseract`, `Languages`, `Timeout`, `MaxInput`, `MaxPixels`, `MaxText`, `TempDir`, `Env` (the program otherwise sees only `PATH`, `HOME`, `TMPDIR`, `TMP`, `LANG`, `LC_ALL`) |
| `Tool.Recognize(ctx, r) (string, error)` | the text of a PNG, JPEG or GIF image; implements `pdf.Recognizer` |
| `NewPlugin(Recognizer, ...PluginOption) *Plugin`, `WithProcessTimeout(d)`, `WithDeferredProcessing()`, `WithLocation(Location)` | the records plugin (`PluginName` `ocr`); recognition runs in `Plugin.Process` after the upload commits |
| `Recognizer`, `WithContentTypes(types...)` | the engine seam: one method, `Recognize(ctx, image) (string, error)`, the same as `pdf.Recognizer`. `Tool` is one; a cloud OCR service (Textract, Google Vision, Azure Document Intelligence) wrapped in that method is another. `WithContentTypes` replaces `ContentTypes()` with the types that engine reads (TIFF, WebP scans); the engine must bound its own work |
| `Plugin.Resume(ctx, store, olderThan) (int, error)` | re-runs `Process` for images still `processing` past the cutoff (a crash, or `WithDeferredProcessing`); schedule it with `jobs.Func` |
| `Plugin.Process(ctx, store, scope, id) error`, `StatusDetail`, `StatusFailedPrefix`, `DefaultProcessTimeout` | the after-commit work, callable from a job; the file is `processing` while it runs, then `ready` or `failed` (`ocr: <code>`) |
| `Location func(f records.File, name string) (disk, prefix string, err error)`, `DerivedText` | `WithLocation`'s function: where texts go, the disk (`""` for the image's own) and a key prefix; the plugin appends `<random>.txt`. An error writes nothing and marks the image `failed`; nil keeps the default |
| `TextKey(records.File) (string, bool)`, `TextLocation(records.File) (disk, key string, ok bool)`, `MetaTextKey`, `MetaTextDisk` | the key of a file's text (`ocr_text_key`), and its disk (`ocr_text_disk`, only when `WithLocation` chose another disk than the image's) |
| `TextInfix`, `TextSuffix`, `TextContentType` | by default texts are stored as `<image key>.ocr-<random>.txt`, `text/plain; charset=utf-8`, on the image's disk |
| `records.ObjectOwner`, `records.FileDiskMover`, `records.DiskRenamer` (on `*Plugin`) | `OwnedObjects` claims every text a file row names, live or trashed, so `Store.SweepOrphans` never deletes one; one whose file was purged is swept. `MoveFileDisk` copies the texts on the file's own disk to the new disk under the same key when `Store.MoveFile` moves the file (size checked, old object deleted after the commit, copies deleted on rollback); one `WithLocation` put on a disk of its own stays there. A text missing from the disk fails the move. `RenameDisk` rewrites the recorded disk of every text on the renamed disk inside `Store.RenameDisk`'s transaction, so the `Location` readers return the new name |
| `ContentTypes()`, `PNG`, `JPEG`, `GIF` | the types read |
| `ErrNoProgram` | in the chain of the `CodeUnavailable` error of a run whose program is missing |
| `ProgramError{Program, Detail}` | the cause of a `CodeRejected` run: its text is safe for clients, `Detail` is the program's output for logs |
| `DefaultTesseract`, `DefaultLanguage`, `DefaultTimeout`, `DefaultMaxInput`, `DefaultMaxPixels`, `DefaultMaxText` | the defaults |

## Limits

- TIFF, WebP and HEIC are not read; convert them first.
- Tests use a stand-in script for every bound; the tests against the real engine skip only when `tesseract` (or Poppler, which renders their fixtures) is not installed, and say so.

## Quarantined files

A [quarantined](../../docs/records/processing-status.md#quarantine-until-scanned) image is not marked processing when it is recorded or replaced, and `Process` refuses it (`records.ErrQuarantined`); `file.released` starts the recognition when its scan passes.
