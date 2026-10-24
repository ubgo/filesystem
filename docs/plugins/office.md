# Office document previews and text

You will have every uploaded Word, Excel, PowerPoint or OpenDocument file described like a PDF: its page count, a preview image of the first page and its text for search, by converting it to PDF with LibreOffice and handing the PDF to the [PDF plugin](pdf.md). The package is `contrib/office`, which runs LibreOffice's `soffice` in headless mode.

## Before you start

- LibreOffice installed where the server runs (`brew install --cask libreoffice`, `apt install libreoffice-core libreoffice-writer libreoffice-calc libreoffice-impress`). `soffice` is found on `PATH`, or set `office.Tool.SOffice` to its path (on macOS, `/Applications/LibreOffice.app/Contents/MacOS/soffice`).
- Poppler, for the PDF plugin that describes the result.
- `go get github.com/ubgo/filesystem/contrib/office`. It is its own module because it runs an external program; the core never does, and it imports nothing but the standard library and the library's own packages.
- The example is [`examples/documents/search`](../../examples/documents/search/main.go), in the separate module `examples/documents`.

## What it guards against

An office document is untrusted input to a very large program. Each conversion:

- copies at most `Tool.MaxInput` bytes (100 MiB) to a private directory, refusing more with `CodeTooLarge`;
- runs with a fresh `HOME`, temporary directory and LibreOffice profile of its own, so runs share nothing and can run in parallel, and nothing is written outside that directory, which is removed before the call returns;
- starts from a hardened profile: macro security "very high" and macro execution disabled, links to other documents and remote content never updated while loading, untrusted referer links blocked;
- passes LibreOffice only `PATH`, `LANG` and `LC_ALL` of the server plus `Tool.Env`, never the application's secrets (database URLs, cloud keys), so a document that gets code running cannot read them;
- runs without a shell, in a process group of its own, and the whole group (the `soffice` launcher and the `soffice.bin` it starts) is killed after `Tool.Timeout` (2 minutes) and after a normal exit;
- watches the PDF while it is written and stops the run when it passes `Tool.MaxOutput` (200 MiB), so a document cannot fill the disk.

What the package cannot fence is the network: a document may still name remote resources, so run conversions where the process has no outbound access when documents come from strangers. Windows has no process groups, so there a cancelled conversion kills only the launcher.

## Step 1 — Plug the converter into the PDF plugin

`office.Tool` is a `pdf.Converter`. Pass it to `pdf.WithConverter`, and the PDF plugin describes every type it converts (by default DOCX, XLSX, PPTX, ODT, ODS and ODP) through the converted PDF; the PDF itself is not kept:

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

The conversion runs after the upload commits, never inside its transaction (see the [PDF plugin](pdf.md#step-2--the-records-plugin)): the file is marked `processing`, then gets the PDF plugin's metadata (`pages`, `page_width_pt`, `doc_title`, ...), and with `WithPreview` and `WithText` a preview and a text object beside it (or where `pdf.WithLocation` puts them), deleted with the file, and is marked `ready`. A document LibreOffice cannot convert is recorded without metadata and marked `failed` (`pdf: rejected`); a missing `soffice` marks it `failed` (`pdf: unavailable`), and the error wraps `office.ErrNoProgram`.

Upload as usual and read the row back once the work has run; what the PDF plugin recorded for a converted document is read the same way as for a PDF:

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

## Step 2 — Convert on your own

`Tool.ConvertToPDF(ctx, r, contentType)` returns the PDF as an `io.ReadCloser`: an unlinked file, so nothing is left on disk whatever the caller does. Use it to keep a PDF rendition next to the original, or to convert from a job instead of inside the upload. `Tool.Converts(contentType)` says whether a type is converted. The example's `convert.go` keeps a rendition beside a recorded document:

<!-- ds:block id=keeppdfrendition-c4n9w7tk -->
**keepPDFRendition** · [`examples/documents/search/convert.go:21-39`](../../examples/documents/search/convert.go#L21-L39)

```go
func keepPDFRendition(ctx context.Context, disk *filesystem.Disk, f records.File) (string, error) {
	tool := office.Tool{}
	if !tool.Converts(f.ContentType) {
		return "", filesystem.Errorf(filesystem.CodeUnsupported, "%s is not an office document", f.ContentType)
	}
	src, err := disk.Read(ctx, f.Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	out, err := tool.ConvertToPDF(ctx, src, f.ContentType)
	if err != nil {
		return "", err
	}
	defer func() { _ = out.Close() }()
	key := f.Key + pdfSuffix
	_, err = disk.Write(ctx, key, out, filesystem.WriteOptions{ContentType: pdf.ContentType})
	return key, err
}
```
<!-- /ds:block hash=b489a1 -->

A PDF rendition stored this way is an object, not a file row: delete it with the document (an `EventHooks` plugin on `file.purged`), or record it with `CreateFile` and [attach](../records/attachments.md) it. The tests `TestRenditionRefusesOtherTypes` and `TestRenditionWithoutLibreOffice` (which runs where `soffice` is absent, and checks the error wraps `office.ErrNoProgram`) run without LibreOffice.

## Try it

```sh
cd examples/documents
go run ./search deck.pptx       # needs LibreOffice and Poppler
go test ./search                # converts nothing; skips what needs missing programs
```

## Options

| Option | What it does |
|---|---|
| `Tool.SOffice` | the program's path or name; empty means `soffice` on `PATH` |
| `Tool.ContentTypes` | the types converted; empty means `office.DefaultContentTypes()` (OOXML and OpenDocument). The older binary formats (`application/msword`, ...) are left out because macro documents mostly live there; list them to accept them |
| `Tool.Timeout` | per conversion; 0 means 2 minutes; a run cut short is `CodeUnavailable` |
| `Tool.MaxInput` | bytes of a document; 0 means 100 MiB; more is `CodeTooLarge` |
| `Tool.MaxOutput` | bytes of the PDF; 0 means 200 MiB; a run writing more is stopped, `CodeTooLarge` |
| `Tool.Env` | variables added to LibreOffice's minimal environment; `HOME` and the temporary directory stay the run's own |
| `Tool.TempDir` | where each run's private directory goes; empty means the system's temp directory |

## Pitfalls

- **LibreOffice starts slowly.** A fresh profile per run costs a few seconds before the first page is laid out; the conversion runs after the upload commits but before `Upload` returns. Call `pdf.Plugin.Process` from a job when that matters.
- **Fonts decide the layout.** A document using fonts the server lacks is laid out with substitutes, so page counts can differ from the author's machine. Install the fonts your users use (`fonts-crosextra-carlito`, `fonts-crosextra-caladea` for Calibri and Cambria metrics).
- **Tests without LibreOffice.** The package's tests check every fence with a stand-in script; the conversion of a real document runs only when `soffice` is installed, and the skip says so.

## Next

- [PDF page counts and previews](pdf.md): what the converted PDF gives the file.
- [OCR](ocr.md): text from images and scans.
