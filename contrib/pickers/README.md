# contrib/pickers

`github.com/ubgo/filesystem/contrib/pickers` imports a file a user chose in a cloud picker (Google Drive Picker, Dropbox Chooser, OneDrive and SharePoint file picker) onto a disk, by file ID plus access token, through `upload.Receive`, so the upload policy applies. It depends only on the library; it is its own module.

The guide is [docs/plugins/pickers.md](../../docs/plugins/pickers.md); the runnable program is [examples/pickers-import](../../examples/pickers-import/).

```sh
go get github.com/ubgo/filesystem/contrib/pickers
```

## API

| Name | What it is |
|---|---|
| `Import(ctx, disk, src, pick, policy, fetch)` | Stats the pick, refuses a reported size over `policy.MaxSize`, streams it into `upload.Receive`; returns `upload.Result` |
| `Pick{ID, DriveID, Token}` | What the picker returned; prints and logs with the token redacted |
| `Source` | `Origins()`, `Stat`, `Open`: one vendor; implement it for another picker |
| `Authorizer` (`Authorize(h http.Header, token string)`) | an optional method of a `Source` whose vendor takes the token another way than `Authorization: Bearer`; it is called only for requests to the source's `Origins` |
| `GoogleDrive{APIURL, Export}` | Drive file IDs; Google-native documents are `CodeUnsupported` unless `Export` maps their type to a `DriveExport{MimeType, Extension}`: imported through `files.export` (a Doc as PDF or DOCX, at most 10 MB), the name given the extension, the bytes checked by the policy like any other |
| `Dropbox{APIURL, ContentURL}` | Dropbox `id:…` IDs; 409 is `CodeNotFound`, folders `CodeUnsupported` |
| `OneDrive{BaseURL}` | Graph item IDs, with `DriveID` for a SharePoint or shared drive |
| `File{Name, Size}` | A pick's metadata; `Size` -1 when unknown |

## Security

- Every request goes through `upload.FetchOptions.Client()`: connections to non-public addresses are refused after DNS resolution, redirects are capped. `FetchOptions.AllowAddr` is the escape hatch for a known internal server.
- The token is added by the client's transport to requests for the source's `Origins` only; any `Authorization` header on a request to another host (a redirect, a careless `Source`) is removed. OneDrive's download redirect to its pre-authenticated host gets no token.
- Pick IDs must match each vendor's ID alphabet (no `/`, no `.`), so a pick cannot steer a request to another API path.
- Transport errors keep only the scheme and host of a URL, never a path or query (OneDrive's pre-authenticated download URL carries its credential there), and vendor JSON answers are read up to a fixed cap.

## Tests

`go test ./...` imports from each source against an in-process stand-in for the three APIs, checks the policy applies (type from bytes, size refused before download), the refusals, the guarded client, token confinement with a second `Source` implemented outside the package, and redaction. Not verified without real accounts: each vendor's live answers.
