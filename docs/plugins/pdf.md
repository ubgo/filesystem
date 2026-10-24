# PDF page counts and previews

You will have every uploaded PDF checked and described: refused when it is not a readable PDF (damaged, disguised, locked with a password) or has too many pages, and stored with its page count and first page size on its file row and a preview image of the first page beside it, so a document list can show "12 pages" and a thumbnail without opening the file again. The package is `contrib/pdf`, which runs Poppler's `pdfinfo` and `pdftoppm` programs.

## Before you start

- Poppler installed where the server runs (`brew install poppler`, `apt install poppler-utils`). The programs are found on `PATH`, or set `pdf.Tool.PDFInfo` and `pdf.Tool.PDFToPPM` to their paths.
- `go get github.com/ubgo/filesystem/contrib/pdf`. It is its own module because it runs external programs; the core never does.
- The example is [`examples/media/pdf`](../../examples/media/pdf/main.go), in the separate module `examples/media`: `go run ./pdf report.pdf` from `examples/media` uploads a file and prints what was recorded; `go test ./pdf` checks it on a generated document.
- [Uploads](../concepts/uploads.md) explains `Inspect`; [files as records](../records/README.md) explains plugins.

## What it guards against

A PDF is untrusted input to a large C library, so every call is bounded: at most `Tool.MaxInput` bytes (200 MiB by default) are copied to a work file, each program run is killed after `Tool.Timeout` (30 seconds), and its output is capped (`Tool.MaxPreview`, 10 MiB). Programs run through `os/exec` without a shell. A preview is scaled to fit `Tool.MaxSide` pixels (1024) on its longest side whatever page size the file claims, so a page declared 200 inches wide renders no larger than any other. A file Poppler cannot read is `CodeRejected` with a message that names nothing of the server (`pdf: pdfinfo could not read the file`); Poppler's own output is in `pdf.ProgramError.Detail` for your logs.

## Step 1 — The upload policy

`Tool.Inspect(limits)` is a `Policy.Inspect` that reads the stored file and refuses what `pdfinfo` cannot read or what has more than `MaxPages` pages; other content types pass untouched:

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

## Step 2 — The records plugin

`pdf.NewPlugin(tool, opts...)` describes every PDF file when it is recorded and when its content is replaced: `pages`, `page_width_pt` and `page_height_pt` (the first page, in points) in the file's metadata. Only that `pdfinfo` step runs inside the upload's transaction. `WithPreview()` also stores a JPEG of the first page next to the PDF and records its key as `preview_key`, but that work, like the text, OCR and conversion below, runs after the upload commits: the file is marked `processing` (status detail `pdf: processing`) in the upload's transaction, then `Plugin.Process` runs outside any transaction under its own timeout (`WithProcessTimeout`, 10 minutes), records what it made on the row and marks the file `ready`, or `failed` with the reason (`pdf: unavailable` for a missing program or a timeout). The upload never fails because of it, and never holds its transaction open while a program runs. Read the file again (`store.File`) for the preview's key: the `File` that `Upload` returns is the row as inserted. The preview follows the file: deleted after a purge commits and replaced with the content; a run whose content was replaced meanwhile keeps nothing. Call `Process` from a job to retry a failed file or to backfill:

