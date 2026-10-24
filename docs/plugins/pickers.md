# Cloud picker imports

Users often have the file they want to upload in Google Drive, Dropbox or OneDrive rather than on the device. Each vendor offers a browser picker (the Google Picker, the Dropbox Chooser, the OneDrive and SharePoint file picker) that returns the chosen file's ID and an access token. `contrib/pickers` takes those two values on the server, fetches the file from the vendor's API, and stores it through `upload.Receive`, so the same policy that guards direct uploads (size, type detected from the bytes, names, keys, hooks) guards imports.

## Before you start

- The package is `github.com/ubgo/filesystem/contrib/pickers`, a module of its own that depends only on the library. Its reference is [contrib/pickers/README.md](../../contrib/pickers/README.md).
- The runnable program is [`examples/pickers-import`](../../examples/pickers-import/): a stand-in for the Drive API and an application in-process, so `go run .` needs no account.
- The picker runs in the browser with your app's OAuth client: the Google Picker with the `drive.file` scope, the OneDrive picker with `Files.Read` (or `Files.Read.All` for shared files). The Dropbox Chooser returns links rather than tokens; import by file ID with the user's token from your app's own Dropbox OAuth instead.
- Tested against an in-process stand-in for the three APIs; each vendor's live answers need real accounts.

## 1. Import a pick

The browser posts the pick to your server; the handler hands it to `Import` with the source and the policy (`pdfPolicy`, the same policy a direct upload uses).

