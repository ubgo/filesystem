# Error codes

Every error the library returns carries one `filesystem.Code` from a closed set of twelve. This page lists each code with its sentinel, its meaning, the HTTP status `fshttp.StatusOf` gives it, what produces it in each package and backend, and how each HTTP handler shows it to clients. It is generated from reading `errors.go`, `fshttp/fshttp.go` and every package that returns a code. For how to handle errors in application code, read [Errors](../concepts/errors.md) first.

## The codes

| Code | Constant | Sentinel | Meaning | `fshttp.StatusOf` | Retried by `layer.Retry` |
|---|---|---|---|---|---|
| `not_found` | `CodeNotFound` | `ErrNotFound` | the key, disk, file, attachment, upload ID or other named thing does not exist | 404 | no |
| `exists` | `CodeExists` | `ErrExists` | a conditional write found the key, or a name is already taken | 409 | no |
| `invalid_key` | `CodeInvalidKey` | `ErrInvalidKey` | the key is empty, longer than 1,024 bytes, not UTF-8, has a control character or a `..` segment, or is reserved by a driver | 400 | no |
| `invalid_argument` | `CodeInvalidArgument` | `ErrInvalidArgument` | an option, argument, DSN or request is invalid | 400 | no |
| `unsupported` | `CodeUnsupported` | `ErrUnsupported` | the backend lacks the capability and there is no portable fallback | 501 | no |
| `read_only` | `CodeReadOnly` | `ErrReadOnly` | a mutation reached a read-only disk | 403 | no |
| `too_large` | `CodeTooLarge` | `ErrTooLarge` | a body passed its size limit; nothing is stored | 413 | no |
| `rejected` | `CodeRejected` | `ErrRejected` | a policy refused the content; nothing is stored | 415 | no |
| `quota_exceeded` | `CodeQuotaExceeded` | `ErrQuotaExceeded` | the scope has no storage left | 507 | no |
| `permission` | `CodePermission` | `ErrPermission` | the backend, a guard, a hook or a signature refused the operation | 403 | no |
| `unavailable` | `CodeUnavailable` | `ErrUnavailable` | a transient failure: throttling, a timeout, a dropped connection, a service that is down | 503 | yes, when the operation is replayable |
| `conflict` | `CodeConflict` | `ErrConflict` | the operation was refused by the state the object or record is in now: it changed since it was read, or its state forbids the operation | 409 | no |
| `not_modified` | `CodeNotModified` | `ErrNotModified` | a conditional read found the object unchanged, so no body is returned; not a failure | 304 | no |
| `unknown` | `CodeUnknown` | `ErrUnknown` | the error could not be classified | 500 | no |

`filesystem.CodeValues` lists them in this order. `fshttp.StatusOf(nil)` is 200. A driver maps every native error into one of these; an error it cannot classify becomes `unknown`, never a new code.

## The error type

`*filesystem.Error` is the one error type the library returns:

| Field | Holds |
|---|---|
| `Code` | the code |
| `Op` | the operation kind (`write`, `stat`, …; [operations](operations.md)), empty outside a Disk operation |
| `Disk` | the disk's name |
| `Key` | the key, the directory of a list, or the source of a copy or move |
| `Err` | the cause, which may be the backend's native error; `errors.As` reaches it through `Unwrap` |

`Error()` renders `filesystem: <op> <disk>:<key>: <code>: <cause>`, leaving out the parts that are empty: `filesystem: stat media:reports/missing.txt: not_found: no object "reports/missing.txt"`.

Rules the functions follow:

- `errors.Is(err, filesystem.ErrNotFound)` is true for every `*Error` with that code anywhere in the chain, whatever its operation, disk or key.
- `filesystem.CodeOf(err)` is the code of the outermost `*Error` in the chain (an `unknown` one wrapping a context error reads as `unavailable`), the code of a bare sentinel, `not_found`, `exists` or `permission` for the `io/fs` sentinels, `unavailable` for `context.Canceled` and `context.DeadlineExceeded`, `unknown` for any other non-nil error, and `""` for nil. Wrapping a coded error in `fmt.Errorf("…: %w", err)` keeps its code.
- When a driver returns an error that is not a `*Error`, the Disk classifies it: `fs.ErrNotExist` becomes `not_found`, `fs.ErrExist` `exists`, `fs.ErrPermission` `permission`, anything else `unknown`. The original stays the cause.
- `filesystem.NewError(code, cause)` and `filesystem.Errorf(code, format, args…)` are how drivers, interceptors and plugins build coded errors.
- `*filesystem.BatchError` reports the keys a `DeleteKeys` could not delete: `Failures` is a list of `KeyError{Key, Err}`, every other key was deleted, and `errors.Is` and `CodeOf` see the per-key errors (`errors.Is(err, ErrPermission)` asks whether any key failed for lack of permission).

