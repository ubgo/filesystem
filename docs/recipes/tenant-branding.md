# Tenant branding

A white-label or B2B SaaS lets each customer put their own logo in the app header, their own favicon in the browser tab, and their own header image at the top of the emails the app sends on their behalf. Each of those is a slot with its own rules (a favicon must be square, an email header wide), exactly one file per slot, a few fixed sizes, and URLs that must keep working in an email opened a year later. This recipe builds that settings page's backend.

The program is [examples/recipe-tenant-branding](../../examples/recipe-tenant-branding). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `PUT /tenants/{id}/branding/{slot}` with `slot` one of `logo`, `favicon`, `email-header`. Each slot checks its own shape and size, re-encodes the image (no EXIF, nothing appended), and replaces the previous file of that slot, purging it and its sizes.
- The sizes the app renders, generated right after the upload: a 512 × 128 box for the header logo, 32 and 180 pixel favicons, a 1200-pixel email header (600 at 2x).
- `GET /tenants/{id}/branding` answers the public URL of every size of every slot set; a slot not set is absent, so the page falls back to the app's own logo.
- `DELETE /tenants/{id}/branding/{slot}` returns a slot to the default.
- Tenants are isolated: each tenant's files live in its own records scope.

## Before you start

You need the examples module (or your own module requiring `github.com/ubgo/filesystem`, `github.com/ubgo/filesystem/records` and `github.com/ubgo/filesystem/contrib/imaging`), a SQL database (SQLite here) and a disk with public URLs (a local disk served by `fshttp.Public` here; a bucket behind a CDN in production). [Avatars](avatars.md) builds the one-slot version of this; read it first if single roles are new.

## The design

| Piece | Why |
|---|---|
| A `slots` table in code | The branding model in one place: each slot's dimension rules and the sizes made from it. |
| `records.WithSingleRoles("logo", "favicon", "email-header")` | Each slot is a role holding at most one file per tenant, enforced by a unique index. |
| One `upload.Policy` per slot, from a shared base | Same types, size cap and re-encoding everywhere; a different `imaging.CheckDimensions` per slot. |
| The tenant ID as the records scope | One tenant's branding is never listed, replaced or deleted through another's. |
| `variants.Plugin.Generate` with the slot's sizes | Only the sizes a slot needs are made: no 32-pixel favicon of an email header. |
| Public visibility | Branding appears on login pages and in emails, which have no session to sign a URL for. |

## Step 1: The slots

