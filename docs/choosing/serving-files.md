# Serving files: which path

A stored file reaches a browser in one of six ways: your Go process streams it, the backend serves a signed link, a public URL points at the bucket or a CDN, a CDN checks its own signature, a reverse proxy sends the bytes after your handler said yes, or the bytes are inlined in the page. They differ in who pays for the bandwidth, whether you can refuse a request after the link was issued, and whether the link can be shared. This page helps you pick one per kind of file.

## Before you start

- [Serving files](../concepts/serving-files.md) is the full guide to every path below, run by [concepts-serving](../../examples/concepts-serving) and [concepts-delivery](../../examples/concepts-delivery).
- Whether a file is public or private is decided per object or per disk: read [Public and private files](../concepts/public-and-private.md) first if you have not.

## The paths side by side

| Path | API | Bytes pass through your process | Can be revoked after issue | Shareable | Backends | Guide | Example |
|---|---|---|---|---|---|---|---|
| Through your server, after your own checks | `fshttp.Signed` for links signed by `signedurl`; `fshttp.Public` for public objects; your handler with `fshttp.ServeEntry` after its own check (ranges, 304 and the security headers kept) | yes | yes: every request runs your code | only while signed | all | [Serving files, steps 1 and 2](../concepts/serving-files.md#step-1--signed-urls-on-any-backend), [step 7](../concepts/serving-files.md#step-7--public-files) | [concepts-serving](../../examples/concepts-serving) |
| Signed URL from the backend | `Disk.SignedURL` (or `records.Store.URL`, `DownloadURL`) | no | no: valid until it expires (at most seven days, `filesystem.MaxPresignExpiry`) | yes, until it expires | `Presign`: S3, GCS with a signing key, Azure | [Serving files, step 1](../concepts/serving-files.md#step-1--signed-urls-on-any-backend), [Private downloads](../recipes/private-downloads.md) | [recipe-private-downloads](../../examples/recipe-private-downloads) |
| Public URL | `Disk.PublicURL`, from `WithPublicURL` or `public_url=` | no | no | yes, forever | any, with a base configured | [Serving files, step 7 and step 8](../concepts/serving-files.md#step-8--a-cdn-in-front), [Public CDN](../recipes/public-cdn.md) | [recipe-public-cdn](../../examples/recipe-public-cdn) |
| A CDN that checks signatures | `contrib/cloudfront` (signed URLs and cookies), `contrib/cloudflare` (token authentication) as the disk's `URLSigner` | no | no: valid until it expires | yes, until it expires | any bucket behind that CDN | [Serving files, step 11](../concepts/serving-files.md#step-11--signed-cdn-links) | [concepts-delivery](../../examples/concepts-delivery) |
| Your checks, the proxy's bytes | `Sendfile` in `fshttp.PublicOptions` or `fshttp.SignedOptions` (`X-Accel-Redirect`, `X-Sendfile`) | no: nginx, Apache or lighttpd sends them | yes | only while signed | local disk behind a reverse proxy | [Serving files, step 12](../concepts/serving-files.md#step-12--let-the-proxy-send-the-bytes) | [concepts-delivery](../../examples/concepts-delivery) |
| Inlined in the page | `Disk.DataURI(ctx, key, maxBytes)` | yes, once, into the HTML or JSON | n/a | as part of the page | all | [Serving files, step 13](../concepts/serving-files.md#step-13--tiny-files-as-data-uris) | [concepts-delivery](../../examples/concepts-delivery) |

For images there is one more entry point: `variants.Handler` serves `GET /{fileID}/{name}` as a `302` to the variant's URL (signed, or public for a public file on a disk with public URLs), generating the variant first if needed, with format negotiation and device-density selection as options ([Variants, step 4](../plugins/variants.md#step-4--serve-variants-over-http), [records-variants](../../examples/records-variants)). Many files at once go out as one zip with `fshttp.ServeZip` ([Serving files, step 10](../concepts/serving-files.md#step-10--many-files-as-one-zip)).

## By kind of file

| The file is | Serve it with | Why |
|---|---|---|
| A product photo, a published avatar, a logo | a public URL through a CDN | Anyone may see it, so a permanent URL a cache can keep is right |
| An invoice, a contract, a medical record | a short signed URL, issued after your own permission check | No URL works for long; each one is issued to someone your code just authorised |
| A private file that must stop being reachable the moment access is withdrawn | your server (`fshttp.Signed`, or your handler) | A signed link from the backend stays valid until it expires; only a request through your code can be refused later |
| Large private media (video, archives) on a local disk | `Sendfile`, so the proxy sends the bytes | Your checks still run; the Go process does not copy gigabytes |
| Paid media behind CloudFront or Cloudflare | the CDN's signer | The CDN refuses an expired or forged link at the edge, and your origin never sees the download |
| A thumbnail in a list | `variants.Handler`, or the plugin's `SrcSet` in your HTML | One URL per size, generated on first request |
| A 300-byte icon or a blurred placeholder | `Disk.DataURI` | It saves a request; keep `maxBytes` small |

## The decision in short

1. If a file is meant to be public, give it a public URL and put a CDN in front ([Public CDN](../recipes/public-cdn.md)).
2. If it is private and a link may live for minutes, sign it: `Disk.SignedURL` with the shortest expiry that works. The backend serves it if it has `Presign`; otherwise `signedurl` and `fshttp.Signed` do it, with the same call.
3. If it is private and access must be revocable at once, serve it through your own code every time; hand the bytes to `fshttp.ServeEntry` rather than `io.Copy`, so ranges, 304s and the security headers stay ([serving from your own handler](../concepts/serving-files.md#serving-from-your-own-handler)).
4. If it is private, large and on a local disk, keep the checks in Go and let nginx send the bytes (`Sendfile`).
5. If a CDN fronts the bucket and supports signed links, sign for the CDN, not the bucket: `WithURLSigner` with the CDN's signer then signs every download even on an S3, GCS or Azure disk, while uploads keep the bucket's own presigned links ([which path signs which method](../concepts/serving-files.md#which-path-signs-which-method)).
6. If it is an image shown at several sizes, serve its variants, not the original.

## Pitfalls

- `SignedURL` has no default expiry: every caller chooses one, and seven days is the ceiling. A signed URL handed out cannot be withdrawn before it expires. For links signed by `signedurl`, removing a key from its list invalidates every link that key signed ([Serving files, step 4](../concepts/serving-files.md#step-4--rotating-keys)); for links the backend signed, only the backend's credentials control them.
- `PublicURL` does not check that the object exists or is public. A URL for a private object simply fails when fetched.
- HTML, SVG and other active content is always served as a download by `fshttp`, so an uploaded page cannot run script on your origin ([Serving files, step 5](../concepts/serving-files.md#step-5--active-content-is-always-a-download)). Serve user content from a separate origin all the same ([Going to production, step 8](../start/production.md#step-8--a-separate-origin-for-user-content)).
- A long `Cache-Control` on a file that can later be made private or deleted keeps cached copies alive until they expire; the library deletes the object, not the CDN's copy. Purge the CDN on `file.visibility_changed`, `file.trashed` and `file.purged`.
- `fshttp.Public` answers 404 for private and missing objects alike, so it never reveals that a private key exists; do the same in handlers of your own.

## Related

- [Serving files](../concepts/serving-files.md): every path in depth, with ranges, caching headers, HEAD and `io/fs`.
- [Private downloads](../recipes/private-downloads.md) and [Public CDN](../recipes/public-cdn.md): the two most common setups as recipes.
- [Which upload method](which-upload-method.md): the other direction.
