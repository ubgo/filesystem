# adapters/dropbox

`github.com/ubgo/filesystem/adapters/dropbox` is the `filesystem.Driver` for Dropbox, over the HTTP API v2 as plain JSON on `net/http`, with no dependency beyond the library (the generated Go SDK is large and unmaintained, and the driver needs eleven endpoints). It is its own module.

The guide is [docs/adapters/dropbox.md](../../docs/adapters/dropbox.md).

```sh
go get github.com/ubgo/filesystem/adapters/dropbox
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Token` | `TokenSource` | required | Supplies the OAuth access token, once per request; `TokenFunc` adapts a function (and so an `oauth2.TokenSource`) |
| `Root` | `string` | account (or app folder) root | Folder holding the disk, starting with `/` |
| `ChunkSize` | `int` | `DefaultChunkSize` (8 MiB) | Bytes per upload request, at most `MaxChunkSize` (150 MiB); one chunk is held in memory per write |
| `Client` | `*http.Client` | a client with `DefaultTimeout` (5 min) | Transport, proxy, TLS; the driver's copy follows no redirect |
| `APIURL`, `ContentURL` | `string` | `DefaultAPIURL`, `DefaultContentURL` | API hosts, for a proxy or a test server; https, or http on loopback only |
| `RootNamespaceID` | `string` | none | Resolves paths against the team space (Dropbox Business): `Dropbox-API-Path-Root {".tag":"root"}` on every request; take it from `users/get_current_account` |
| `NamespaceID` | `string` | none | Resolves paths against any reachable namespace (a team or shared folder); exclusive with `RootNamespaceID` |
| `SelectUser` | `string` | none | The team member ID a team-scoped token acts as (`Dropbox-API-Select-User`) |

`New(cfg)` makes no request; both namespace fields together are `CodeInvalidArgument`. `Config` prints and logs with the token source redacted, and the driver keeps only the source's method, so printing either never shows a token.

## Capabilities

`Copy`, `Move`, `DeletePrefix`, `ConditionalWrite` (write mode `add`), `CompareAndSwap` (mode `update` with `strict_conflict`; the ETag is the rev), `CaseInsensitive`. Never: presigning (temporary links last four hours and cannot be shortened), visibility, metadata, content type, headers, multipart.

## Behaviour

- Keys are paths under `Root`; a segment ending in `.` or a space is stored percent-escaped (`filesystem.KeyEscaper`) and decoded back.
- A body shorter than one chunk is one `files/upload`; a longer one goes through an upload session committed by one `finish`, so a reader never sees a partial file and a failing body stores nothing.
- `Copy` and `Move` onto an existing destination never lose it when a step fails. Dropbox has no overwriting copy or move, so a copy first goes to a transient sibling named with `SwapPrefix` (`.fs-swap-`), then the old destination is moved aside, the new file moved in, and the old one deleted; if moving in fails, the old destination is moved back. What stays non-atomic: a reader can briefly miss the destination between the two moves, a listing during the operation can show the transient files, and if a rollback itself fails the old content is kept at the `SwapPrefix` name the error reports (an application can sweep such files). A destination that does not exist costs one call, which fails rather than overwrite a file that appears meanwhile.
- `List` is one recursive `files/list_folder` following its cursor; keys keep the casing Dropbox reports (`path_display`).
- Errors: `not_found` → `CodeNotFound`, `conflict` → `CodeExists` (`CodeConflict` for a failed swap), `insufficient_space` → `CodeQuotaExceeded`, `malformed_path` → `CodeInvalidKey`, 401/403 or a failing token source → `CodePermission`, 429/5xx/`too_many_*` → `CodeUnavailable` with `Retry-After`.
- `Native()` returns the `*http.Client`.

## Tests

`go test ./...` runs the conformance suite twice (default and 1 KiB chunks) against an in-process fake of the documented endpoints, plus chunking, redirects, escaping, redaction and status mapping. `deps_test.go` keeps production code to the standard library and the library. Not verified without a real account: Dropbox's exact refusal of a name, and its Unicode normalisation of names.
