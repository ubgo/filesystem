# Errors

Every error the library returns carries a `Code` from one closed set, whichever backend produced it. Application code branches on the code and never on an AWS, Google or Azure error type, which is what lets you move from local disk to S3 without touching error handling. This page covers the codes, the sentinels for `errors.Is`, the fields of `*filesystem.Error`, reaching the backend's native error, batch failures, and the HTTP status each code maps to.

The snippets come from `examples/concepts-errors`. Run it with `go run ./concepts-errors` from the `examples` directory.

## The codes

| Code | Sentinel | Meaning | HTTP (`fshttp.StatusOf`) |
|---|---|---|---|
| `not_found` | `ErrNotFound` | the key does not exist | 404 |
| `exists` | `ErrExists` | an `IfNotExists` write found the key, or a name is already registered | 409 |
| `invalid_key` | `ErrInvalidKey` | the key is empty, too long, not UTF-8, has a control character or a `..` segment | 400 |
| `invalid_argument` | `ErrInvalidArgument` | another argument or option is wrong: an unknown visibility, a bad expiry, a typo in a DSN | 400 |
| `unsupported` | `ErrUnsupported` | the backend lacks the capability and there is no fallback | 501 |
| `read_only` | `ErrReadOnly` | a mutation reached a read-only disk | 403 |
| `too_large` | `ErrTooLarge` | a write passed `MaxSize`, or an upload its policy's limit; nothing is stored | 413 |
| `rejected` | `ErrRejected` | a policy refused the content: its type, its name, a scan; nothing is stored | 415 |
| `quota_exceeded` | `ErrQuotaExceeded` | the scope has no storage left (records quota plugin) | 507 |
| `permission` | `ErrPermission` | the backend refused: credentials, ACL, file mode, a guard, a bad signature | 403 |
| `unavailable` | `ErrUnavailable` | a transient failure: throttling, a reset connection, a scanner that is down; the only code `layer.Retry` retries by default | 503 |
| `conflict` | `ErrConflict` | a compare-and-swap write (`WriteOptions.IfMatch`) found the object changed or gone since it was read; nothing is written | 409 (once fshttp maps it) |
| `not_modified` | `ErrNotModified` | a conditional read (`Disk.ReadIf`) found the caller's copy current; not a failure, there is no body | 304 (once fshttp maps it) |
| `unknown` | `ErrUnknown` | the driver could not classify the error | 500 |

`filesystem.CodeValues` lists them all. A driver maps every native error into one of these; an error it cannot classify becomes `unknown`, never a new code.

## Step 1 — Branch on the code

`filesystem.CodeOf(err)` returns the code of the outermost `*filesystem.Error` in the chain, `""` for `nil`, and otherwise classifies the error the way the Disk does: `not_found`, `exists` or `permission` for the `io/fs` sentinels, `unavailable` for a context that was cancelled or passed its deadline, and `unknown` for anything else. A `switch` on it is the usual shape:

