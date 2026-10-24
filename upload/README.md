# upload

`github.com/ubgo/filesystem/upload` is how bytes arrive from browsers and phones: streaming server uploads, multipart form posts, presigned direct uploads confirmed by the server, browser multipart uploads in the protocol of Uppy's AWS S3 plugin, imports from a URL behind an SSRF guard, and sweeping of uploads nobody finished. Every path enforces the same `Policy` against the bytes actually stored.

The concepts, guarantees and examples are in [Uploads](../docs/concepts/uploads.md) and [Security](../docs/concepts/security.md).

The policy every example in the uploads guide uses:

<!-- ds:block id=policy-e9b5m967 -->
**policy** · [`examples/concepts-uploads/main.go:43-47`](../examples/concepts-uploads/main.go#L43-L47)

```go
var policy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"image/*", "application/pdf"},
	Key:     upload.RandomKey(uploadDir),
}
```
<!-- /ds:block hash=c35128 -->

## Policy

| Name | What it is |
|---|---|
| `Policy` | what an upload path accepts and where it stores it: `MaxSize`, `MinSize`, `Allowed`, `Denied`, `AllowActiveContent`, `AllowActive` (named active types accepted while every other protection stays on, such as SVG for `contrib/svg`), `Key`, `KeyContext`, `Visibility` (applied by `Receive` and Uppy, and by tickets only with `SignVisibility`), `AllowPublic` (lets a ticket request ask for a public object; off, such a request is `CodePermission`), `SignVisibility` (opt-in: `Presign` signs `Visibility` into every ticket whatever the client sends; a request for another visibility is `CodePermission`; needs a `Visibility`, and public still needs `AllowPublic`, else `Validate` answers `CodeInvalidArgument`), `Checksums` (more `filesystem.Checksum` digests beside SHA-256, reported in `Result.Checksums`: `Receive` has `Disk.Write` compute them, `Confirm` reads the stored object once, multipart included; a `Want` that does not match is `CodeInvalidArgument` in `Receive` and `CodeRejected`, object deleted, in `Confirm`; nil computes nothing), `Headers`, `Collision`, `Extensions`, `MatchExtension`, `Detector` (a `sniff.Detector` for formats the built-in detection does not know, asked only when the bytes come back octet-stream, its answer still checked by every rule), `Process`, `Inspect` |
| `Policy.Validate()` | checks the policy itself (a size, at least one type, well-formed extensions, a known visibility, not both `Key` and `KeyContext`); `CodeInvalidArgument` otherwise |
| `Policy.Accepts(contentType)` | whether the type is allowed and not denied; `CodeRejected` otherwise |
| `Policy.AcceptsName(name, contentType)` | the name rules (`DefaultDeniedExtensions`, `Extensions`, `MatchExtension`); `CodeRejected` otherwise |
| `DefaultDenied` | types refused unless `AllowActiveContent`: `sniff.ActiveTypes` plus Windows and Linux executables and WebAssembly |
| `DefaultDeniedExtensions` | file-name extensions refused in any dotted segment unless `AllowActiveContent`: server-side scripts and executables (`.php`, `.jsp`, `.exe`, `.htaccess`, …) |
| `KeyFunc` | `func(Info) (string, error)`: chooses a storage key |
| `KeyContextFunc`, `Policy.KeyContext` | `func(ctx, Info) (string, error)`: a key function that also gets the upload's context (the request's, for the handlers), so a key can follow the tenant a middleware stored on it without a `Resolver`; set `Key` or `KeyContext`, not both |
| `ShardedKey(dir, depth)` | a `KeyFunc` like `RandomKey` that splits the random name into `depth` levels of two-character directories (`photos/q3/7a/q37a….png`), so a local disk never holds one huge directory; `depth` in `[1, MaxShardDepth]` (4), otherwise every key fails with `CodeInvalidArgument` |
| `NameKey(dir)` | a `KeyFunc` storing under `dir` by the client's name made safe, for products where users pick names; pair it with `Policy.Collision` |
| `Collision` | what an upload does when its key holds an object: `CollisionOverwrite` (default), `CollisionFail` (`CodeExists`), `CollisionRename` (the first free `name (1).ext`); fail and rename write with `IfNotExists` and need `ConditionalWrite`; `Presign` refuses anything but overwrite |
| `FreeKey(ctx, disk, key)` | `key` when free, else the first free of `name (1).ext` … `name (100).ext` (`MaxRenameAttempts`), else `CodeExists`; it only looks, so write with `IfNotExists` |
| `RandomKey(dir)` | a `KeyFunc` storing under `dir` with 128 random bits and the detected type's extension; the default |
| `HeadersFunc` | `func(Info) (filesystem.HTTPHeaders, error)`: chooses the HTTP headers stored with a file (`Policy.Headers`); stored by `Receive` and `Uppy`, pinned in the signed PUT by `Presign` where the disk can (`CodeUnsupported` where it cannot); the disk needs `Capabilities.Headers` |
| `StaticHeaders(h)` | a `HeadersFunc` storing `h` with every file |
| `Attachment(cacheControl)` | a `HeadersFunc` storing `attachment; filename=<the client's safe name>` (RFC 2231 encoded when needed) and the given `Cache-Control` |
| `Info` | what a `KeyFunc`, `HeadersFunc` or `Processor` sees: `Filename` (made safe) and `ContentType` (detected) |
| `SafeFilename(name)` | a client's file name reduced to a safe display name: base name only, no control characters, at most 255 bytes, `file` when empty |
| `Processor` | `func(ctx, Info, io.Reader) (io.Reader, Info, error)`: transforms a file before it is stored (`Policy.Process`) |
| `Inspector` | `func(ctx, *filesystem.Disk, Result) error`: examines a stored file; an error deletes it (`Policy.Inspect`) |
| `Inspectors(list...)` | runs several inspectors in order, stopping at the first error; nil entries are skipped |