<!-- ds:block id=slots-rcnmhvks -->
**slots** · [`examples/recipe-tenant-branding/main.go:87-91`](../../examples/recipe-tenant-branding/main.go#L87-L91)

```go
var slots = map[string]slot{
	slotLogo:        {limits: imaging.DimensionLimits{MinHeight: 64, MaxWidth: 4000, MaxHeight: 4000}, sizes: []string{"logo-header"}},
	slotFavicon:     {limits: imaging.DimensionLimits{MinWidth: 180, MinHeight: 180, Aspect: 1}, sizes: []string{"favicon-32", "favicon-180"}},
	slotEmailHeader: {limits: imaging.DimensionLimits{MinWidth: 600, MaxPixels: 4_000_000}, sizes: []string{"email-header-600"}},
}
```
<!-- /ds:block hash=1a56f3 -->

The rules are `imaging.DimensionLimits`. `Aspect: 1` makes the favicon square (within 1%, since editors round sizes); `MinHeight: 64` keeps a logo from being shown larger than it is; `MaxPixels` on the email header refuses a 20-megapixel photo nobody should mail.

<!-- ds:block id=sizes-qs9ewxs7 -->
**sizes** · [`examples/recipe-tenant-branding/main.go:96-101`](../../examples/recipe-tenant-branding/main.go#L96-L101)

```go
var sizes = map[string]imaging.Transform{
	"logo-header":      {Mode: imaging.ModeFit, Width: 512, Height: 128, Format: imaging.FormatPNG},
	"favicon-32":       {Mode: imaging.ModeFill, Width: 32, Height: 32, Format: imaging.FormatPNG},
	"favicon-180":      {Mode: imaging.ModeFill, Width: 180, Height: 180, Format: imaging.FormatPNG},
	"email-header-600": {Mode: imaging.ModeFit, Width: 1200, Height: 600, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=c947dd -->

Which mode for which size: `ModeFit` for a logo and an email header, whose shape varies per tenant and must not be cropped (a wordmark cut in half is worse than a smaller one); `ModeFill` for favicons, which must be exactly square. PNG for the logo and favicons keeps transparency; JPEG for the email header keeps the message small.

## Step 2: One policy per slot

<!-- ds:block id=basepolicy-hkeghjms -->
**basePolicy** · [`examples/recipe-tenant-branding/main.go:107-113`](../../examples/recipe-tenant-branding/main.go#L107-L113)

```go
var basePolicy = upload.Policy{
	MaxSize:    maxBrandingBytes,
	Allowed:    []string{"image/png", "image/jpeg", "image/webp"},
	Key:        upload.RandomKey("branding"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(imaging.Options{MaxBytes: maxBrandingBytes}),
}
```
<!-- /ds:block hash=f22910 -->

<!-- ds:block id=policies-n3qm6xw3 -->
**policies** · [`examples/recipe-tenant-branding/main.go:118-130`](../../examples/recipe-tenant-branding/main.go#L118-L130)

```go
func policies() (map[string]upload.Policy, error) {
	out := make(map[string]upload.Policy, len(slots))
	for name, s := range slots {
		check, err := imaging.CheckDimensions(s.limits)
		if err != nil {
			return nil, fmt.Errorf("slot %s: %w", name, err)
		}
		p := basePolicy
		p.Inspect = check
		out[name] = p
	}
	return out, nil
}
```
<!-- /ds:block hash=e209bd -->

`CheckDimensions` returns an inspector, which runs once the upload is stored, reads only the image header and refuses (and deletes) an image outside the limits. A limit that makes no sense (a minimum above a maximum) is an error here, at start-up, not on the first upload.

SVG is not in `Allowed`, and is refused by default anyway: an SVG can carry script, and a logo is served from your domain. If tenants must upload SVG logos, sanitize them first ([SVG](../plugins/svg.md)) or rasterise them to PNG on upload.

## Step 3: Set a slot

<!-- ds:block id=app-setslot-v4yntvfq -->
**App.setSlot** · [`examples/recipe-tenant-branding/main.go:194-215`](../../examples/recipe-tenant-branding/main.go#L194-L215)

```go
func (a *App) setSlot(ctx context.Context, tenant, name string, body io.Reader, filename string) (records.File, error) {
	p, ok := a.policies[name]
	if !ok {
		return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "no branding slot %q", name)
	}
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.Upload(ctx, tenant, diskName, body, filename, p); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, tenant, f.ID, records.Owner{Type: ownerTenant, ID: tenant}, name, records.PreviousPurge)
		return err
	})
	if err != nil {
		return records.File{}, err
	}
	if err := a.Variants.Generate(ctx, a.Store, tenant, f.ID, slots[name].sizes...); err != nil {
		fmt.Fprintf(os.Stderr, "branding %s/%s: %v\n", tenant, name, err)
	}
	return f, nil
}
```
<!-- /ds:block hash=4325cf -->

The upload and the attachment commit together; `PreviousPurge` deletes the slot's previous file, its rows and its sizes after the commit, and a failure before the commit deletes the new object. The sizes are generated after the commit, outside the transaction; if that fails the slot is still set, and each size is made on its first request.

## Step 4: Describe the branding

<!-- ds:block id=app-describe-39kec2pp -->
**App.describe** · [`examples/recipe-tenant-branding/main.go:225-246`](../../examples/recipe-tenant-branding/main.go#L225-L246)

```go
func (a *App) describe(ctx context.Context, tenant string) (branding, error) {
	out := branding{}
	for name, s := range slots {
		list, err := a.Store.Attachments(ctx, tenant, records.Owner{Type: ownerTenant, ID: tenant}, name)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			continue
		}
		urls := map[string]string{}
		for _, size := range s.sizes {
			u, err := a.Variants.URL(ctx, a.Store, tenant, list[0].File.ID, size, linkLifetime)
			if err != nil {
				return nil, err
			}
			urls[size] = u
		}
		out[name] = urls
	}
	return out, nil
}
```
<!-- /ds:block hash=620c7b -->

For a public file on a disk with a public base, `Plugin.URL` returns the public URL and ignores the lifetime. That matters for the email header: an email is opened days or months after it is sent, and a signed URL in it would have expired. Variant keys change whenever the image does, so when a tenant uploads a new header, emails already sent keep showing the old one until it is purged, and new emails show the new one.

## Try it

```sh
cd examples
go run ./recipe-tenant-branding
```

```text
listening on http://localhost:8123 data in /var/folders/sk/.../T/recipe-tenant-branding-1987791090
```

Set the logo (800 × 200), the favicon (256 × 256) and the email header (1600 × 400); each answer is the whole branding so far:

```sh
curl -s -X PUT -F file=@logo.jpg localhost:8123/tenants/acme/branding/logo
curl -s -X PUT -F file=@icon.jpg localhost:8123/tenants/acme/branding/favicon
curl -s -X PUT -F file=@header.jpg localhost:8123/tenants/acme/branding/email-header
```

```json
{"logo":{"logo-header":"http://localhost:8123/public/variants/fil_ugeurcs5gnbq5utjhh6iggpwyt/logo-header-bc56e177905909a7-l4wrzw6c52xri42se65eoj54pd.png"}}
```

```json
{"email-header":{"email-header-600":"http://localhost:8123/public/variants/fil_hmfpfhqukmy5ow54frakc6fmz3/email-header-600-ba4854fd150223b3-urdoqtleipfkm2qiuawgb2bnio.jpg"},"favicon":{"favicon-180":"http://localhost:8123/public/variants/fil_aqmiqlrk4xcb2wsnp2qitu4nnx/favicon-180-2d459430bf4886b9-blwpasuxciq6ut2ppheiu5tubv.png","favicon-32":"http://localhost:8123/public/variants/fil_aqmiqlrk4xcb2wsnp2qitu4nnx/favicon-32-2816e837351dbe18-7elzpza4aafkpgmfevndg3wjak.png"},"logo":{"logo-header":"http://localhost:8123/public/variants/fil_ugeurcs5gnbq5utjhh6iggpwyt/logo-header-bc56e177905909a7-l4wrzw6c52xri42se65eoj54pd.png"}}
```

A 300 × 200 image as a favicon, and an unknown slot, are refused:

```sh
curl -s -X PUT -F file=@wide.jpg localhost:8123/tenants/acme/branding/favicon -w '\n%{http_code}\n'
curl -s -X PUT -F file=@wide.jpg localhost:8123/tenants/acme/branding/wallpaper -w '\n%{http_code}\n'
```

```text
{"error":"image is 300x200; its shape must be 1.00:1 (within 1%), not 1.50:1","code":"rejected"}

