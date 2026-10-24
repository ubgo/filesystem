# github.com/ubgo/filesystem/records/recordshttp

HTTP endpoints for the records layer: upload files and attach them to an owner, list an owner's files with links, reorder and detach them, make an existing file an owner's only one, redirect to a file, replace its content, change its visibility or its alt text, caption and title, move it to the trash, restore and purge it. One `http.Handler` over `net/http`'s pattern mux, so it mounts in any router that accepts a handler. The guide is [HTTP endpoints](../../docs/records/http-endpoints.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is part of the `records` module, which requires only the core `github.com/ubgo/filesystem` module and `github.com/ubgo/sqlb`. It adds no tables and no migrations.

## Wire it

<!-- ds:block id=newhandler-ytttpkep -->
**newHandler** · [`examples/records-http/main.go:174-191`](../../examples/records-http/main.go#L174-L191)

```go
func newHandler(s *records.Store, disks *filesystem.Manager, signer *signedurl.Signer) (http.Handler, error) {
	media, err := recordshttp.Handler(recordshttp.Config{
		Store:        s,
		Disk:         diskName,
		Policy:       policy,
		MaxFiles:     maxFilesPerRequest,
		Scope:        scopeOf,
		Authorize:    authorizer(),
		LinkLifetime: linkLifetime,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/media/", http.StripPrefix("/media", withActor(media)))
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	return mux, nil
}
```
<!-- /ds:block hash=17e177 -->

`Handler(cfg)` returns an error when `Store`, `Scope` or `Authorize` is missing, when no disk can be chosen (no `Disk`, no `DiskFor` and a Store without `WithVisibilityDisks`), when both `Disk` and `DiskFor` are set, or when `Policy.Validate` fails. Mount it with its prefix stripped.

## Config

| Field | Meaning | Default |
|---|---|---|
| `Store *records.Store` | The Store files are recorded in | required |
| `Disk string` | Disk of the Store's manager that uploads are stored on. Empty: chosen per request by `DiskFor`, or, without it, by the policy's visibility through the Store's `WithVisibilityDisks` (public CDN bucket and private bucket behind one handler) | required unless `DiskFor` or `WithVisibilityDisks` |
| `DiskFor func(*http.Request, Target) (string, error)` | Chooses each upload request's disk after `Scope`, `Authorize` and `Resolve`, before the body is read; `Target` carries scope, owner and role (a tenant's own bucket, a disk per role). Its error refuses the request with its code (`500` when uncoded) and stores nothing; an empty or unknown disk name is `500` `unknown`. Not with `Disk`. The replace route keeps a file on its own disk | `nil` |
| `Policy upload.Policy` | Types, size, keys and visibility of uploads and replacements | required |
| `Scope func(*http.Request) (string, error)` | The scope the request acts in, from the session; an error answers `401`. `ScopeFromRequest` is a ready one that reads the scope your middleware set with `WithScope` (none set: `401`) | required |
| `Authorize func(*http.Request, Action, Target) error` | Whether the request may perform the action; an error answers `403`, its message is not sent | required |
| `MaxFiles int` | Files per upload request | `10` |
| `LinkLifetime time.Duration` | Lifetime of returned and redirected-to signed URLs | `DefaultLinkLifetime` (10 minutes) |
| `Partial bool` | Uploads attach every file they can, each in its own transaction, and list the others in `ListResponse.Failed`, as `upload.HandlerOptions.Partial` | `false`: all files or none |
| `Resolve upload.Resolver` | Chooses each upload and replace request's policy from `Policy`, after `Authorize` and before the body is read; validated (an invalid result is `500` `unknown`, status text only); its own error refuses the request with its code (`unavailable` with a delay is `503` + `Retry-After`) | `nil`: `Policy` |
| `MaxTotal int64` | Caps all the files of one request together: each file gets what earlier ones left, a file that does not fit is `too_large` (a failed file under `Partial`); lowers the request cap when smaller than `MaxFiles` × `MaxSize`; negative refused | `0`: none |
| `Errors fshttp.ErrorWriter` | Renders every failed request in your own envelope (problem+json, an HTML page) and is where a 5xx's cause gets logged: it gets the status this handler would send and the error (the hook's own error, coded `permission`, for the `401` and `403`; a `conflict` for the `412`). `Retry-After` is already set when the backend asked for one. Show clients `fshttp.ClientMessage(err)`, never `err.Error()`. A `Partial` upload's answer listing failed files is a `ListResponse`, not an error | `nil`: `ErrorResponse` JSON |
| `Respond SuccessWriter` | Renders every successful request with a body in your own envelope (`{"data": …}`, camelCase): it gets the status this handler would send and a closed `SuccessBody` (`ListResponse`, `AttachedFile`, `FileResponse` or `StatusResponse`; a `Partial` upload's answer is a `ListResponse`), after the handler's headers (`ETag`, `Retry-After`) are set. Each body carries its `records.File` in `File` (never encoded) for metadata, dimensions or variant URLs. A `204` has no body and does not pass here | `nil`: `JSONSuccess`, the documented JSON |
| `Quarantine bool` | Records every file the upload route stores, and every new content the replace route stores, as quarantined until a worker scans it (`records.Store.Scan`): `GET /files/{id}` answers `409` `conflict`, the list route gives it no `url`, setting it public is `409`, the status route says `quarantined`; `Policy` must not be public | `false` |

## Routes

| Route | Action | Body | Answer |
|---|---|---|---|
| `POST /owners/{type}/{id}/{role}` | `upload` | multipart, files in field `file` (`FormField`) | `201` `ListResponse` without URLs |
| `GET /owners/{type}/{id}/{role}` | `list` | query `limit` (`QueryLimit`) and `cursor` (`QueryCursor`), both optional | `200` `ListResponse` with a `url` per file: every file without the query, one page (`records.Store.AttachmentsPage`, `next_cursor` when more follow) with either; a non-numeric `limit` or a bad cursor is `400` |
| `PUT /owners/{type}/{id}/{role}` | `set_attachment`, then `read` on the file | `SetAttachmentRequest` `{"file_id": "fil_…", "previous": "keep"}` | `200` `AttachedFile` without URL |
| `PUT /owners/{type}/{id}/{role}/order` | `reorder` | `ReorderRequest` `{"ids": [...]}` | `204` |
| `DELETE /attachments/{id}` | `detach` | | `204` |
| `GET /files/{id}` | `read` | | `302` to the file's URL, `Cache-Control: private, no-store` |
| `PUT /files/{id}` | `replace` | multipart, one file in field `file` | `200` `FileResponse` |
| `DELETE /files/{id}` | `trash` | | `204` |
| `POST /files/{id}/restore` | `restore` | | `204` |
| `POST /files/{id}/purge` | `purge` | | `204` |
| `PUT /files/{id}/visibility` | `set_visibility` | `VisibilityRequest` `{"visibility": "public"}` | `204` |
| `GET /files/{id}/status` | `read` | none | `200` `StatusResponse` `{"file_id", "status", "status_detail"}`, `Cache-Control: no-store`; a trashed file is `404` |
| `PUT /files/{id}/text` | `set_text` | `TextRequest` `{"alt": "…", "caption": "…", "title": "…"}`, the whole text (absent fields are removed) | `204` |

An upload request stores and attaches every file in one `Store.Tx`: all or none, with the objects of a rolled-back request deleted. With `Partial` each file has its own transaction; the answer is `201` when all were attached, `200` when some were, the first failure's status when none was, and a request-level failure purges what the request attached. Its body is capped at `MaxFiles × Policy.MaxSize + 64 KiB`; a replacement's at `Policy.MaxSize + 64 KiB`. JSON bodies are capped at 1 MiB and refuse unknown fields. A trashed file is `404` on `GET /files/{id}` and absent from lists.

Every route that names a file loads it in the request's scope before `Authorize`, and passes it as `Target.File`: a file of another scope is `404` and never reaches the hook. The set-attachment route asks twice, `set_attachment` on the owner, role and file, then `read` on the file alone, because the owner's list will hand out a link to it.

`If-Match` with the file's ETag (its SHA-256 in quotes, sent on `PUT /files/{id}`) guards `PUT /files/{id}`, `PUT /files/{id}/visibility`, `DELETE /files/{id}`, restore and purge: a stale ETag is `412` (`conflict`), nothing changed; each route also passes it to the Store as `records.IfUnchanged`, re-checked under the row lock.

## Types

| Type | Fields |
|---|---|
| `Action` | Alias of `records.Action`, so a permission plugin can decide without importing this package: `ActionUpload` `"upload"`, `ActionList` `"list"`, `ActionReorder` `"reorder"`, `ActionDetach` `"detach"`, `ActionRead` `"read"`, `ActionReplace` `"replace"`, `ActionTrash` `"trash"`, `ActionRestore` `"restore"`, `ActionPurge` `"purge"`, `ActionSetAttachment` `"set_attachment"`, `ActionSetVisibility` `"set_visibility"`, `ActionSetText` `"set_text"`; `ActionValues` lists them |
| `Target` | Alias of `records.ActionTarget`: `Scope`, `Owner records.Owner`, `Role` (owner routes, and detach from the attachment), `FileID` (file routes, detach, set-attachment), `File *records.File` (the loaded file on file routes and set-attachment, nil elsewhere; read-only), `Change *records.Change` (the requested `Visibility`, `Text` or `Previous` on the visibility, text and set-attachment routes, read from the capped body before `Authorize`; nil elsewhere and when the body cannot be read, which is then `403` if refused and `400` once allowed) |
| `AttachedFile` | `attachment_id`, `file_id`, `filename`, `content_type`, `size`, `position`, `uploaded_by` (omitted when empty), `url` (list only), `alt`, `caption`, `title` (the file's text, each omitted when empty), `status`, `status_detail` (omitted when empty) |
| `StatusResponse` | `file_id`, `status` (`records.Status`), `status_detail` (omitted when empty) |
| `ListResponse` | `files`: `[]AttachedFile`; `failed`: `[]upload.FileFailure` (`filename`, `error`, `code`), only from a `Partial` upload, omitted when empty; `next_cursor`, only from a paged list when more files follow |
| `SetAttachmentRequest` | `file_id`; `previous`: `records.Previous` (`keep`, `trash`, `purge`), default `keep` |
| `VisibilityRequest` | `visibility`: `filesystem.Visibility` (`public`, `private`) |
| `TextRequest` | `alt`, `caption`, `title`: strings, as `records.Store.SetText` takes them |
| `FileResponse` | `file_id`, `filename`, `content_type`, `size` |
| `ReorderRequest` | `ids`: every attachment ID the list route returns for the owner and role (trashed files are not listed and not needed), once each, in the new order |
| `ErrorResponse` | `error` (message), `code` (`filesystem.Code`) |

Scope on the context, shared with the framework adapters:

| Function | What it does |
|---|---|
| `WithScope(ctx, scope)` | Returns `ctx` carrying the scope; call it in the authentication middleware. |
| `ScopeFromContext(ctx)` | The scope set with `WithScope`, and whether one was. |
| `ScopeFromRequest(r)` | A ready `Config.Scope`: the request context's scope, or a `permission` error (`401`) when none was set. |

## Errors

With `Config.Errors` set, every failure below goes to it with the same status, and the body is yours. Without it:

The status is `fshttp.StatusOf(err)`; a body over its cap is `413` `too_large`. The `error` text is `fshttp.ClientMessage(err)`: the real message only for `too_large`, `rejected`, `quota_exceeded`, `invalid_argument` and `invalid_key`, the codes that describe the client's own input; every other code, `exists` included, answers with the status text. A visible error that wraps another `filesystem.Error` is judged by the inner error's code, so a Disk error's disk and key never reach the client. A backend's requested wait (`filesystem.Error.RetryAfter`) is passed on as `Retry-After`. The hooks' refusals are `{"error":"not signed in","code":"permission"}` (`401`) and `{"error":"forbidden","code":"permission"}` (`403`): like every other error body, only the message, never the `filesystem: permission:` prefix of the error's text.

## Limits

- Purge is allowed on a live file when `Authorize` allows it; a rule that wants trash-first checks `Target.File.DeletedAt`.
- Inside a scope, a file that exists but is refused is `403` while an unknown one is `404`; file IDs are random, so this does not let a client enumerate them.
- Links need a disk that can presign or a `filesystem.WithURLSigner` plus a mounted `fshttp.Signed`; otherwise the list and read routes answer `501`.