## What produces each code

### not_found

- **Disk:** `Stat`, `Read`, `ReadRange`, `ReadAll`, `Copy` and `Move` of a missing key; `SetVisibility` of a missing key; multipart calls with an unknown upload ID. `Exists` turns it into `false, nil`. `Delete` of a missing key is not an error.
- **Manager:** `Disk(name)` for an unregistered name, `Default()` with no disks, `SetDefault` of an unknown name.
- **Drivers:** S3 `NoSuchKey`, `NotFound` (a HEAD 404), `NoSuchUpload` and any 404; GCS `ErrObjectNotExist`, `ErrBucketNotExist` and 404; Azure `BlobNotFound`, `ContainerNotFound` and 404; local and SFTP a missing file, a path through a file (`not a directory`) and a key that is a directory; memory a missing key.
- **upload:** `FromURL` when the remote server answers 404 or 410.
- **records:** a file, attachment, folder, version, variant or share link that does not exist in the scope, and a file in the trash where a live one is needed (`file "…" is in the trash`); `quota.Usage` of a scope with no usage row.
- **share:** a link token that is unknown, expired or used up (`link not found or no longer valid`), deliberately the same answer for all three.

### exists

- **Disk:** `Write` with `IfNotExists` when the key exists (S3 `PreconditionFailed` and 412, GCS 412, Azure `ConditionNotMet`, `BlobAlreadyExists`, 409 and 412, local and SFTP an existing file, memory).
- **Mux and Manager:** `Register` of a scheme twice, `Add` of a disk name twice.
- **records:** `Attach` of a file already attached to the owner under the role; `Attach` to an occupied single role (use `SetAttachment`); a plugin installed twice; folders: a sibling with the same name.

### invalid_key

- **Disk:** every method, before the driver or any interceptor runs, through `filesystem.CleanKey`: empty after cleaning, more than `MaxKeyLength` (1,024) bytes, invalid UTF-8, a control character (NUL included), a `..` segment.
- **Drivers:** keys under a driver's reserved directory: `.filesystem` on local and SFTP (staging for atomic writes), `.filesystem-multipart` on Azure (multipart markers).

### invalid_argument

- **Disk construction:** an invalid disk name (`[A-Za-z0-9][A-Za-z0-9._-]{0,63}`), a nil driver, a driver that declares a capability it does not implement or `PresignPart` without `Multipart`, a nil interceptor, a nil URL signer, a public URL that is not absolute `http(s)`, an unknown default visibility.
- **Disk calls:** a nil body, a negative `MaxSize`, `SizeHint`, byte range or list limit, an unknown visibility, header values with CR or LF, a copy or move onto the same key, `DeletePrefix` of the root, a presign method other than GET or PUT, an expiry not in (0, 7 days], a part number outside 1 to 10,000, an empty upload ID, a completion with no parts, a retry of a write whose body is a one-shot stream.
- **Configuration:** a DSN that does not parse, a scheme with no opener, an unknown DSN parameter, a non-boolean or non-numeric value, a missing bucket, container, path, user or authentication, a password inside an SFTP DSN, `OpenManager` with no disks, an unset `…_DSN` variable, two disks reading one variable, a default disk not in the list; the error names the variable or parameter, never the DSN.
- **signedurl:** a base URL that is not absolute or has a query, no keys, a key shorter than 32 bytes, an empty or repeated key ID.
- **upload:** a policy without `MaxSize` or allowed types, with an unknown visibility or a malformed extension; a request that is not multipart, has more files than allowed or no file in the field; a ticket request that is not valid JSON or not POSTed; Uppy bodies that are not JSON, a missing `key` parameter, a part number that is not a number; `FromURL` of a non-HTTP URL, a URL with credentials, a private or loopback address, too many redirects, a 4xx answer other than 404, 410 and 429.
- **upload sessions:** a `SessionRequest` that would widen the policy (a `MaxSize` above it or below its `MinSize`, a type outside its `Allowed`), with neither or both of `Key` and `Prefix`, a malformed type pattern, more than `MaxSessionTypes` types, or a TTL outside (0, `MaxSessionLifetime`].
- **records quarantine:** a quarantined file recorded or made public, or a public file quarantined (`ErrPublicQuarantine`); an unknown `ScanFailure`; `QuarantineContent` on a change other than a replacement.
- **records and plugins:** an unregistered owner type, an owner ID or role of the wrong shape, an unknown `Previous` or visibility, a reorder that does not list each attachment once, an invalid cursor or page size, a migration edited after it ran, an invalid plugin name or a migration owned by another plugin; plugin-specific limits (tags: length and count; folders: names, depth, moving into itself, deleting a non-empty folder; versions: keeping fewer than one; share: lifetime and download count).
- **Drivers:** a local key whose parent path is a file; an S3 or Azure multipart completion whose parts are missing, out of order or carry the wrong ETag (S3 `InvalidPart`, `InvalidPartOrder`, `EntityTooSmall`).
- **contrib:** an imaging transform with an unknown mode, format or gravity, a size outside 1 to 8,192, a quality outside 0 to 100; tus without `Disk`, `TempDir` or `BasePath`; variants without transforms or with an undeclared name.