<!-- ds:block id=newstore-fd7vah94 -->
**newStore** · [`examples/media/pdf/main.go:85-108`](../../examples/media/pdf/main.go#L85-L108)

```go
func newStore(ctx context.Context) (*records.Store, *filesystem.Disk, error) {
	disk, err := filesystem.NewDisk(diskMedia, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1) // one connection keeps the in-memory database alive
	plugin := pdf.NewPlugin(tool, pdf.WithPreview())
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(plugin))
	if err != nil {
		return nil, nil, err
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return store, disk, nil
}
```
<!-- /ds:block hash=c319e0 -->

Read it back with `pdf.Of(file)` (a `pdf.Meta` with the page count and size) and `pdf.PreviewKey(file)`.

Check: record a three-page A4 report, then a copy cut short:

```sh
cd examples/media
go run ./pdf report.pdf
go run ./pdf cut.pdf
```

```text
stored    documents/n2jpezfkd4rzrlqlz7elvpwosu.pdf (application/pdf, 3577 bytes)
pages     3
page 1    595x842 pt
preview   documents/n2jpezfkd4rzrlqlz7elvpwosu.pdf.preview-db7kqww75l.jpg (image/jpeg, 4731 bytes)
2026/10/06 11:46:07 filesystem: rejected: pdf: pdfinfo could not read the file
```

### Previews and texts on another disk

By default the preview and the text sit beside their file, on its disk. `pdf.WithLocation` puts them elsewhere: previews on a public CDN disk while the documents stay private, texts in the bucket a search indexer reads. The function gets the file and the derived object's name and returns a disk name of the Store's Manager (`""` for the file's own) and a key prefix; the plugin appends a random part and the suffix, so a replacement's objects never overwrite the current ones before it is processed:

```go
plugin := pdf.NewPlugin(tool, pdf.WithPreview(), pdf.WithText(),
	pdf.WithLocation(func(f records.File, name string) (string, string, error) {
		if name == pdf.DerivedPreview {
			return "cdn", "previews/" + f.ID + "-", nil
		}
		return "search", f.Scope + "/" + f.ID + "-", nil
	}))
```

An object's disk is recorded as `preview_disk` or `text_disk` when it is not the file's, and `pdf.PreviewLocation(f)` and `pdf.TextLocation(f)` return the disk and the key; the purge and a replacement delete them there. An error from the function writes nothing more, deletes what the run already stored and marks the file `failed` (`pdf: <code>`).

Derived objects are no orphans: the plugin implements `records.ObjectOwner`, so `Store.SweepOrphans` keeps every preview and text a file row names (live or trashed) and deletes only one whose file was purged, and `records.FileDiskMover`, so `Store.MoveFile` takes the preview and text on the file's own disk along to the new disk under the same key. One `WithLocation` put on a disk of its own stays where it is. A preview or text missing from the disk fails the move, as a missing variant does, rather than leaving the file pointing at nothing. And `records.DiskRenamer`: `Store.RenameDisk` rewrites the disk recorded for a derived object on the renamed disk in the same transaction, so the `Location` readers return the new name.

## Step 3 — Document information, text and office files

The plugin also records what the file says about itself: `doc_title`, `doc_author`, `doc_created` (RFC 3339) and `encrypted` (`true` for a file restricted by an owner password), read back through `pdf.Of(file)` as `Meta.Title`, `Meta.Author`, `Meta.Created` and `Meta.Encrypted`. These are claims by whoever made the file: control characters are removed and title and author are cut to 255 characters, and they never overwrite the application's own `title` (`records.MetaTitle`).

`WithText()` stores the document's text next to it as a `text/plain; charset=utf-8` object for a search indexer, its key in `text_key` (read it with `pdf.TextKey(file)`); `text_truncated` is `true` when the text stopped at `Tool.MaxText` (1 MiB) or `Tool.MaxTextPages` (2000 pages). A document without a letter or digit in its text gets no text object. The text is made after the upload commits, like the preview, and follows the file: deleted on purge and when the content is replaced. `Tool.Text(ctx, r, nil)` returns the same text without the plugin.

`WithOCR(recognizer)` is `WithText` that also reads scans: when a PDF has no text layer, its first `Tool.MaxOCRPages` pages (20) are rendered at `Tool.OCRSide` pixels (3508, A4 at 300 dpi) and passed to the `pdf.Recognizer`, and `text_ocr` is set. [contrib/ocr](ocr.md)'s `ocr.Tool` is one; a cloud OCR service is another, billed per page, which is what `MaxOCRPages` bounds.

`WithConverter(converter)` describes other document types through the PDF a `pdf.Converter` makes of them: page count, information, preview and text, as for a PDF. [contrib/office](office.md)'s `office.Tool` converts Word, Excel, PowerPoint and OpenDocument files with LibreOffice. The search example in `examples/documents` installs all three:

<!-- ds:block id=plugins-vbctunwf -->
**plugins** · [`examples/documents/search/main.go:56-64`](../../examples/documents/search/main.go#L56-L64)

```go
func plugins() []records.Plugin {
	reader := ocr.Tool{Languages: []string{"eng"}}
	docs := pdf.NewPlugin(pdf.Tool{MaxOCRPages: 10},
		pdf.WithPreview(),
		pdf.WithOCR(reader),
		pdf.WithConverter(office.Tool{}),
	)
	return []records.Plugin{docs, ocr.NewPlugin(reader)}
}
```
<!-- /ds:block hash=52f924 -->

Check: `go run ./search report.pdf receipt.jpg` from `examples/documents`, with a PDF and a photo of a receipt:

```text
report.pdf (application/pdf)
  pages     1
  title     Q3 Report
  preview   stored
  text      26 bytes (text layer): Revenue grew 12 percent
receipt.jpg (image/jpeg)
  text      15 bytes (OCR): Total due 42.50
```

## Options

| Option | What it does |
|---|---|
| `Tool.PDFInfo`, `Tool.PDFToPPM`, `Tool.PDFToText` | program paths or names; empty means `pdfinfo`, `pdftoppm` and `pdftotext` on `PATH` |
| `Tool.Timeout` | per program run; 0 means 30 seconds; a run cut short is `CodeUnavailable` |
| `Tool.MaxInput` | bytes copied to the work file; 0 means 200 MiB; more is `CodeTooLarge` |
| `Tool.MaxPreview` | a preview's encoded size; 0 means 10 MiB |
| `Tool.MaxSide` | a preview's longest side in pixels; 0 means 1024 |
| `Tool.MaxText`, `Tool.MaxTextPages` | extracted text in bytes (1 MiB) and pages read (2000); past either the text is cut and marked truncated |
| `Tool.MaxOCRPages`, `Tool.OCRSide` | pages of a scan sent to a recognizer (20) and their longest side in pixels (3508) |
| `Tool.TempDir` | where work files go; empty means the system's temp directory; they are removed before each call returns |
| `pdf.Limits{MaxPages}` | what `Inspect` accepts; 0 does not limit |
| `pdf.WithPreview()` | the plugin also stores a preview of the first page |
| `pdf.WithLocation(fn)` | where the preview and the text go: `fn(f, name)` (`pdf.DerivedPreview` or `pdf.DerivedText`) returns a disk name (`""` for the file's own) and a key prefix; the plugin appends `<random><suffix>`. An error writes nothing, deletes what the run already wrote and marks the file `failed`; nil keeps the default, beside the file |
| `pdf.WithText()`, `pdf.WithOCR(r)` | the plugin also stores the text; with `WithOCR`, a scan's text through `r` |
| `pdf.WithConverter(c)` | the plugin also describes the types `c` converts to PDF |
| `pdf.WithDeferredProcessing()` | an upload only marks the file `processing` and returns; `Resume` does the work. The default runs it inline after the commit, which needs no setup but holds the `Upload` call for the whole run (up to the process timeout per upload, even after the client has gone away) |
| `Plugin.Resume(ctx, store, olderThan)` | runs `Process` for every file, in any scope, still `processing` (with the plugin's detail) and recorded before `olderThan`: finishes what a crash left, and does the work under `WithDeferredProcessing`. Schedule it with `jobs.Func`; pick `olderThan` past the process timeout, or the current time with `WithDeferredProcessing` |
| `pdf.WithProcessTimeout(d)` | bounds one `Process` run (conversion, preview, text and OCR of one file); default 10 minutes |
| `Tool.Env` | variables added to the programs' minimal environment, as `KEY=value` |
| `pdf.WithStrict()` | the plugin refuses a PDF file `pdfinfo` cannot read, instead of recording it without metadata |

## Pitfalls

- **A missing Poppler fails every PDF upload.** The plugin and `Inspect` return an error wrapping `pdf.ErrNoProgram` (`CodeUnavailable`) rather than recording PDFs without metadata.
- **PDFs locked with a password are refused.** `pdfinfo` cannot open a file encrypted with a user password, so `Inspect` refuses it, and the plugin records it without metadata (or refuses it with `WithStrict`). A file restricted only by an owner password (no printing, no copying) opens and is accepted.
- **The preview is the first page only, as a JPEG.** Serve it like any other image; for more pages, run `pdftoppm` from a job.
- **Slow work runs after the commit, still in the request.** `AfterCommit` work runs before `Upload` returns, outside the transaction; a scan costs one render and one recognition per page and a conversion a LibreOffice start. Install the plugin without `WithPreview`, `WithText` or `WithConverter` and call `Process` from a job when that latency matters. `WithStrict` covers only the `pdfinfo` step inside the upload.
- **Programs see a minimal environment.** Each program gets `PATH`, `HOME`, `TMPDIR`, `TMP`, `LANG` and `LC_ALL` of the server and nothing else, so database URLs and cloud keys never reach a parser of untrusted files; `Tool.Env` adds what a program needs.
- **`pages` and `encrypted` on your own files are left alone.** The plugin sets and clears its keys only on files it describes (PDFs and the types its converter takes); when a PDF is replaced by another type, every key it wrote goes. The four keys that name objects (`preview_key`, `preview_disk`, `text_key`, `text_disk`) are the exception: they are dropped from the metadata you pass for a new file of any type, because the purge deletes what they name and the orphan sweep keeps it.
- **Embedded titles are not yours.** `doc_title` is what the file claims; show it as such, and keep your own heading in `records.MetaTitle`.

## Quarantined files

A [quarantined](../records/processing-status.md#quarantine-until-scanned) file is neither described nor marked processing when it is recorded or replaced, and `Process` refuses it (`records.ErrQuarantined`); when its scan passes it is described (`BeforeRelease`) and `file.released` starts `Process` (preview, text, conversion).

## Next

- [Video metadata and posters](video.md) and [SVG sanitising](svg.md): the other untrusted formats.
- [Image variants](variants.md): thumbnails of images.
