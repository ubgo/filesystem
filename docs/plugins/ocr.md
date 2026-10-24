# Text from images and scans (OCR)

You will have the text in every uploaded image (a photographed receipt, a screenshot, a scanned page) and in every scanned PDF stored beside the file, for a search index. The package is `contrib/ocr`, which runs the Tesseract OCR engine's `tesseract` program.

## Before you start

- Tesseract installed where the server runs (`brew install tesseract`, `apt install tesseract-ocr`), with the language models you read (`apt install tesseract-ocr-deu`, `brew install tesseract-lang`). `tesseract --list-langs` shows them. The program is found on `PATH`, or set `ocr.Tool.Tesseract` to its path.
- `go get github.com/ubgo/filesystem/contrib/ocr`. It is its own module because it runs an external program; the core never does, and it imports nothing but the standard library and the library's own packages.
- The example is [`examples/documents/search`](../../examples/documents/search/main.go), in the separate module `examples/documents`.

## What it guards against

An image is untrusted input to a C++ program, so every call is bounded: at most `Tool.MaxInput` bytes (50 MiB) are copied to a work file; the file must be a PNG, JPEG or GIF by its header and at most `Tool.MaxPixels` pixels (50 million, judged from the header before anything is decoded); the run is killed after `Tool.Timeout` (1 minute); and more than `Tool.MaxText` bytes of text (1 MiB) is `CodeTooLarge`. The header check matters beyond memory: `tesseract` reads a text file as a list of image paths to open, so a text file uploaded as an image could make it read other files on the server; such a file never reaches the program. Languages must match `[A-Za-z][A-Za-z0-9_]{0,31}` before they become an argument, so nothing like `--tessdata-dir=/etc` or `../x` passes (`CodeInvalidArgument`). The program runs without a shell. A file the engine cannot read is `CodeRejected` with a message that names nothing of the server; its own output is in `ocr.ProgramError.Detail` for your logs.

## Step 1 — Images

`ocr.NewPlugin(tool)` reads every PNG, JPEG and GIF file when it is recorded and when its content is replaced, and stores the text next to it as a `text/plain; charset=utf-8` object, its key in `ocr_text_key` (read it with `ocr.TextKey(file)`). An image without text, or one the engine cannot read, gets none. Recognition never runs inside the upload's transaction: the file is marked `processing` (status detail `ocr: processing`) in it, then `Plugin.Process` runs once it commits, outside any transaction under its own timeout (`WithProcessTimeout`, 5 minutes), records the key and marks the file `ready`, or `failed` with the reason (`ocr: unavailable` for a missing `tesseract` or a timeout); the upload itself never fails because of it. Read the file again (`store.File`) for the key. The text follows the file: deleted after a purge commits and replaced with the content; a run whose image was replaced meanwhile keeps nothing. Call `Process` from a job to retry a failed file or backfill older ones.

## Step 2 — Scanned PDFs

`ocr.Tool` is a `pdf.Recognizer`. Pass it to the [PDF plugin](pdf.md) with `pdf.WithOCR(tool)`: a PDF with a text layer is read with `pdftotext` as usual, and one without (no letter or digit in its text) has its first `pdf.Tool.MaxOCRPages` pages rendered and read by Tesseract, its text stored like any other with `text_ocr` set. The search example installs both plugins with one `ocr.Tool`:

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

## Step 3 — Read the text back

The text is an object beside the file; its key is in the file's metadata, under the PDF plugin's key for PDFs (`pdf.TextKey`) and the OCR plugin's for images (`ocr.TextKey`). Read the row again after the upload, since recognition runs after the commit:

