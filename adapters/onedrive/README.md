# adapters/onedrive

`github.com/ubgo/filesystem/adapters/onedrive` is the `filesystem.Driver` for Microsoft Graph drives: OneDrive (personal and for Business) and SharePoint document libraries. It speaks Graph REST as plain JSON on `net/http`, with no dependency beyond the library (the Graph Go SDK is several hundred generated packages; the driver needs eight requests). It is its own module.

The guide is [docs/adapters/onedrive.md](../../docs/adapters/onedrive.md).

```sh
go get github.com/ubgo/filesystem/adapters/onedrive
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Token` | `TokenSource` | required | Supplies the OAuth access token, once per request; `TokenFunc` adapts a function |
| `Drive` | `string` | `DefaultDrive` (`/me/drive`) | Graph path of the drive: `/drives/{id}`, `/users/{id}/drive`, `/sites/{id}/drive` |
| `Root` | `string` | drive root | Folder holding the disk, such as `Apps/Invoices` |
| `ChunkSize` | `int` | `DefaultChunkSize` (10 MiB) | Bytes per upload request: a multiple of `FragmentUnit` (320 KiB), at most `MaxChunkSize` (60 MiB); a write holds up to two chunks in memory (one for a body shorter than a chunk; for an upload session, the fragment being sent and the next, read ahead to tell whether it is the last) |
| `Client` | `*http.Client` | a client with `DefaultTimeout` (5 min) | Transport, proxy, TLS; the driver wraps its transport to add the token |
| `BaseURL` | `string` | `DefaultBaseURL` | Graph endpoint, for a national cloud or a test server; https, or http on loopback only |

`New(cfg)` makes no request. `Config` prints and logs with the token source redacted.

## Capabilities

`Move`, `DeletePrefix`, `ConditionalWrite` (`conflictBehavior=fail`), `CompareAndSwap` (`If-Match` on the item's eTag, for bodies shorter than `ChunkSize` only: Graph checks it when an upload session starts, not when it commits, so a longer body with `IfMatch` is `CodeUnsupported`), `CaseInsensitive`. Never: `Copy` (Graph copies asynchronously behind a monitor URL; the Disk streams instead), presigning (Graph's pre-authenticated URLs take no expiry), visibility, metadata, content type, headers, multipart.

## Behaviour

- Keys are paths under `Root`; `" * : < > ? \ | #` and a segment ending in `.` or a space are stored percent-escaped (`filesystem.KeyEscaper`) and decoded back. Graph creates missing parent folders on upload; `Move` creates them first.
- A body shorter than one chunk is one `PUT …:/content`. A longer one goes through an upload session whose last fragment commits the file; `Content-Range` states the total from `WriteOptions.SizeHint`, or `*` until the final fragment when the size is unknown. A failing body cancels the session.
- The token is added only to requests for the Graph origin: download redirects and session URLs (other hosts) never get it.
- `List` walks folders with `children` requests, following `@odata.nextLink` only within the Graph origin.
- Errors: 404 `CodeNotFound`, 409 `CodeExists`, 412 `CodeConflict` (and a swap of a missing item), 401/403 or a failing token source `CodePermission`, 413 `CodeTooLarge`, 507 `CodeQuotaExceeded`, 429/5xx `CodeUnavailable` with `Retry-After`.
- `Native()` returns the `*http.Client` (with the token transport).

## Tests

`go test ./...` runs the conformance suite twice (default and 320 KiB chunks) against an in-process fake of Graph with separate download and upload hosts that refuse any request carrying a token, plus session ranges and conditions, URL redaction in transport errors, the JSON size cap, cancellation, escaping, redaction and status mapping. Not verified without a real tenant: Graph's acceptance of `*` as the total in `Content-Range` for a stream of unknown size (pass `SizeHint` when the size is known), and its full list of refused names (`.lock`, `CON`, `desktop.ini`, names starting `~$`).
