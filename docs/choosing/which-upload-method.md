# Which upload method

Bytes reach your storage in one of eight ways, and every one of them runs the same `upload.Policy`: the type is detected from the content's first bytes, the size is counted while streaming, active content is refused unless the policy allows it, and a refused file is never left on the disk ([Uploads, the guarantees](../concepts/uploads.md#the-guarantees)). What differs is who sends the bytes, whether they pass through your server, how large a file can comfortably be, and which backends support it. This page helps you pick one.

## Before you start

- Every method takes an `upload.Policy`: read [Uploads, step 1](../concepts/uploads.md#step-1--the-policy) first.
- Methods that let the client send bytes straight to storage need a disk that can sign: a backend with the `Presign` capability (S3, GCS with a signing key, Azure), or any disk given a `URLSigner` with `fshttp.Signed` mounted. [Which adapter](which-adapter.md) lists which backend has what.

## The methods side by side

| Method | API | Bytes pass through your server | Size it suits | Backends | Guide | Example |
|---|---|---|---|---|---|---|
| Plain form post | `upload.Handler`, or `upload.Receive` on a stream you hold | yes | up to tens of MB | all | [Uploads, step 3](../concepts/uploads.md#step-3--the-form-handler) | [concepts-uploads](../../examples/concepts-uploads) |
| Presigned PUT | `upload.Presign` (or `upload.PresignHandler`), then `upload.Confirm` | no | one request, up to the backend's single-PUT limit | `Presign`, or a `URLSigner`; a public upload (`Policy.AllowPublic`) needs `PresignVisibility`: S3 and GCS in ACL mode | [Uploads, step 4](../concepts/uploads.md#step-4--direct-uploads-presign-then-confirm), [Public direct uploads](../concepts/uploads.md#public-direct-uploads), [Browser direct uploads](../recipes/browser-direct-uploads.md) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Browser POST form the bucket checks | `Policy.PostForm: true` with `upload.Presign`, or `Disk.PresignPost`; then `upload.Confirm` | no | as a presigned PUT, with the size range enforced by the bucket | `PresignPost`: S3, and GCS with a signing key | [Form uploads the bucket checks](../concepts/uploads.md#form-uploads-the-bucket-checks) | [recipe-browser-form-uploads](../../examples/recipe-browser-form-uploads), [adapters-s3-post](../../examples/adapters-s3-post) |
| Browser multipart (Uppy) | `upload.Uppy` | no | very large files, parts uploaded in parallel | `Multipart` and `PresignPart`: the s3 driver only; elsewhere every route answers 501 | [Uploads, step 5](../concepts/uploads.md#step-5--browser-multipart-uploads-with-uppy) | [concepts-uploads](../../examples/concepts-uploads), [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Resumable (tus) | `contrib/tus` | yes, then promoted through `Receive` | any size, over links that drop | all | [Resumable uploads](../recipes/resumable-uploads.md) | [recipe-resumable-uploads](../../examples/recipe-resumable-uploads) |
| Upload sessions | `upload.NewSessions` with `upload.MemorySessions` or `records/uploadsessions`; the session's `Resolver` on `upload.Handler` | yes | as a form post | all | [Upload sessions](../plugins/upload-sessions.md), [Uploads, upload sessions](../concepts/uploads.md#upload-sessions-a-token-valid-for-a-window) | [records-upload-sessions](../../examples/records-upload-sessions) |
| Chunked (Dropzone, FilePond, resumable.js) | `upload.Chunked` | yes, in small POSTs assembled on the server | files larger than a proxy's body limit | all | [Upload clients, step 2](../concepts/upload-clients.md#step-2--chunked-uploads) | [concepts-upload-clients](../../examples/concepts-upload-clients) |
| Data URI or base64 in JSON | `upload.FromDataURI`, `upload.FromBase64` | yes | small: the whole body is in the JSON | all | [Base64 bodies and data URIs](../concepts/uploads.md#base64-bodies-and-data-uris) | [concepts-uploads](../../examples/concepts-uploads) |

Two more ways in, for completeness: `upload.FromURL` imports a file from a link the user pasted, with the fetch guarded against private addresses ([Import from URL](../recipes/import-from-url.md), [recipe-import-from-url](../../examples/recipe-import-from-url)); and `records.Store.Upload` runs `Receive` and records the file in one call ([Files](../records/files.md), [records-files](../../examples/records-files)).

## What each method checks, and when

| Method | `Process` runs | `Inspect` runs | Checked |
|---|---|---|---|
| Form post, `Receive` | yes | yes | before and while storing |
| Presigned PUT or POST | no | yes | at `Presign` (the declared type and size) and at `Confirm` (the stored bytes) |
| Uppy | no | yes | at create (declared) and at complete (stored bytes) |
| tus | yes | yes | when the upload finishes |
| Chunked | as `Receive` | as `Receive` | the declared total at the first chunk, the bytes at assembly |
| Data URI, base64, `FromURL` | yes | yes | before and while storing |

`Process` (re-encoding an image, stripping metadata) can run only where the bytes pass through your server. If you must re-encode every file, choose a method whose bytes pass through you, or process afterwards in a job ([Maintenance jobs](../concepts/maintenance-jobs.md)). A presigned upload is the client's bytes until `Confirm` has read them: never skip `Confirm`, even after a POST form the bucket checked.

## The decision in short

1. If the client is your own web page and files are photos or documents of a few MB, use a **form post** (`upload.Handler`). It is the simplest contract and the only one where everything, including `Process`, runs before you answer.
2. If the files are large and you do not want them through your servers, use a **presigned PUT** and `Confirm`.
3. If the bucket itself must refuse a file of the wrong size or type, before your server sees anything, use a **POST form** (`PostForm: true`), on S3 or GCS.
4. If files are hundreds of MB to many GB from a browser, on S3, use **Uppy multipart**.
5. If uploads come from phones on bad networks, or must resume after the app is killed, use **tus**. See [Mobile uploads](../recipes/mobile-uploads.md) for the full mobile decision.
6. If a proxy in front of you caps request bodies, or your front end already uses Dropzone, FilePond or resumable.js, use **chunked** uploads.
7. If the uploader is not the user your API authenticates (a one-off slot for a phone, a third party, a "send me the file" link), issue an **upload session**. Use `records/uploadsessions` when you run more than one instance.
8. If a client can only send JSON and the files are tiny (a signature, an icon), accept a **data URI** or base64.

## Pitfalls

- `upload.Uppy` needs `PresignPart`; on any disk but S3 every route answers 501. Check `disk.Capabilities()` before you ship a front end that depends on it.
- `UppyOptions.Authorize` runs before every route but create. Without it, one user could complete or abort another's upload, because the upload ID and key come from the client. `UppyOptions.OnStored` records the file in the complete request itself, deleting it if the record fails.
- A presigned upload can be abandoned half way. Sweep unfinished multipart uploads and staged chunks on a schedule ([Uploads, step 9](../concepts/uploads.md#step-9--sweep-what-nobody-finished)), and only ever point `upload.Sweep` at a staging prefix, never at recorded files.
- A data URI multiplies the body by about four thirds and holds it in memory; keep the policy's `MaxSize` small on that route.
- tus keeps unfinished sessions in a local directory by default, so behind a load balancer without sticky sessions one upload's requests reach instances that do not have it. Give every instance the same `Config.Store` (tusd's s3store, gcsstore or azurestore) and a shared `Config.Locker` ([Resumable uploads, for production](../recipes/resumable-uploads.md#for-production)).

## Related

- [Uploads](../concepts/uploads.md): every path end to end.
- [Upload clients](../concepts/upload-clients.md): the JSON contract, progress, folders and retries under an `Idempotency-Key`.
- [Mobile uploads](../recipes/mobile-uploads.md) and [Browser direct uploads](../recipes/browser-direct-uploads.md): the two most common client setups as recipes.
- [Serving files](serving-files.md): the other direction.
