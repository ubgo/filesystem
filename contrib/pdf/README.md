# contrib/pdf

`github.com/ubgo/filesystem/contrib/pdf` (package `pdf`) reads what applications need from uploaded PDFs with Poppler's programs: `pdfinfo` for the page count, the first page's size and the document's title, author, creation date and encryption, `pdftoppm` for a preview of the first page, and `pdftotext` for the text, with OCR of scans through a `Recognizer` and other document types through a `Converter`. On top of that: an `upload.Inspector` that refuses unreadable, locked or over-long PDFs, and a records plugin that stores the page count and a preview with every PDF file. It is its own module because it runs external programs; the core never does, and this module imports nothing but the standard library and the library's own packages (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/pdf
```

Needs `pdfinfo` and `pdftoppm` on `PATH` (or configured paths), and `pdftotext` for text. The guide, with output from a run: [PDF](../../docs/plugins/pdf.md). The policy of [`examples/media/pdf`](../../examples/media/pdf/main.go):

<!-- ds:block id=policy-c9vcfm4h -->
**policy** · [`examples/media/pdf/main.go:44-49`](../../examples/media/pdf/main.go#L44-L49)

```go
var policy = upload.Policy{
	MaxSize: 50 << 20,
	Allowed: []string{pdf.ContentType},
	Key:     upload.RandomKey("documents"),
	Inspect: tool.Inspect(pdf.Limits{MaxPages: maxPages}),
}
```
<!-- /ds:block hash=bd0a47 -->

## API

| Name | What it is |
|---|---|
| `Tool` | runs the programs; the zero value uses the defaults. Fields: `PDFInfo`, `PDFToPPM`, `PDFToText`, `Timeout`, `MaxInput`, `MaxPreview`, `MaxSide`, `MaxText`, `MaxTextPages`, `MaxOCRPages`, `OCRSide`, `TempDir`, `Env` (programs otherwise see only `PATH`, `HOME`, `TMPDIR`, `TMP`, `LANG`, `LC_ALL`) |
| `Tool.Probe(ctx, r) (Info, error)` | `Info{Pages, Width, Height, Title, Author, Created, Encrypted}` (size of the first page, in points; title and author cleaned and cut to `MaxInfoChars`); an unreadable or locked file is `CodeRejected` |
| `Tool.Text(ctx, r, Recognizer) (Extract, error)` | `Extract{Text, Truncated, OCR}`: the text by `pdftotext`, at most `MaxText` bytes and `MaxTextPages` pages; a scan read page by page through the `Recognizer` when it is not nil |
| `Recognizer`, `Converter` | the two seams: OCR of one page image (`contrib/ocr`), and conversion of another type to PDF (`contrib/office`) |
| `Tool.Preview(ctx, r) ([]byte, error)` | the first page as a JPEG fitting `MaxSide` pixels |
| `Tool.Inspect(Limits) upload.Inspector` | refuses a stored PDF that is unreadable or has more than `Limits{MaxPages}` pages; other types pass |
| `NewPlugin(Tool, ...PluginOption) *Plugin`, `WithPreview()`, `WithText()`, `WithOCR(Recognizer)`, `WithConverter(Converter)`, `WithProcessTimeout(d)`, `WithDeferredProcessing()`, `WithStrict()`, `WithLocation(Location)` | the records plugin (`PluginName` `pdf`): `pdfinfo` inside the upload's transaction, everything else in `Plugin.Process` after it commits |
| `Plugin.Resume(ctx, store, olderThan) (int, error)` | re-runs `Process` for files still `processing` past the cutoff (a crash, or `WithDeferredProcessing`); schedule it with `jobs.Func` |
| `Plugin.Process(ctx, store, scope, id) error`, `StatusDetail`, `StatusFailedPrefix`, `DefaultProcessTimeout` | the after-commit work, callable from a job to retry or backfill; the file is `processing` with `StatusDetail` while it runs, then `ready` or `failed` (`pdf: <code>`) |
| `Location func(f records.File, name string) (disk, prefix string, err error)`, `DerivedPreview`, `DerivedText` | `WithLocation`'s function: where the preview and the text go, the disk (`""` for the file's own) and a key prefix; the plugin appends `<random><suffix>`. An error writes nothing (or deletes what the run wrote) and marks the file `failed`; nil keeps the default |
| `Of(records.File) (Meta, bool)`, `PreviewKey(records.File) (string, bool)`, `TextKey(records.File) (string, bool)`, `PreviewLocation(records.File)`, `TextLocation(records.File)` (`disk, key string, ok bool`) | read what the plugin recorded; the `Location` readers also say which disk each object is on |
| `MetaPages`, `MetaPageWidth`, `MetaPageHeight`, `MetaDocTitle`, `MetaDocAuthor`, `MetaDocCreated`, `MetaEncrypted`, `MetaPreviewKey`, `MetaPreviewDisk`, `MetaTextKey`, `MetaTextDisk`, `MetaTextCut`, `MetaTextOCR` | the metadata keys: `pages`, `page_width_pt`, `page_height_pt`, `doc_title`, `doc_author`, `doc_created`, `encrypted`, `preview_key`, `preview_disk`, `text_key`, `text_disk`, `text_truncated`, `text_ocr`; the disk keys only when `WithLocation` chose another disk than the file's. They are set and cleared only on files the plugin describes (PDFs and converted types), so another file keeps an application's own `pages` or `encrypted` |
| `PreviewInfix`, `PreviewSuffix`, `PreviewContentType` | by default previews are stored as `<pdf key>.preview-<random>.jpg`, `image/jpeg`, on the file's disk |
| `TextInfix`, `TextSuffix`, `TextContentType` | by default texts are stored as `<pdf key>.text-<random>.txt`, `text/plain; charset=utf-8`, on the file's disk |
| `records.ObjectOwner`, `records.FileDiskMover`, `records.DiskRenamer` (on `*Plugin`) | `OwnedObjects` claims every preview and text a file row names, live or trashed, so `Store.SweepOrphans` never deletes one; one whose file was purged is swept. `MoveFileDisk` copies the previews and texts on the file's own disk to the new disk under the same key when `Store.MoveFile` moves the file (size checked, old object deleted after the commit, copies deleted on rollback); one `WithLocation` put on a disk of its own stays there. A preview or text missing from the disk fails the move. `RenameDisk` rewrites the recorded disk of every preview or text on the renamed disk inside `Store.RenameDisk`'s transaction, so the `Location` readers return the new name |
| `ContentType` | `application/pdf`, the type the package acts on |
| `ErrNoProgram` | in the chain of the `CodeUnavailable` error of a run whose program is missing |
| `ProgramError{Program, Detail}` | the cause of a `CodeRejected` run: its text is safe for clients, `Detail` is Poppler's output for logs |
| `DefaultPDFInfo`, `DefaultPDFToPPM`, `DefaultPDFToText`, `DefaultTimeout`, `DefaultMaxInput`, `DefaultMaxPreview`, `DefaultMaxSide`, `DefaultMaxText`, `DefaultMaxTextPages`, `DefaultMaxOCRPages`, `DefaultOCRSide`, `MaxInfoChars` | the defaults and bounds |

## Limits

- Tests skip only when `pdfinfo` or `pdftoppm` is not installed, and say so; the locked-PDF case also needs `qpdf` and is logged as not run without it.

## Quarantined files

A [quarantined](../../docs/records/processing-status.md#quarantine-until-scanned) file is neither described nor marked processing when it is recorded or replaced, and `Process` refuses it (`records.ErrQuarantined`); when its scan passes it is described (`BeforeRelease`) and `file.released` starts `Process` (preview, text, conversion).