### unsupported

- **Disk:** a method whose capability is missing and has no fallback: `SetVisibility`, `SignedURL` (no `Presign` and no signer), `PublicURL` (no `PublicURL` and no configured base), the multipart methods; a write with `Metadata`, `Headers` or `IfNotExists` the driver cannot store or honour; a write whose visibility differs from the default on a driver without per-object visibility.
- **Mirror:** a signed PUT or presigned part on a mirrored disk (it would bypass the mirrors).
- **Drivers:** S3 `CreateMultipart` with `IfNotExists`; an SFTP server without the `posix-rename` extension.
- **upload:** every `Uppy` route on a disk without `Multipart` and `PresignPart`.
- **records:** an engine other than SQLite, Postgres and MySQL.

### read_only

- `filesystem.ReadOnly()` and the DSN parameter `readonly=true`: every operation with `Op.Mutating`, including a signed PUT URL. `Disk.Check` on a read-only disk reports it, because the check writes.

### too_large

- **Disk:** a body that passes `WriteOptions.MaxSize` while streaming, or a `SizeHint` above it; nothing larger is stored.
- **upload:** a file over `Policy.MaxSize` (form, import, tus, `Receive`), a request over the form cap, a ticket whose declared size is outside (0, `MaxSize`], a directly uploaded object larger than the policy at `Confirm`, a remote file that announces a size over the limit.
- **fshttp:** a signed PUT over `SignedOptions.MaxUploadBytes` (5 GiB by default).
- **scan:** a file larger than clamd's `StreamMaxLength`.
- **imaging:** an input over `Options.MaxBytes`.

### rejected