415
{"error":"Not Found","code":"not_found"}

404
```

Reset the favicon, and look at another tenant:

```sh
curl -s -i -X DELETE localhost:8123/tenants/acme/branding/favicon | head -1
curl -s localhost:8123/tenants/globex/branding
```

```text
HTTP/1.1 204 No Content
{}
```

`go test ./recipe-tenant-branding` checks every size's real dimensions (the 1600 × 400 email header gives 1200 × 300, the 800 × 200 logo 512 × 128), that replacing the logo leaves the object count unchanged, and that every slot's rules refuse what they should without leaving objects.

## Pitfalls

- **Signed URLs in emails.** A signed URL expires; the email does not. Keep branding public, or serve email images through a route of your own that never expires.
- **One generic "image" policy for every slot.** A 4000 × 300 banner uploaded as a favicon becomes a 32-pixel grey line. Check each slot's shape on the server; the browser's file picker is not a check.
- **Cropping a logo.** `ModeFill` crops; a wordmark loses its ends. Use `ModeFit` for anything with text.
- **SVG logos straight to the disk.** An SVG can run script when opened from your domain. Sanitize or rasterise it.
- **Emails already sent.** A replaced header is purged, so emails sent with the old URL show a broken image. If that matters, attach the email header with `records.PreviousKeep`, which only detaches the old file, and purge detached headers older than a few months from a job of your own. `PreviousTrash` does not help here: trashing a public file hides it and drops its variants, so the old URLs stop working at once.
- **A CDN in front of the bucket** caches by URL. Variant URLs change with the image, so caching them forever is safe; never cache the `GET /tenants/{id}/branding` answer forever.

## For production

- Authorize: only an admin of tenant `{id}` may change its branding.
- Put branding on its own public bucket behind a CDN (`WithPublicURL("https://brand-cdn.example.com")`), separate from private tenant data ([public and private](../concepts/public-and-private.md)).
- Cache the branding answer per tenant in your app (it changes rarely), and invalidate it on `PUT` and `DELETE`.
- Add slots by adding a row to `slots` and a role to `WithSingleRoles`; existing tenants are unaffected.

## Related

- [Avatars](avatars.md): one image per user, with a generated fallback.
- [Cover images](cover-images.md): a wide banner as a responsive srcset.
- [Attachments](../records/attachments.md): roles, single roles, `SetAttachment`.
- [Variants](../plugins/variants.md): declaring sizes, generating them, pruning old ones.
- [Multi-tenancy](../concepts/multi-tenancy.md): scopes, per-tenant disks and quotas.
