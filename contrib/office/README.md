# contrib/office

`github.com/ubgo/filesystem/contrib/office` (package `office`) converts office documents (DOCX, XLSX, PPTX, ODT, ODS, ODP) to PDF with LibreOffice's `soffice` in headless mode, so [contrib/pdf](../pdf/README.md)'s plugin describes them like PDFs: page count, a preview and the text. Each conversion runs with a fresh temporary HOME and profile, macros off, a time cap that kills every process the run started, and a cap on the PDF while it is written. It is its own module because it runs an external program; the core never does, and this module imports nothing but the standard library and the library's own packages (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/office
```

Needs `soffice` on `PATH` (or a configured path). The guide: [Office documents](../../docs/plugins/office.md). The example: [`examples/documents/search`](../../examples/documents/search/main.go).

## API

| Name | What it is |
|---|---|
| `Tool` | the converter; the zero value uses the defaults. Fields: `SOffice`, `ContentTypes`, `Timeout`, `MaxInput`, `MaxOutput`, `TempDir`, `Env` (LibreOffice otherwise sees only `PATH`, `LANG`, `LC_ALL` and its run's own `HOME` and temporary directory) |
| `Tool.Converts(contentType) bool` | whether the type is converted |
| `Tool.ConvertToPDF(ctx, r, contentType) (io.ReadCloser, error)` | the PDF, as an unlinked file the caller closes; a document LibreOffice cannot convert is `CodeRejected` |
| `DefaultContentTypes()`, `DOCX`, `XLSX`, `PPTX`, `ODT`, `ODS`, `ODP` | the types converted by default |
| `ErrNoProgram` | in the chain of the `CodeUnavailable` error of a conversion whose program is missing |
| `ProgramError{Program, Detail}` | the cause of a `CodeRejected` conversion: its text is safe for clients, `Detail` is LibreOffice's output for logs |
| `DefaultSOffice`, `DefaultTimeout`, `DefaultMaxInput`, `DefaultMaxOutput` | the defaults |

## Limits

- The network is not fenced: run conversions without outbound access when documents come from strangers.
- On Windows a cancelled conversion kills only the launcher (no process groups); Windows is not yet supported by the library.
- Tests check every fence with a stand-in script; the real conversion test skips only when `soffice` is not installed, and says so.
