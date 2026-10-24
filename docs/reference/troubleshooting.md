# Troubleshooting

Real failure modes, found in the code, its tests, [COVERAGE.md](../../COVERAGE.md) and running the examples, grouped by where you meet them. Each entry gives the message or symptom, the cause, and the fix. Messages are shown as the library renders them; your disk names and keys will differ.

## Configuration and start-up

**`invalid_argument: disk media: unknown DSN parameter "endpiont" for scheme "s3"`**

A typo, or a parameter of another scheme. Every opener refuses parameters it does not know, on purpose. The valid ones: [DSN parameters](dsn.md).

**`invalid_argument: disk media: no opener registered for scheme "gs"`**

Nothing registers itself by being imported. Call `mux.Register(gcs.Scheme, gcs.Opener)` (and import the adapter's module) before `Open` or `OpenManager`. The `fs` CLI registers every bundled scheme: `file`, `mem`, `s3`, `gs`, `azblob`, `sftp`, `ftp`, `ftps`, `ftps-implicit`, `webdav`, `webdavs`, `cloudinary` and `vercelblob`.

**`STORAGE_ARCHIVE_DSN is not set (disk "archive")`, `STORAGE_DISKS lists no disks`, `STORAGE_DEFAULT_DISK names "x", which is not in STORAGE_DISKS`**

`OpenManager` checks every variable at start-up. The DSN variable is the disk name upper-cased with `-` and `.` as `_` (`media-cdn` reads `STORAGE_MEDIA_CDN_DSN`); two names that map to the same variable (`a-b` and `a.b`) are refused.

**`file DSN has a host "uploads"; use file:///absolute/path or file:relative/path`**

`file://uploads` has two slashes, which makes `uploads` a host. Use `file:///var/app/uploads` or `file:uploads`.

**`driver *x.Driver declares capability Copy but does not implement its interface`**

A custom driver claims a capability whose interface it lacks (or `PresignPart` without `Multipart`). `NewDisk` refuses it so the mistake fails at start-up. Implement the interface or drop the bit: [write a driver](../extending/write-a-driver.md).

**`invalid disk name "media files"`**

Disk names match `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`, because they appear in signed URL paths, logs and variable names.

**`signing key "k1" is 16 bytes, shorter than 32`**

`signedurl` keys must be at least 32 random bytes (`signedurl.MinKeyBytes`). Generate them with `crypto/rand`, store them as secrets, and keep old keys listed after the newest while links signed with them are alive.

**`sftp DSN needs known_hosts (or insecure_ignore_host_key=true for tests)`, `sftp DSN must not contain a password; use password_env`**

Host verification is required and secrets never go in the DSN. Point `known_hosts` at a file with the server's key, and put the password in an environment variable named by `password_env`, or use `identity`.

**`sftp: server lacks posix-rename@openssh.com, needed for atomic writes`**

The SFTP driver writes to a staging file and renames it into place, which needs the OpenSSH extension. OpenSSH and `pkg/sftp` servers have it; some appliance servers do not. `Config.AllowNonAtomicWrites` accepts such a server, at the cost of a replace that removes the old file before renaming the new one in.

**Opening an sftp, ftp or webdav disk on a write-only drop folder fails with a permission error**

The driver creates its staging directory under `Root` when it opens, and a drop folder (a bank or EDI inbox, a partner's upload box) refuses `mkdir` there. Set `Config.StagingPath` to an absolute directory outside `Root` on the same filesystem, so writes stay atomic and no key is reserved; if nothing outside the keys may be created either, set `Config.WriteInPlace` with `Config.AllowNonAtomicWrites`, accepting that a poller can see a file while it arrives ([capabilities](capabilities.md)).

**`azure: AZURE_STORAGE_CONNECTION_STRING has no AccountName and AccountKey`**

`azure.Opener` needs a shared-key connection string. For Azure AD or SAS-only credentials, build the `*azblob.Client` yourself and call `azure.New(azure.Config{Client, Container})`; without `SharedKey` the disk has no signed URLs.

**`records: migration legalhold/0001_legal_holds changed after it was applied`**

A migration's statements were edited after it ran. Restore the original statements and add a new migration for the change (`records.NewDDL(engine).AddColumn` for a column).

**`records: another process holds the migration lock`** (`unavailable`)

On MySQL, another instance was migrating for longer than `WithMigrationLockWait` (60 s by default). Run migrations as one deploy step, or raise the wait.

**`check disk media: write: …`** at start-up

`Disk.Check` (or `Manager.Check`) writes, reads back and deletes a probe under `.filesystem-check/`. The wrapped error says which step failed: wrong credentials (`permission`), a missing bucket (`not_found`), a read-only disk (`read_only`, because the check writes). A leftover `.filesystem-check/` object after a crash is safe to delete.

## Writes and keys

**`invalid_key: key contains a ".." segment`**

Keys are cleaned before any driver sees them, and `..` is refused rather than resolved, on every backend. Never build keys from client input except through a policy's `KeyFunc`; the client's file name is for display.

**`invalid_key: keys under ".filesystem" are reserved by the local driver`**

The local and SFTP drivers stage writes under `.filesystem`, and the Azure driver keeps multipart markers under `.filesystem-multipart`. Choose another prefix.

**`invalid_argument: a parent of "a/b" is a file, so the key cannot be created`**

On local disk, `a` is already a file, so `a/b` cannot exist. Object stores allow both keys; the local driver cannot. Keep files and "directories" from sharing names.

**`unsupported: driver *local.Driver does not store metadata`** (or `HTTP headers`)

Local and SFTP disks store neither; the write is refused rather than silently dropping them. Keep metadata in your database (records does), or use a backend that stores it ([capabilities](capabilities.md)).

**`unsupported: driver *s3.Driver stores no per-object visibility; disk default is private`**

The disk has no per-object visibility (an S3 bucket without `acl=true`, Azure, GCS without `acl=true`), and the write asked for one other than the default. Use a public disk and a private disk (`records.WithVisibilityDisks("cdn", "vault")`) instead of per-object visibility.

**`unsupported: driver *s3.Driver has no atomic conditional write`**

`IfNotExists` needs `conditional_write=true` on S3, set only for providers that honour `If-None-Match`; on one that ignores it, the "conditional" write would overwrite silently.

**`invalid_argument: write body is a stream and cannot be replayed`**

An interceptor (usually a hand-written retry) called a write twice whose body could not be rewound. `layer.Retry` never does this: it retries a write only when the body implements `io.Seeker`. Use `WriteBytes`, a `bytes.Reader` or a file to make a write retryable.

**A write from standard input or a socket is never retried (`attempts=1`)**

The body is an `*os.File` that is a pipe, socket or terminal (`os.Stdin` under `cmd | app`): it implements `io.Seeker` but cannot seek, so the Disk writes it once as a stream, and `layer.Retry` cannot replay it after a transient failure. Buffer it to a temporary file first, or redirect from a regular file (`fs put LOCATION < file`), when the write must be retryable.

**`too_large: body exceeds 10485760 bytes`**

The body passed `WriteOptions.MaxSize` (or an upload policy's `MaxSize`) while streaming; nothing was stored. A `SizeHint` above the limit is refused before reading (`declared size … exceeds …`).

**`invalid_argument: expiry 720h0m0s is not in (0, 168h0m0s]`**

Signed URLs live at most 7 days (`filesystem.MaxPresignExpiry`, S3's limit) and must have an expiry. A link that must live longer is a public file.

**`invalid_argument: refusing to delete the root of the disk`**, **`refusing to sweep the root of disk media`**

`DeletePrefix("")` and `upload.Sweep(ctx, disk, "", …)` are refused: emptying a whole disk is never a prefix operation. Name the directory.

## Uploads

**`rejected: content type "text/html; charset=utf-8" is not allowed`**

The type is detected from the bytes (and refined by the extension where that is safe), never taken from the client's header or file name. HTML, SVG, XML, JavaScript and executables are refused by every policy unless `AllowActiveContent` is set. Unknown binary formats detect as `application/octet-stream`; allow that type only if you mean it. Check what a file detects as with `sniff.Detect(head, name)`.

**`files named "photo.php.jpg" are refused: ".php" can be executed`**

Every dotted segment of a name is checked against `upload.DefaultDeniedExtensions`, because some servers execute `shell.php.jpg`.

**`the name says application/pdf but the content is image/png`**

`Policy.MatchExtension` refuses a name whose extension claims another type than the bytes. Turn it off if your users routinely misname files, or keep it to catch disguised uploads.

**A 413 for a file under `MaxSize`**

Something in front of the handler limits the body: fiber's `BodyLimit` (4 MiB by default, plain-text answer), echo's `BodyLimit` middleware, a proxy (`client_max_body_size` in nginx), or a load balancer. `upload.Handler` caps a request at `MaxFiles × MaxSize` plus 64 KiB, so a request with more files than the policy expects also hits it.

**`invalid_argument: no file in field "file"`**

The form field is not `file` (set `HandlerOptions.Field`), the part has no file name, or middleware consumed the body before the handler (`r.ParseMultipartForm`, `r.FormValue`, gin's `c.FormFile`, a body logger).

**Uppy answers 501 `disk bucket cannot sign multipart uploads`**

`upload.Uppy` needs `Multipart` and `PresignPart`: S3-compatible disks only. Use tus for resumable uploads elsewhere.

**Uppy part uploads fail in the browser with a CORS error, or `complete` gets no ETags**

The parts go straight to the bucket's origin. The bucket needs a CORS rule allowing `PUT` from your origin and exposing the `ETag` header.

**A tus upload answers 404, or `Location` URLs point at the wrong path**

`tus.Config.BasePath`, the mount pattern and the `http.StripPrefix` prefix must be the same string, trailing slash included (`/tus/`).

**tus logs `RequestIncoming`, `UploadCreated`, … at Info on every request**

That is tusd's request log. Set `tus.Config.Logger` to a `*slog.Logger` whose handler is at `slog.LevelWarn` (nil means `slog.Default()`) to keep only failures.

**A tus upload fails with 500 when `OnStored` refuses it**

The final `PATCH` answers the status of the `OnStored` error's code (`fshttp.StatusOf`), as `upload.Handler` does: a `quota_exceeded` is 507, a `rejected` 415. A 500 means the error carries no code (`unknown`); return a classified error (`filesystem.Errorf(filesystem.CodeX, …)`, or pass on the code of the records call that failed).

**Unfinished tus sessions fill the disk**

Sessions a client abandons stay in `tus.Config.TempDir`. Delete files older than your resume window from it on a schedule.

**`unavailable: scan: …` on every upload**

clamd (or your scanner) is down or unreachable, and scanning fails closed. Check `scan.Clamd{…}.Ping(ctx)`; if staying up matters more, wrap the scanner to fail open and alert ([write a scanner](../extending/write-a-scanner.md)).

**`too_large: scan: the file is larger than clamd's StreamMaxLength`**

Raise `StreamMaxLength` in `clamd.conf` above the policy's `MaxSize`, or lower `MaxSize`.

**`FromURL` refuses a URL with `invalid_argument`**

Imports refuse non-HTTP URLs, URLs with credentials, more redirects than allowed, and any connection to a non-public address (loopback, private networks, link-local, cloud metadata), checked at dial time so DNS rebinding cannot reach inside. A remote 4xx other than 404, 410 and 429 is also `invalid_argument`; 404 and 410 are `not_found`; 429 and 5xx are `unavailable`, worth retrying later.

## Serving files

**Signed links answer 403**

One answer covers every failure, so a forger learns nothing. In order of likelihood: the link expired; the signing key was removed from the signer's list (keep old keys until their links expire); the method differs (a GET link used for PUT); a signed PUT sent a different `Content-Type` than the one signed; the link was edited (a re-encoded query, a proxy rewriting the path).

**Signed links answer 404**

The prefix was not stripped (`fshttp.Signed` expects `/{disk}/{key}`), the signer's base URL is not where the handler is mounted, or the disk named in the link is not in the Manager passed to `fshttp.Signed`. A scoped disk signs with its own name, so add scoped disks to that Manager.

**`unsupported: driver *local.Driver has no presign capability`**

Local and memory disks need a signer: `filesystem.WithURLSigner(signer)` and `fshttp.Signed` mounted at the signer's base. GCS needs `signing_key`; Azure needs a shared key.

**`fshttp.Public` answers 404 for a file that exists**

It serves only public objects: the object's visibility must be public, or, on a disk without per-object visibility, the disk's default must be (`visibility=public`). A private and a missing key answer the same, on purpose.

**`PublicURL` answers `unsupported`**

Only two drivers build public URLs themselves: Vercel Blob on a public store and Cloudinary for the `upload` delivery type (see [capabilities](capabilities.md)). For every other driver, set the base with `public_url=` or `WithPublicURL`, usually your CDN.

**Downloads render HTML or SVG instead of downloading**

They do not when served by `fshttp` or by a signed link given its content type (`records.Store.URL` passes it): active content is forced to `attachment` and every `fshttp` response carries `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`. A public bucket URL serves what the bucket serves; keep active content off public disks.

**Range requests return the whole file through a proxy or middleware**

Compression middleware rewrites the body; exclude the download routes from it.

## Backends

**S3 `AccessDenied` (`permission`) on writes with `acl=true`**

The bucket has "bucket owner enforced" object ownership (the AWS default since 2023), which rejects ACLs. Remove `acl=true` and use bucket policies, or separate public and private buckets.

**SeaweedFS lists directories whose objects were deleted**

Filer-backed stores keep an empty directory after its last object goes. Add `prune_prefixes=true` to the DSN, at one extra request per directory listed.

**An `IfNotExists` write overwrote an object**

The provider ignores `If-None-Match` and the DSN still said `conditional_write=true`. Declare it only for providers that honour it (AWS, R2, recent MinIO, SeaweedFS); the conformance suite's `ConditionalWrite` case run against the provider shows which.

**MinIO for local development**

The MinIO server repository was archived in April 2026. The library's own S3 tests run on RustFS and SeaweedFS (`task test:rustfs`, `task test:s3`); Garage is another option.

**SFTP operations keep failing with `unavailable` after a disconnect**

A lost SFTP connection is `unavailable` for the calls in flight when it dropped, so `layer.Retry` retries them and `fshttp` answers 503. A disk opened from a DSN reconnects on the next call: it dials again with the same host, credentials and host-key check, re-checks the server's extensions, and carries on. If every call keeps failing, that redial is what fails: the server is unreachable (`unavailable`) or refuses the handshake (`permission`), so read the error rather than reopening. A disk built with `sftp.New` from a `Client` alone has nothing to redial with; give `Config.Dial` a function that opens a new connection, or reopen the disk. The driver never repeats an operation itself, so wrap the disk in `layer.Retry` to retry the one that failed.

## Records

**`records: owner type "post" is not registered (WithOwnerTypes)`**

Every owner type is declared up front: `records.WithOwnerTypes("user", "post")`.

**`records: user "42" already has a file as "avatar", which holds one (use SetAttachment)`** (`exists`)

`avatar` is a single role (`WithSingleRoles`). `Attach` never replaces; `SetAttachment` does, and decides what happens to the old file.

**`database is locked` on SQLite**

Several connections are writing. Open SQLite with `?_pragma=busy_timeout(5000)` and `db.SetMaxOpenConns(1)`. An in-memory database (`:memory:`) needs `SetMaxOpenConns(1)` anyway: every new connection would open an empty database of its own.

**Objects exist that no row refers to**

The Store writes the object before the row and deletes the row before the object, so a crash or a failed cleanup can leave an object, never a row without one. Schedule `Store.SweepOrphans(ctx, disk, prefix, olderThan)`: it deletes only the objects no `fs_files` row names and no plugin holds, older than the cutoff. Do not point `upload.Sweep` at recorded files: it deletes by age, recorded or not. [Consistency](../concepts/consistency.md) explains the order.

**`SweepTrash` reports `permission` for the same file every run**

A plugin's `BeforePurge` refuses one trashed file. The sweep purges the others and names the refused file in its error, every run. Restore the file or lift the plugin's reason; a plugin that refuses purges should keep its files out of the trash ([write a records plugin](../extending/write-a-records-plugin.md)).

## Operations and errors

**Client disconnects show up as 500s**

A cancelled request context classifies as `unavailable`, so `fshttp.StatusOf` answers 503 and availability alerts fire for clients that simply went away. Filter with `errors.Is(err, context.Canceled)` before counting.

**Retries never happen**

`layer.Retry` retries only `unavailable` errors on replayable operations. A write of a one-shot stream, a move and a multipart completion are never retried, and an error a driver cannot classify is `unknown`. A context deadline or cancellation is `unavailable`, but Retry stops as soon as the caller's own context has ended. Widen `RetryOptions.Retryable` deliberately, not to "everything".

**A metric series per tenant**

A scoped disk reports its own name, and scoped disks named per tenant multiply `filesystem.disk` label values. Name scoped disks per feature, or map names to a bounded set in your own `layer.Observe` callback ([OpenTelemetry](../integrations/otel.md)).

## Next

- [Error codes](errors.md): every code and its producers.
- [Capabilities](capabilities.md): what each backend can and cannot do.
- [DSN parameters](dsn.md): every parameter of every scheme.
- [Feature index](features.md): every feature and the guide and example for it.