<!-- ds:block id=upload1-k8w3c6nr -->
**upload1** · [`examples/documents/search/main.go:84-100`](../../examples/documents/search/main.go#L84-L100)

```go
func upload1(ctx context.Context, out io.Writer, store *records.Store, disk *filesystem.Disk, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	file, err := store.Upload(ctx, scope, diskMedia, f, filepath.Base(path), policy)
	if err != nil {
		return err
	}
	// The preview, text and OCR are made once the upload commits; the row
	// as stored now has them, or a failed status saying why not.
	if file, err = store.File(ctx, scope, file.ID); err != nil {
		return err
	}
	return report(ctx, out, disk, file)
}
```
<!-- /ds:block hash=0f7f69 -->

<!-- ds:block id=report-p6t2v9wm -->
**report** · [`examples/documents/search/main.go:131-167`](../../examples/documents/search/main.go#L131-L167)

```go
func report(ctx context.Context, out io.Writer, disk *filesystem.Disk, f records.File) error {
	fmt.Fprintf(out, "%s (%s)\n", f.Filename, f.ContentType)
	if f.Status == records.StatusFailed {
		fmt.Fprintf(out, "  failed    %s\n", f.StatusDetail)
	}
	if m, ok := pdf.Of(f); ok {
		fmt.Fprintf(out, "  pages     %d\n", m.Pages)
		if m.Title != "" {
			fmt.Fprintf(out, "  title     %s\n", m.Title)
		}
	}
	if _, ok := pdf.PreviewKey(f); ok {
		fmt.Fprintln(out, "  preview   stored")
	}
	key, ok := pdf.TextKey(f)
	if !ok {
		key, ok = ocr.TextKey(f)
	}
	if !ok {
		fmt.Fprintln(out, "  text      none")
		return nil
	}
	text, err := disk.ReadAll(ctx, key)
	if err != nil {
		return err
	}
	how := "text layer"
	if ocred, _ := strconv.ParseBool(f.Metadata[pdf.MetaTextOCR]); ocred || strings.HasPrefix(key, f.Key+ocr.TextInfix) {
		how = "OCR"
	}
	line := strings.Join(strings.Fields(string(text)), " ")
	if len(line) > excerpt {
		line = line[:excerpt] + "..."
	}
	fmt.Fprintf(out, "  text      %d bytes (%s): %s\n", len(text), how, line)
	return nil
}
```
<!-- /ds:block hash=42fafb -->

To keep the texts out of the media bucket, in the one your search indexer reads, give the plugin a location; the text's disk is recorded as `ocr_text_disk` and `ocr.TextLocation(f)` returns the disk and the key:

```go
plugin := ocr.NewPlugin(tool, ocr.WithLocation(func(f records.File, name string) (string, string, error) {
	return "search", f.Scope + "/" + f.ID + "-", nil
}))
```

Derived objects are no orphans: the plugin implements `records.ObjectOwner`, so `Store.SweepOrphans` keeps every text a file row names (live or trashed) and deletes only one whose file was purged, and `records.FileDiskMover`, so `Store.MoveFile` takes the text on the file's own disk along to the new disk under the same key. One `WithLocation` put on a disk of its own stays where it is. A text missing from the disk fails the move, as a missing variant does, rather than leaving the file pointing at nothing. And `records.DiskRenamer`: `Store.RenameDisk` rewrites the disk recorded for a derived object on the renamed disk in the same transaction, so the `Location` readers return the new name.

`MetaTextOCR` on a PDF, or a text found through `ocr.TextKey` (under `ocr.TextInfix` by default), says the text came from recognition rather than a text layer: weight it lower in search, since OCR makes mistakes.

Check: from `examples/documents`, `go run ./search report.pdf receipt.jpg` with a photo of a receipt prints `text      15 bytes (OCR): Total due 42.50` for the photo. `go test ./search` builds a text PDF and a photographed page itself and expects `(text layer): Revenue grew` and `(OCR): Total due`; it skips when Poppler or Tesseract is missing.

## Options

| Option | What it does |
|---|---|
| `Tool.Tesseract` | the program's path or name; empty means `tesseract` on `PATH` |
| `Tool.Languages` | the models to read with, in order (`[]string{"eng", "deu"}`); empty means `eng` |
| `Tool.Timeout` | per image; 0 means 1 minute; a run cut short is `CodeUnavailable` |
| `Tool.MaxInput` | bytes of an image; 0 means 50 MiB; more is `CodeTooLarge` |
| `Tool.MaxPixels` | width times height; 0 means 50 million; more is `CodeTooLarge` |
| `Tool.MaxText` | bytes of text of one image; 0 means 1 MiB; more is `CodeTooLarge` |
| `Tool.Env` | variables added to the program's minimal environment (`TESSDATA_PREFIX=/opt/tessdata`) |
| `ocr.WithDeferredProcessing()` | an upload only marks the file `processing` and returns; `Resume` does the work. The default runs it inline after the commit, which needs no setup but holds the `Upload` call for the whole run (up to the process timeout per upload, even after the client has gone away) |
| `Plugin.Resume(ctx, store, olderThan)` | runs `Process` for every file, in any scope, still `processing` (with the plugin's detail) and recorded before `olderThan`: finishes what a crash left, and does the work under `WithDeferredProcessing`. Schedule it with `jobs.Func`; pick `olderThan` past the process timeout, or the current time with `WithDeferredProcessing` |
| `ocr.WithProcessTimeout(d)` | bounds one `Process` run; default 5 minutes |
| `ocr.WithLocation(fn)` | where texts go: `fn(f, ocr.DerivedText)` returns a disk name (`""` for the image's own) and a key prefix; the plugin appends `<random>.txt`. An error writes nothing and marks the image `failed`; nil keeps the default, beside the image |
| `ocr.WithContentTypes(types...)` | the types the plugin reads, replacing PNG, JPEG and GIF; name only types the engine reads |
| `Tool.TempDir` | where work files go; empty means the system's temp directory; they are removed before each call returns |

## Another engine

`NewPlugin` takes any `ocr.Recognizer`, one method with the same shape as `pdf.Recognizer`. `Tool` (Tesseract) is one; a cloud service is another, wrapped in a small type:

```go
type textract struct{ client *textractsdk.Client }

func (t textract) Recognize(ctx context.Context, image io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(image, 10<<20))
	if err != nil {
		return "", err
	}
	return detectText(ctx, t.client, b) // your call to the service
}

plugin := ocr.NewPlugin(textract{client}, ocr.WithContentTypes("image/png", "image/jpeg", "image/tiff"))
```

The engine bounds its own work (input size, time, output), as `Tool` does through its limits. Its error marks the file `failed` with the error's code; the upload never fails. `WithContentTypes` names the types it reads, TIFF and WebP scans for most cloud engines; without it the plugin reads Tesseract's three.

## Pitfalls

- **Only PNG, JPEG and GIF.** Convert TIFF, WebP or HEIC first ([image processing](image-processing.md)); other types pass the plugin untouched.
- **Accuracy follows resolution.** Tesseract is tuned for text about 300 dpi; a thumbnail reads badly. Scans in PDFs are rendered at `pdf.Tool.OCRSide` (3508 pixels, A4 at 300 dpi).
- **Recognition runs after the commit, still in the request.** `AfterCommit` work runs before `Upload` returns, a few seconds for a dense page; a scanned PDF costs a render and a recognition per page up to `MaxOCRPages`. Call `Process` from a job instead when that latency matters.
- **Tesseract sees a minimal environment.** It gets `PATH`, `HOME`, `TMPDIR`, `TMP`, `LANG` and `LC_ALL` of the server and nothing else; set `Tool.Env` to `TESSDATA_PREFIX=...` when the models are not where it was built to look.

## Quarantined files

A [quarantined](../records/processing-status.md#quarantine-until-scanned) image is not marked processing when it is recorded or replaced, and `Process` refuses it (`records.ErrQuarantined`); `file.released` starts the recognition when its scan passes.

## Next

- [PDF page counts and previews](pdf.md): text of PDFs with a text layer.
- [Auto-tagging](vision.md): what an image shows rather than what it says.