<!-- ds:block id=importhandler-nvdxbkbu -->
**importHandler** · [`examples/pickers-import/main.go:84-101`](../../examples/pickers-import/main.go#L84-L101)

```go
func importHandler(disk *filesystem.Disk, src pickers.Source, fetch upload.FetchOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, maxPickBody)).Decode(&in); err != nil {
			http.Error(w, "bad pick", http.StatusBadRequest)
			return
		}
		res, err := pickers.Import(r.Context(), disk, src, pickers.Pick{ID: in.ID, Token: in.Token}, pdfPolicy, fetch)
		if err != nil {
			http.Error(w, string(filesystem.CodeOf(err)), http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(imported{Key: res.Key, Filename: res.Filename, Type: res.ContentType, Size: res.Size})
	})
}
```
<!-- /ds:block hash=3dcd82 -->

`Import` asks the vendor for the file's name and size first and refuses a size over `Policy.MaxSize` before downloading anything; then it streams the bytes into `upload.Receive`, which detects the type, applies every policy rule and stores the file. The result is the same `upload.Result` a direct upload returns.

The three sources:

- `pickers.GoogleDrive{}`: `Pick.ID` is the Drive file ID. Google Docs, Sheets and other native files have no bytes and are `CodeUnsupported`.
- `pickers.Dropbox{}`: `Pick.ID` is the Dropbox file ID (`id:…`). A missing file is `CodeNotFound`, a folder `CodeUnsupported`.
- `pickers.OneDrive{}`: `Pick.ID` is the item ID and `Pick.DriveID` the drive the picker reports (a SharePoint library or another user's drive); empty means the token owner's own drive.

Each has an endpoint field for a national cloud or a test server. A picker of another vendor is a `Source` of your own: `Origins()` names the hosts the token may go to, `Stat` returns the name and size, `Open` streams the bytes. The example's `source.go` writes one for a Box-like API:

<!-- ds:block id=boxsource-v6c3k9wn -->
**boxSource** · [`examples/pickers-import/source.go:29`](../../examples/pickers-import/source.go#L29-L29)

```go
type boxSource struct{ API string }
```
<!-- /ds:block hash=061fa7 -->

<!-- ds:block id=boxstat-q2w7n4ck -->
**boxSource.Stat** · [`examples/pickers-import/source.go:42-56`](../../examples/pickers-import/source.go#L42-L56)

```go
func (b boxSource) Stat(ctx context.Context, c *http.Client, p pickers.Pick) (pickers.File, error) {
	if !boxIDPattern.MatchString(p.ID) {
		return pickers.File{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "box: malformed file ID")
	}
	resp, err := get(ctx, c, b.API+"/files/"+p.ID)
	if err != nil {
		return pickers.File{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var meta boxFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBoxMeta)).Decode(&meta); err != nil {
		return pickers.File{}, filesystem.Errorf(filesystem.CodeUnavailable, "box: unreadable metadata")
	}
	return pickers.File{Name: meta.Name, Size: meta.Size}, nil
}
```
<!-- /ds:block hash=9b616a -->

`Origins`, `Stat` and `Open` are its methods. Each checks the ID against the vendor's alphabet before building a URL (the test `TestOwnSource` sends `../admin` and gets `invalid_argument`), sends one GET through the client `Import` passes, and maps the vendor's answers to the library's codes (404 to `not_found`, 401 and 403 to `permission`, anything else to `unavailable`). Never set an `Authorization` header in a source: the client adds the pick's token for the source's `Origins` and strips it everywhere else.

### Google Docs as PDF, and other auth schemes

A Google-native file has no bytes and is `CodeUnsupported` by default. Name the formats to export them as:

```go
drive := pickers.GoogleDrive{Export: map[string]pickers.DriveExport{
	"application/vnd.google-apps.document":     {MimeType: "application/pdf", Extension: ".pdf"},
	"application/vnd.google-apps.spreadsheet":  {MimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Extension: ".xlsx"},
}}
res, err := pickers.Import(ctx, disk, drive, pick, policy, upload.FetchOptions{})
```

The file is fetched through Drive's `files.export` (at most 10 MB) and named with the extension; the policy still detects the type from the bytes. A native type the map does not name stays `CodeUnsupported`.

A source whose vendor does not take `Authorization: Bearer` implements `pickers.Authorizer`; the method is called only for requests to the source's `Origins`, and other hosts still get no credential:

```go
func (s mySource) Authorize(h http.Header, token string) { h.Set("X-Api-Token", token) }
```

## Try it

```sh
cd examples/pickers-import
go run .
go test .
```

```text
POST /import/drive -> 200 {"filename":"invoice-2026-10.pdf","key":"….pdf","size":28,"type":"application/pdf"}
box import: contract.pdf application/pdf 29 bytes
box import with token "exp…": permission
```

## 2. Security

The token is the user's credential and the vendor's answers are untrusted input, so:

- Every request goes through the guarded client of `upload.FetchOptions` (see [importing from a URL](../recipes/import-from-url.md) for the guard): connections to loopback, private, link-local and other non-public addresses are refused after DNS resolution, so neither a configured endpoint nor a redirect reaches an internal service. `AllowAddr` is the one escape hatch, for a known internal server.
- The client's transport adds the token only to requests for the source's `Origins`, and removes any `Authorization` header from a request to another host, whoever set it. OneDrive answers a download with a redirect to a pre-authenticated host, which never receives the token.
- Pick IDs must match each vendor's ID alphabet, which holds neither `/` nor `.`, so a crafted ID such as `..` cannot steer a request to another path of the vendor's API.
- A failed request's error keeps only the scheme and host of the URL, so a pre-authenticated download URL never reaches a log, and vendor JSON answers are read up to a fixed cap.
- `Pick` prints and logs with the token redacted.

## 3. Errors

A missing file is `CodeNotFound`, a refused or expired token `CodePermission`, a file over the limit `CodeTooLarge` (before the download when the vendor reports the size, while streaming otherwise), a type the policy refuses `CodeRejected`, a malformed pick `CodeInvalidArgument`, a native document or folder `CodeUnsupported`, and a vendor that throttles or fails `CodeUnavailable`.

## Pitfalls

- **Google Docs, Sheets and Slides cannot be imported.** They are native documents without bytes, so the import is `unsupported`. Tell the user to download them as PDF or Office files first, or export them through the Drive API in a source of your own.
- **The picker's token is short-lived.** Pickers hand the browser an access token that expires within the hour. Send it to your server with the pick and import at once; queueing the import for later fails with `permission`.
- **The vendor's size is a claim.** A file is refused before the download when the vendor reports a size over the limit, and while streaming otherwise. A policy's `MaxSize` is enforced on the bytes either way.
- **Import stores bytes; it does not record them.** `pickers.Import` returns an `upload.Result`. To get a row, call `records.Store.CreateFile` with its key after the import; the example stops at the stored object.
- **OneDrive files in a shared library need `DriveID`.** Without it the item is looked up in the token owner's own drive and is `not_found`.
- **Never log the pick.** `Pick` redacts its token when printed, but a request body you log yourself does not.