## Server-side uploads

| Name | What it is |
|---|---|
| `Receive(ctx, disk, body, filename, policy)` | stores one stream: detect, check, process, key, stream with the size cap, inspect |
| `Result` | a stored upload: `Key`, `Filename`, `ContentType`, `Size`, `SHA256`, `Checksums` (JSON tags `key`, `filename`, `content_type`, `size`, `sha256`, `checksums`; `Checksums` is nil and omitted unless `Policy.Checksums` asked for digests, which makes `Result` a struct to compare with `reflect.DeepEqual`, not `==`) |
| `Handler(disk, policy, HandlerOptions)` | a `multipart/form-data` POST handler; all files or none, or `Partial` |
| `HandlerOptions` | `Field` (default `DefaultField`), `MaxFiles` (default 1), `Partial`, `MaxTotal` (bytes of all files of one request together; `CodeTooLarge` beyond), `Resolve`, `OnStored`, `Errors` (an `fshttp.ErrorWriter` for every failed request; nil is `JSONErrors`), `Respond` (an `upload.SuccessWriter` for the success body; nil is `JSONSuccess`, unchanged) |
| `Resolver` | `func(r *http.Request, base Policy) (Policy, error)`: the policy for one request (per-plan limits, per-user keys), run before any byte of the body is read, so also the hook for authentication, quota and rate-limit refusals; its error is answered with its code's status, `fshttp.ClientMessage` and any `Retry-After`; a returned policy that fails `Validate` is answered 500 with the status text only |
| `DefaultField` | `"file"`, the form field `Handler` reads |
| `Response` | the success body: `files`, and `failed` with `Partial` |
| `FileFailure` | one file a `Partial` request did not store: `filename`, `error`, `code` |
| `ErrorResponse` | the body of a failed request: `error`, `code` |
| `SuccessWriter`, `SuccessBody`, `JSONSuccess`, `ChunkProgress` | `func(w, r, status, body SuccessBody)`: renders a successful response in the application's own envelope; `SuccessBody` is the closed set `Response`, `ChunkProgress`, `Ticket`, `Limits`; `Handler`, `PresignHandlerWith`, `Chunked` and `LimitsHandler` take one as `Respond`, and nil is `JSONSuccess`, byte for byte the default body. Uppy and tus bodies are protocol-fixed and have none |
| `JSONErrors` | upload's default `fshttp.ErrorWriter`, writing an `ErrorResponse` with `fshttp.ClientMessage`; every handler here takes an `Errors` option to answer in another envelope instead |

## Direct uploads