<!-- ds:block id=avatarordefault-e4fy6urs -->
**avatarOrDefault** · [`examples/concepts-errors/main.go:64-76`](../../examples/concepts-errors/main.go#L64-L76)

```go
func avatarOrDefault(ctx context.Context, disk *filesystem.Disk, key string) string {
	_, err := disk.Stat(ctx, key)
	switch filesystem.CodeOf(err) {
	case "":
		return key
	case filesystem.CodeNotFound:
		return "default.png"
	case filesystem.CodeUnavailable:
		return "retry later"
	default:
		return "error: " + err.Error()
	}
}
```
<!-- /ds:block hash=122cda -->

Check: `avatar: default.png`.

The same works through your own wrapping. `fmt.Errorf("load profile: %w", err)` keeps the code, so a service layer can add context without losing the classification.

## Step 2 — Or ask with errors.Is

Each `Err*` sentinel matches every `*filesystem.Error` with its code, whatever operation, disk or key produced it: `errors.Is(err, filesystem.ErrNotFound)`. Use whichever reads better; they agree.

Check: `errors.Is ErrNotFound: true` and `wrapped, still not_found`.

## Step 3 — What an error says

The Disk fills in the operation, the disk name and the key before returning, so a log line always says what failed where. `errors.As` reaches the struct:

<!-- ds:block id=describe-meqmwbzm -->
**describe** · [`examples/concepts-errors/main.go:81-88`](../../examples/concepts-errors/main.go#L81-L88)

```go
func describe(out io.Writer, err error) {
	var fe *filesystem.Error
	if !errors.As(err, &fe) {
		return
	}
	fmt.Fprintf(out, "code=%s op=%s disk=%s key=%s\n", fe.Code, fe.Op, fe.Disk, fe.Key)
	fmt.Fprintf(out, "message: %v\n", err)
}
```
<!-- /ds:block hash=8fa181 -->

```text
code=not_found op=stat disk=media key=avatars/nobody.png
message: load profile: filesystem: stat media:avatars/nobody.png: not_found: no object "avatars/nobody.png"
```

The message format is `filesystem: <op> <disk>:<key>: <code>: <cause>`, with empty parts left out. `Op` is an `OpKind` (`stat`, `read`, `write`, `delete`, `list`, `copy`, `move`, `presign`, …; the full list is in [reference/operations](../reference/operations.md)).

`RetryAfter` is the one more field: how long the backend asked the caller to wait before trying again (an HTTP `Retry-After` on a 429 or 503, or the vendor's equivalent), `0` when it did not say. `filesystem.RetryAfterOf(err)` finds it anywhere in the chain. `layer.Retry` waits at least that long before its next attempt and gives up at once when the wait is longer than its `MaxDelay`; `fshttp` and the upload handlers pass it on to their clients as a `Retry-After` header, in whole seconds rounded up.

## Step 4 — The native error, when you need it

`*filesystem.Error` keeps the backend's own error as its cause, reachable through `errors.As`. On local disk that is an `*fs.PathError`; on S3 the SDK's `smithy.APIError`, on GCS a `*googleapi.Error`. This is the escape hatch for vendor-specific handling, such as reading an S3 request ID for a support ticket. Code that uses it is tied to that backend; portable code stops at the code.

<!-- ds:block id=nativecause-z956hs3j -->
**nativeCause** · [`examples/concepts-errors/main.go:95-110`](../../examples/concepts-errors/main.go#L95-L110)

```go
func nativeCause(ctx context.Context, out io.Writer, dir string) error {
	drv, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		_ = drv.Close()
		return err
	}
	defer func() { _ = disk.Close() }()
	_, err = disk.Stat(ctx, missingKey)
	var pe *fs.PathError
	fmt.Fprintf(out, "local: %s, native *fs.PathError: %t\n", filesystem.CodeOf(err), errors.As(err, &pe))
	return nil
}
```
<!-- /ds:block hash=d983c9 -->

Check: `local: not_found, native *fs.PathError: true`.

## Step 5 — Batch failures

`DeleteKeys` deletes every key it can and reports the rest in a `*filesystem.BatchError`, whose `Failures` list each failed key with its own error. One bad key never stops a cleanup. `errors.Is` and `CodeOf` see the per-key errors, so `errors.Is(err, filesystem.ErrPermission)` asks whether any key failed for lack of permission.

<!-- ds:block id=batch-qkwf94nk -->
**batch** · [`examples/concepts-errors/main.go:116-130`](../../examples/concepts-errors/main.go#L116-L130)

```go
func batch(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	for _, k := range []string{"tmp/a", "tmp/b"} {
		_, _ = disk.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{})
	}
	err := disk.DeleteKeys(ctx, []string{"tmp/a", "../escape", "tmp/b"})
	var be *filesystem.BatchError
	if errors.As(err, &be) {
		for _, f := range be.Failures {
			fmt.Fprintf(out, "batch failure: %q %s\n", f.Key, filesystem.CodeOf(f.Err))
		}
	}
	fmt.Fprintf(out, "batch errors.Is ErrInvalidKey: %t\n", errors.Is(err, filesystem.ErrInvalidKey))
	left, _ := disk.Exists(ctx, "tmp/a")
	fmt.Fprintf(out, "tmp/a still there: %t\n", left)
}
```
<!-- /ds:block hash=6042d1 -->

Check: `batch failure: "../escape" invalid_key`, `batch errors.Is ErrInvalidKey: true`, and `tmp/a still there: false`, because the valid keys were deleted.

A driver that returns an error other than a `*BatchError` from its bulk delete means the call failed as a whole; the Disk then reports every key as failed, because none can be assumed deleted. `Manager.Check`, `Manager.Close` and a mirrored disk join several failures with `errors.Join`, which `errors.Is` also sees through.

## Step 6 — HTTP status codes

`fshttp.StatusOf(err)` maps an error to the status a handler should answer with. The library's handlers (`fshttp`, `upload`, `recordshttp`) use it, and it is exported so your own handlers answer the same way:

<!-- ds:block id=statuses-hn9je66t -->
**statuses** · [`examples/concepts-errors/main.go:135-140`](../../examples/concepts-errors/main.go#L135-L140)

```go
func statuses(out io.Writer) {
	for _, c := range filesystem.CodeValues {
		s := fshttp.StatusOf(filesystem.Errorf(c, "example"))
		fmt.Fprintf(out, "%s -> %d %s\n", c, s, http.StatusText(s))
	}
}
```
<!-- /ds:block hash=560d97 -->

The mapping is the HTTP column of the table at the top. `fshttp` answers with the status text only, so driver messages and keys of other objects never reach a client. Your own handlers get the same rule from `fshttp.ClientMessage(err)`, which the upload handlers use: they send the message too, but only for `too_large`, `rejected`, `quota_exceeded`, `invalid_argument` and `invalid_key`, because it describes the client's own file, and they put the code in the JSON body so a client can tell "too large" from "wrong type". The message sent is the error's cause, never the full annotated text, and a cause that is itself an annotated `*filesystem.Error` is judged again by the same rule, so the storage key being written never reaches the client (a request that passes the request size cap in the middle of a write answers `{"error":"http: request body too large","code":"too_large"}`):

```json
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
```

### Your own error envelope

Those two shapes are defaults, not the contract. Every HTTP handler of the library takes an `fshttp.ErrorWriter`, `func(w, r, status, err)`, in its options: `fshttp.PublicOptions.Errors`, `SignedOptions.Errors` and `ZipOptions.Errors`; `upload.HandlerOptions.Errors` and the same field on the presign, chunked, Uppy and limits options; and `Responder{Errors: …}` in the gin, echo, fiber and chi adapters. A nil one keeps the package's shape above. The writer gets the status the handler chose and the error itself, with `Retry-After` already set, so it can answer RFC 9457 `application/problem+json`, map `quota_exceeded` to 402 for a plan limit, and log the cause of a 5xx:

```go
problem := func(w http.ResponseWriter, r *http.Request, status int, err error) {
	if filesystem.CodeOf(err) == filesystem.CodeQuotaExceeded {
		status = http.StatusPaymentRequired // a plan limit, not a full disk
	}
	if status >= 500 {
		slog.ErrorContext(r.Context(), "storage failed", "err", err) // the full error, for logs only
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"title":  fshttp.ClientMessage(err), // never err.Error(): it names the disk and the key
		"code":   filesystem.CodeOf(err),
	})
}
uploads := upload.Handler(disk, policy, upload.HandlerOptions{Errors: problem})
files := fshttp.Signed(manager, signer, fshttp.SignedOptions{Errors: problem})
```

Success bodies have the same seam: in the upload handlers, `Respond`, an `upload.SuccessWriter` (see [Uploads](uploads.md#your-own-success-envelope)); in the records endpoints, `recordshttp.Config.Respond` (see [HTTP endpoints](../records/http-endpoints.md#your-own-success-envelope)); and in share links, `share.WithRespond` (see [Share links: Your own link policy](../plugins/share.md#your-own-link-policy)).

The error is never nil: a refusal the handler makes itself (a wrong method, a private object, a request that does not match its signed URL) comes as a `*filesystem.Error` too. Show clients `fshttp.ClientMessage(err)`, never `err.Error()`.

Try it: [`examples/concepts-errors`](../../examples/concepts-errors) runs this section in `envelope.go` (`problem`): `go run ./concepts-errors` from `examples/`.

## Errors from drivers and interceptors

Drivers create errors with `filesystem.NewError(code, cause)` (keeping the native cause) or `filesystem.Errorf(code, format, args...)`. A driver that learns from the backend how long to wait sets it with `.WithRetryAfter(d)` on its `unavailable` error. Plain errors are classified on the way out: the `io/fs` sentinels map to `not_found`, `exists` and `permission`, anything else becomes `unknown` with the original kept as the cause. Interceptors return errors the same way; `layer.Guard` turns a plain error from its check into `permission`.

## Pitfalls

- Comparing `err == filesystem.ErrNotFound`: the returned error is a `*filesystem.Error`, never the sentinel itself. Use `errors.Is` or `CodeOf`.
- Retrying on every error: only `unavailable` is transient. Retrying `rejected`, `too_large` or `permission` repeats the same failure. A context deadline or cancellation is `unavailable`, so a deadline from `layer.Timeout` inside `layer.Retry` is retried, while `layer.Retry` gives up at once when the caller's own context has ended; see [Interceptors](interceptors.md).
- Showing `err.Error()` to end users: it contains the disk name, the storage key and backend messages (`filesystem: write files.acme:settings/../../globex/x.json: invalid_key: …` tells one tenant another's disk and keys). Answer with `fshttp.StatusOf(err)` and `fshttp.ClientMessage(err)`, which gives the client its own error's words for the codes about its input and the status text for everything else, or map the code to your own message.
- Treating `Exists` returning an error as "missing": `Exists` returns `false, nil` only for `not_found`. An outage is an error.

## Next

- [Interceptors](interceptors.md): retrying `unavailable`, and how a timeout is classified.
- [Uploads](uploads.md): the JSON error bodies of the upload handlers.
- [reference/errors](../reference/errors.md): every code, sentinel and status in one table.
