# contrib/tus

`github.com/ubgo/filesystem/contrib/tus` serves resumable uploads over the [tus](https://tus.io) protocol, the one mobile apps and uploaders such as Uppy use to survive dropped connections, by embedding [tusd](https://github.com/tus/tusd) rather than reimplementing the protocol. It is its own module, so tusd is downloaded only by apps that use it.

```sh
go get github.com/ubgo/filesystem/contrib/tus
```

tusd keeps each unfinished upload in a local temporary directory. When the last byte arrives, and before the client is told it succeeded, the file goes through the same `upload.Policy` as every other upload path (type detected from the bytes, size cap, `Process`, `Inspect`) into the target disk, and `OnStored` runs. If any of that fails, the client's final request fails with a matching status, nothing stays on the disk, and the session is deleted. The stored key comes back in the `Filesystem-Key` header.

Guides: [resumable uploads](../../docs/recipes/resumable-uploads.md) for the whole flow, [net/http](../../docs/integrations/net-http.md) for mounting it next to the other handlers (with a curl transcript).

## API

| Name | What it is |
|---|---|
| `New(cfg Config) (http.Handler, error)` | the tus handler; validates the config and the policy and creates `TempDir` (when `Store` is not set); mount it under `BasePath` with that prefix stripped |
| `Config.Disk` | the disk finished uploads are stored on; required |
| `Config.Policy` | the `upload.Policy`; checked against the declared type at creation (the client's word), enforced on the bytes at the end; required |
| `Config.TempDir` | a local directory for unfinished sessions (tusd's filestore) and their locks (tusd's filelocker, which keeps two `PATCH` requests from writing one upload at once), used for nothing else; required unless `Store` is set; abandoned sessions stay there until `Sweep` deletes them |
| `Config.Store` | optional `SessionStore` (a tusd data store that can terminate uploads: `s3store`, `gcsstore`, `azurestore`, or `filestore` on a shared mount) keeping unfinished sessions instead of the filestore in `TempDir`, so several instances without sticky sessions can each continue an upload; needs `Locker`; `Sweep` does not reach it, so expire its sessions with the backend's lifecycle rules |
| `Config.Locker` | optional `handler.Locker` locking each upload while a request works on it, instead of the filelocker in `TempDir`; required with `Store`, and shared by every instance when the store is (a Redis or etcd locker) |
| `Config.Cors` | optional `*handler.CorsConfig`; nil means `DefaultCors()`: tusd's default (every origin, no credentials) with `Upload-Session` (`HeaderUploadSession`) added to the allowed headers, which an upload session's client sends with every request. Start from `DefaultCors()` to restrict `AllowOrigin` or allow credentials and keep the headers tus needs; `Disable` leaves CORS to a proxy |
| `Config.BasePath` | the path the handler is mounted at, such as `/uploads/`; tusd builds upload URLs from it; required |
| `Config.Resolve` | optional; an `upload.Resolver` choosing each upload's policy from `Policy`, at creation (declared size and type) and at the final `PATCH` (the bytes), so an upload session (`upload.Sessions.Resolver()`) applies its prefix and limits; it must give one upload the same policy both times and may only narrow `Policy`; its error answers its code's status, an invalid resolved policy 500 |
| `Config.Authorize` | optional; runs on every request but CORS preflights (creation, `PATCH`, `HEAD`, `DELETE`) with its context and headers, and an error refuses the request with 403 |
| `Config.AuthorizeUpload`, `UploadRequest{Method, Header, UploadID, Metadata}` | optional; runs after `Authorize` on every request but preflights with the upload it names and its stored metadata (on creation, ID `""` and the declared metadata); an error is 403. The per-upload ownership check: an upload whose metadata cannot be read is 403 without calling it |
| `Config.StampMetadata` | optional; at creation returns metadata the server sets over the client's keys of the same name (the owner's ID, for `AuthorizeUpload`); an error refuses the creation with 403 |
| `Config.OnStored` | optional; runs after the file is stored with the `upload.Result` and the client's metadata, typically to record and attach it; an error deletes the stored object and fails the final request with the status of its code |
| `Config.Logger` | optional `*slog.Logger` for tusd's own logs (a line per request at Info, failures at Error); nil means `slog.Default()` |
| `Config.RespectForwardedHeaders` | optional; builds the `Location` of new uploads from `X-Forwarded-Proto`, `X-Forwarded-Host` and `Forwarded`, for a handler behind a TLS-terminating proxy; enable it only when the proxy sets or strips those headers |
| `Sweep(ctx, dir, olderThan) (int, error)` | deletes the sessions in `dir` (a `TempDir`) that received nothing since `olderThan` and returns how many; see [Abandoned sessions](#abandoned-sessions) |
| `HeaderKey` | `Filesystem-Key`, the response header carrying the stored key on the final `PATCH` |
| `HeaderUploadSession`, `DefaultCors() *handler.CorsConfig`, `SessionStore` | the upload session header, the default CORS policy, and the interface `Config.Store` takes |
| `MetaFilename`, `MetaFiletype` | `filename` and `filetype`, the `Upload-Metadata` keys read (Uppy and the official tus clients send them) |

With `BasePath` set to `/uploads/`, mount the handler at the pattern `/uploads/` and strip the prefix `/uploads/`, trailing slash included, with `http.StripPrefix`; the three strings must match.

## Responses

| Situation | Status | Body |
|---|---|---|
| upload created | 201, `Location` | |
| `Authorize` refused (any request) | 403, with tusd's CORS headers | `ERR_FORBIDDEN: upload not authorised` |
| `AuthorizeUpload` or `StampMetadata` refused, or the upload's metadata unreadable | 403, with tusd's CORS headers | `ERR_FORBIDDEN: upload not authorised` |
| no `Upload-Length` (deferred length) | 400 | `ERR_LENGTH_REQUIRED: Upload-Length is required` |
| declared length over `Policy.MaxSize` | 413 | tusd's own answer |
| declared `filetype` refused by the policy | 415 | `ERR_REJECTED: <message>` |
| final `PATCH`, file stored | 204, `Filesystem-Key` | |
| final `PATCH`, the bytes refused (type, size, `Process`, `Inspect`) | `fshttp.StatusOf` of the error | `ERR_<CODE>: <message>`; the message for client-visible codes (`too_large`, `rejected`, `quota_exceeded`, `invalid_argument`, `invalid_key`), the status text otherwise |
| final `PATCH`, `OnStored` failed | `fshttp.StatusOf` of its error (507 for `quota_exceeded`, 500 with no code) | `ERR_<CODE>: <message>`, the message for client-visible codes as above |

Downloads through the tus handler are disabled; serve stored files with `fshttp` or signed URLs.

## Abandoned sessions

The handler deletes a session when its upload finishes, whether the file was stored or refused. A session a client starts and never finishes stays in `TempDir` forever: tusd's file store has no expiry. Call `tus.Sweep(ctx, cfg.TempDir, time.Now().Add(-24*time.Hour))` from a scheduled job (hourly is typical) on every instance that has a `TempDir`.

- A session is idle from the later modification time of its two files: `<id>`, the bytes so far, which every `PATCH` appends to, and `<id>.info`, its state, written when it is created. The `.info` time alone is the creation time, so sweeping file by file would break a slow upload that is still alive.
- Only files named by a tusd session ID (32 lower-case hex digits, with or without `.info`) are touched; anything else in the directory is left alone. A data file whose `.info` was never written (a crash during creation) is swept too, and so are a swept session's filelocker files (`<id>.lock`, `<id>.stop`), which a crash can leave behind.
- A client that resumes a swept session gets `404` and starts again, which the tus clients do. Choose the age from how long your clients realistically pause (an app in the background, a phone offline overnight).
- A file that cannot be deleted is reported in the joined error and retried by the next run; the count covers the sessions that were deleted. A `TempDir` that does not exist yet is not an error.

## Limits

- `Authorize` checks who is calling, from the request's context and headers; it does not know which user created an upload. The upload URL, unguessable, is what ties a session to its creator.
- Sessions live in a local directory (tusd's file store); there is no option yet to keep them in a shared store such as S3, so a cluster needs sticky routing or a shared mount (below).
- The temporary directory must be on local disk of the instance that receives every request of an upload; behind a load balancer, route an upload's requests to one instance or share the directory.