| Name | What it is |
|---|---|
| `TicketRequest` | what a client declares: `filename`, `content_type`, `size` |
| `Ticket` | an authorised direct upload: `key`, `method`, `url`, `headers`, `expires_at` |
| `Presign(ctx, disk, TicketRequest, policy, expires)` | checks the declaration and signs a PUT pinning the content type and the policy's headers (listed in `Ticket.Headers`, which the client must send); the disk needs `Presign` or a URL signer, and for headers `PresignHeaders` or a header-pinning signer |
| `Policy.PostForm` | `Presign` issues a presigned POST form (`Ticket.Fields`, method POST) through `Disk.PresignPost`: the backend enforces the exact declared size, the type and the key; needs `PresignPost` |
| `PresignHandler(disk, policy, expires)` | answers POSTed `TicketRequest` JSON with a `Ticket` |
| `PresignHandlerWith(disk, policy, expires, PresignHandlerOptions{Resolve, Idempotency, Errors, Respond})` | `PresignHandler` with a per-request `Resolver`, an `ErrorWriter` and a `SuccessWriter` |
| `TicketRequest` | `Filename`, `ContentType`, `Size`, an optional `SHA256` (hex; its form is checked) for `ConfirmExpected`, and an optional `Visibility` signed into the upload (public needs `Policy.AllowPublic`; the disk needs `PresignVisibility`, S3 and GCS in ACL mode, else `CodeUnsupported`; empty signs nothing, as before) |
| `ConfirmExpected(ctx, disk, key, filename, policy, Expected{Size, SHA256})` | `Confirm` that also refuses, deleting the object, a stored size or SHA-256 other than the declared one; zero fields are not checked |
| `Confirm(ctx, disk, key, filename, policy)` | checks the stored object (size, detected type, name, a stored, declared `Content-Type` that would make a browser render the bytes as an active or text type they are not (aliases such as `audio/wav` for `audio/wave` pass; disks without the `ContentType` capability are not compared), `Inspect`) and deletes it if refused; the client must send the `Content-Type` the `Ticket` carries, because a backend that cannot pin it (an Azure SAS) stores whatever was sent |
| `Chunked(disk, policy, ChunkedOptions{Staging, Owner, Fields, MaxChunkBytes, MaxUploadsPerOwner, AssemblyTimeout, StagingDisk, Resolve, OnStored, Now, Errors, Respond})` | Dropzone, FilePond and resumable.js chunk protocols: chunks in any order and retried, staged per owner and upload ID within the declared size, progress in a compare-and-swap manifest, assembled through `Receive` by the request that completes them (201; others 200 with progress); returns an error at setup when the staging disk lacks `ConditionalWrite` and `CompareAndSwap`; the per-owner cap is one compare-and-swap of an owner index, and an assembler taken over after `AssemblyTimeout` is fenced by a manifest generation; `Now` (nil means `time.Now`) is the clock claims are stamped and judged by, so takeover is testable with a fake clock |
| `ChunkFields`, `DropzoneFields()`, `ResumableFields()` | the form field names of a chunk library |
| `Idempotency{Store, Owner}`, `HandlerOptions.Idempotency`, `PresignHandlerOptions.Idempotency` | honours the `Idempotency-Key` header: a retry of the same request gets the first response (200, `Idempotent-Replayed: true`), the key reused for another request 422, a running twin 409, a failure is not recorded; keys scoped by owner |
| `IdempotencyStore`, `Recorded` (`Fingerprint`, `Body`, `ContentType`: a replay answers with the first response's type, `application/json` when none was kept; a store that persists `Recorded` keeps all three), `NewMemoryIdempotency(MemoryIdempotencyOptions{TTL, MaxEntries, Now})` | the response store (`Reserve`, `Complete`, `Release`) and the in-process one, bounded by TTL and entry count |
| `LimitsHandler(policy, LimitsOptions{MaxWidth, MaxHeight, Resolve, Errors, Respond})`, `LimitsOf`, `Limits` | the JSON limits a client follows to resize and filter files before uploading |
| `SafePath(name)`, `FolderKey(dir)`, `Result.Path`, `Info.Path` | folder uploads: the client's relative path made safe, and a `KeyFunc` keeping the layout under `dir` |
| `Policy.Progress`, `ProgressStep` | bytes received, reported about every 64 KiB and at the end |
| `ContentAddressed(dir, ContentAddressing{Owner, Shared})`, `Policy.Settle`, `Settler` | content-derived keys through the `Settle` hook (which runs after `Inspect` and may move the upload): duplicates share one object within one owner (`dir/<owner digest>/<sha256>.<ext>`) by default, across owners only with `Shared`, and only with the same visibility and headers |
| `Uppy(disk, policy, UppyOptions)` | the endpoints of Uppy's AWS S3 multipart plugin; the disk needs `Multipart` and `PresignPart`, otherwise every route answers 501 |
| `UppyOptions` | `Expires` (part URL lifetime, default 15 minutes), `Authorize(r, key)` (checks every request that continues an upload), `Resolve` (per-request policy; must give one upload the same limits at create and complete), `OnStored(ctx, r, Result)` (runs after the completed file passes `Confirm`, typically to create its record; its error deletes the file and fails the request), `Errors` (an `fshttp.ErrorWriter`; nil is `JSONErrors`). The complete request may carry `filename` beside `parts`; it is checked by the policy's name rules and reported in `Result.Filename` |

## Upload sessions

A server-issued token valid for a window (UP-15): one key or key prefix, limits no wider than the endpoint's policy, an expiry always judged on the server. Guide: [Upload sessions](../docs/concepts/uploads.md#upload-sessions-a-token-valid-for-a-window).

| Name | What it is |
|---|---|
| `NewSessions(store, SessionsOptions{Now})` | issues and redeems sessions over a `SessionStore`; a nil store is `CodeInvalidArgument` |
| `Sessions.Issue(ctx, policy, SessionRequest{Key\|Prefix, MaxSize, Allowed, TTL})` | records a session and returns `IssuedSession{Session, Token}`, the token shown only here; a request that widens the policy, has neither or both of `Key` and `Prefix`, or a TTL outside (0, `MaxSessionLifetime`] is `CodeInvalidArgument` |
| `Sessions.Redeem(ctx, token, policy)` | the policy narrowed to the session (size, types re-checked against the policy as it is now, key or prefix); any unusable token is one `CodePermission` error |
| `Sessions.Resolver()` | a `Resolver` reading `HeaderUploadSession` (`Upload-Session`), for `Handler`, `Chunked`, `Uppy`, `PresignHandlerWith` and `LimitsHandler` |
| `Sessions.Revoke(ctx, id)`, `Sessions.Sweep(ctx)` | end a session now; delete the expired ones (schedule it) |
| `Session`, `Session.Expired(now)` | what a store keeps: `ID`, `SecretHash` (SHA-256 of the secret, never the secret), `Key` or `Prefix`, `MaxSize`, `Allowed`, `CreatedAt`, `ExpiresAt` |
| `SessionStore`, `MemorySessions` | `SaveSession`, `LoadSession`, `DeleteSession`, `SweepSessions`, and the in-process store (zero value ready); `records/uploadsessions` is the database one |
| `SessionIDPrefix`, `MaxSessionLifetime`, `MaxSessionTypes` | `ups_`, 7 days, 32 |

## Imports from a URL

| Name | What it is |
|---|---|
| `FromURL(ctx, disk, rawURL, policy, FetchOptions)` | fetches with the SSRF guard and stores through `Receive` |
| `FromBase64(ctx, disk, encoded, filename, policy)` | decodes standard base64 while streaming and stores through `Receive`; invalid base64 is `CodeInvalidArgument` |
| `FromDataURI(ctx, disk, uri, filename, policy)` | the same for a base64 data URI; its declared type is ignored, non-base64 URIs are `CodeInvalidArgument` |
| `Promote(ctx, cache, cacheDir, key, store, storeKey)` | moves a cached upload to its permanent key (same disk: a move; another disk: copy, then delete); a key outside `cacheDir` is `CodePermission` and nothing moves; a root `cacheDir` is `CodeInvalidArgument` |
| `Fetch(ctx, rawURL, FetchOptions)` | the guarded fetch alone; the caller closes `Remote.Body` |
| `FetchOptions` | `Timeout`, `MaxRedirects` (negative follows none), `MaxSize` (refuses a larger announced length), `AllowAddr` (admits an address `PublicAddr` refuses), `Now` (the clock a `Retry-After` date is measured from; nil means `time.Now`), `Header` (sent with every request: a `User-Agent`, an `Authorization` for an internal source; Go drops credentials on a redirect to another host), `Proxy` (an `http://host:port` CONNECT proxy such as Smokescreen: the fetch still resolves the name, checks the address with `PublicAddr` and `AllowAddr`, and asks the proxy for that address, never the name, so the SSRF guard holds for the final destination) |
| `FetchOptions.Client()` | the guarded `*http.Client` itself (the address checked after DNS resolution on every connection, no environment proxy, `FetchOptions.Proxy` only through the guarded tunnel, bounded redirects), for any other request to an address a user chose, such as webhooks |
| `Remote` | a fetched response: `Body`, `Filename`, `Size` (`-1` when unknown), `URL` (the URL finally fetched, after redirects, for resolving relative links) |
| `PublicAddr(addr)` | whether an address is on the public internet; refuses loopback, private, link-local, CGNAT, multicast, reserved, NAT64 and IPv4-mapped forms |
| `DefaultFetchTimeout` | one minute, for the whole fetch |
| `DefaultMaxRedirects` | 5 |

## Housekeeping

| Name | What it is |
|---|---|
| `Sweep(ctx, disk, dir, cutoff)` | deletes every object under `dir` modified before `cutoff` and returns the count; the root is refused; deletes by age only |

## Errors

Every function returns `*filesystem.Error` codes: `CodeRejected` for a refused type, name or inspection; `CodeTooLarge` over a size limit; `CodeInvalidArgument` for an invalid policy, request, URL or address; `CodeNotFound` for a missing object (`Confirm`) or a 404/410 answer (`Fetch`); `CodeUnavailable` for network failures and 5xx answers; `CodePermission` when `UppyOptions.Authorize` refuses or an upload session token is unusable. Handlers map codes to statuses with `fshttp.StatusOf` and pass a backend's requested wait (`filesystem.Error.RetryAfter`) on as a `Retry-After` header. A JSON error message is the error's own cause, and only for codes about the client's input; the annotated text, which names the disk and the storage key, is never sent. See [Errors](../docs/concepts/errors.md).