- **upload:** a detected content type not in `Policy.Allowed` or in `Policy.Denied` and the default denied list (HTML, SVG, XML, JavaScript, executables); a file name with a denied extension in any dotted segment (`photo.php.jpg`); an extension outside `Policy.Extensions`; with `MatchExtension`, a name whose extension claims another type than the bytes; a refusal from `Policy.Process` or `Policy.Inspect`.
- **scan:** an infected file (`scan: the file is infected (<signature>)`).
- **upload sessions:** a session whose types the endpoint's policy no longer allows (`the upload session's types are no longer allowed`).
- **imaging:** a file that is not a supported image, an image over the pixel limit, a crop outside the image; variants of a file that is not an image.

### quota_exceeded

- **records/quota:** an upload or replacement that would take the scope past its byte or file limit, refused inside the upload's transaction.

### permission

- **Drivers:** S3 `AccessDenied` and 403; GCS and Azure 401 and 403; local and SFTP file modes (`fs.ErrPermission`); SFTP authentication failure at open.
- **signedurl and fshttp:** a missing or bad signature, a wrong method, an unknown key ID, an expired link: one answer for all (`signed URL is invalid or expired`).
- **layer.Guard:** any refusal without a code of its own.
- **recordshttp:** a refusal by the `Scope` hook (answered 401) or the `Authorize` hook (403); **Uppy:** a refusal by `UppyOptions.Authorize` (403).
- **upload sessions:** a missing, malformed, unknown, revoked or expired `Upload-Session` token, or a wrong secret: one answer for all (`upload session is missing, unknown or expired`, 403).

### unavailable

- **Drivers:** S3 `SlowDown`, 429 and 5xx; GCS and Azure 429 and 5xx; an SFTP server that cannot be dialled (also when a dropped connection is redialled), and an SFTP connection lost during an operation (the next call reconnects when the disk has a dialer, as every disk opened from a DSN does; a disk built from a `Client` alone stays `unavailable` until reopened). An S3, GCS or Azure answer that carried `Retry-After` keeps it on the error (`filesystem.RetryAfterOf`), so `layer.Retry` waits at least that long and `fshttp` passes it on.
- **Any operation:** a context that ended, by deadline or cancellation, including an attempt cut short by `layer.Timeout`; `layer.Retry` retries it only while the caller's own context is alive.
- **layer.Limit:** a caller whose context ends while waiting for a slot.
- **Mirror:** a mirror that failed after the primary succeeded (the object exists on the primary only; retrying converges).
- **scan:** clamd unreachable, an unexpected reply or an error reply; any scanner error without a code of its own.
- **upload:** `FromURL` network failures, remote 5xx answers and a remote 429 (too many requests).
- **records:** another process holding the MySQL migration lock past `WithMigrationLockWait`; quota's counter row racing on first insert.

### conflict

Read the object or record again and decide; a blind retry meets the same state.

- **Compare-and-swap writes (`WriteOptions.IfMatch`):** the object changed, or is gone, since the caller read its ETag (memory, local, S3, GCS, Azure, sqlblob, Dropbox, OneDrive, Vercel Blob). Nothing is written.
- **Object state:** a read of an S3 or Azure object in an archive tier that is not restored (`s3.ErrArchived`, `azure.ErrArchived`); an Azure append to a blob that is not an append blob; a GCS storage-class change that raced; a Google Drive key naming two or more files.
- **Disk:** `ErrSuperseded`, a concurrent attempt that already delivered the result; `Sync` onto a case-insensitive disk where two source keys collide.
- **upload:** a chunked upload taken over by another request (`another request took over assembling this upload`); a request whose `Idempotency-Key` twin is still running; content addressing that finds identical content stored with other visibility or headers.
- **records:** `ErrChanged`, a change refused by `IfUnchanged` (recordshttp answers it 412, as a failed `If-Match`); `SetVisibility` in place on a file that shares its object with others; `ErrQuarantined`, a link, view, share, stream or public visibility asked for a quarantined file, or `SetStatus` asked to move one out of quarantine (recordshttp answers 409; share links answer the uniform 404); `ErrNotQuarantined`, `FailScan` for a file that is not quarantined.
- **records plugins:** locks: `ErrLocked`, a change to a file checked out by someone else; audit: `ErrChainBroken`, a hash chain that does not verify.
- **contrib:** archive: an object that changed while it was read; crypt: a key rotation that met a concurrent write.

### not_modified

- **Disk.ReadIf** (and the cache disk): `IfNoneMatch` matched or the object was not modified since `IfModifiedSince`. `fshttp` answers 304.

### unknown

- Any driver error the driver does not classify. A context deadline or cancellation is not here: it is `unavailable` (see above), and `errors.Is(err, context.Canceled)` and `context.DeadlineExceeded` still work on the cause.
- `Disk.Check` reading back different bytes than it wrote.
- An Azure server-side copy that ends in a failed state.
- folders: a subtree deeper than the limit or a cycle found in stored data.

## How handlers show errors to clients

| Handler | Status | Body |
|---|---|---|
| `fshttp.Signed`, `fshttp.Public` | `StatusOf(err)`; every signature failure is 403 | the status text only; a private and a missing key both answer 404 on `Public` |
| `upload.Handler`, `PresignHandler`, `Uppy` | `StatusOf(err)` | `{"error": "…", "code": "<code>"}`; the message is the error's own for `too_large`, `rejected`, `quota_exceeded`, `invalid_argument` and `invalid_key`, the status text for every other code |
| `upload.Handler` with `Partial` | 200 when some files were stored, else the first failure's status | `{"files": […], "failed": [{"filename", "error", "code"}]}` |
| `recordshttp.Handler` | `StatusOf(err)`, 401 for `Scope`, 403 for `Authorize`, 413 for the request cap | `{"error": "…", "code": "<code>"}`; client-visible messages as for upload, plus `exists` |
| `contrib/tus` | `StatusOf(err)` on the final `PATCH` (including an `OnStored` error); 415 for a refused declared type, 403 for `Authorize` (on any request), 400 for a deferred length, 413 for a declared length over `MaxSize` (from tusd) | `ERR_<CODE>: <message>` in tusd's plain-text format, the message shown for the same client-visible codes |

Driver messages, keys of other objects and hook errors never reach a client through these handlers.

Each of these shapes is a default. An `fshttp.ErrorWriter` in the handler's options (`Errors` on `fshttp.PublicOptions`, `SignedOptions`, `ZipOptions` and on every `upload` handler's options, `Responder{Errors}` in the framework adapters) renders the error instead, in any envelope and with any status; see [Your own error envelope](../concepts/errors.md#your-own-error-envelope).

Note on tus: an `OnStored` error keeps its code, as in `upload.Handler`: a `quota_exceeded` from a records upload in `OnStored` reaches a tus client as 507, and an error with no code as 500.

## Next

- [Errors](../concepts/errors.md): handling errors in application code.
- [Troubleshooting](troubleshooting.md): the error messages you are most likely to meet, and their fixes.
- [Operations and events](operations.md): the `Op` an error names.
