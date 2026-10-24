# Tenant branding: a logo, a favicon and an email header per tenant

The branding settings of a white-label SaaS. Each tenant has three slots, `logo`, `favicon` and `email-header`, each a single role holding one file, each with its own dimension rules (a favicon must be square, an email header at least 600 pixels wide), each re-encoded on upload and resized to the sizes the app renders, all served from public URLs that keep working in emails. It uses `records` (single roles, the tenant as the scope, `SetAttachment` with `PreviousPurge`), `contrib/imaging` (`Sanitize`, `CheckDimensions`) and the `variants` plugin (`Generate`, `URL`). The guide is [Tenant branding](../../docs/recipes/tenant-branding.md).

## Run it

```sh
cd examples
go run ./recipe-tenant-branding
```

It creates a temporary directory (printed at start-up) holding `app.db` (SQLite) and `files/`, and listens on `http://localhost:8123`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Run the tests with `go test ./recipe-tenant-branding`.

| Route | What it does |
|---|---|
| `PUT /tenants/{id}/branding/{slot}` | multipart form, field `file`; answers the tenant's whole branding |
| `DELETE /tenants/{id}/branding/{slot}` | purges the slot's file, back to the app's default |
| `GET /tenants/{id}/branding` | the public URL of every size of every slot set |
| `GET /public/…` | the public files, served by `fshttp.Public` |

## Walkthrough

1. Set the three slots (an 800 × 200 logo, a 256 × 256 favicon, a 1600 × 400 email header). The last answer lists every size:

```json
{"email-header":{"email-header-600":"http://localhost:8123/public/variants/fil_hmfpfhqukmy5ow54frakc6fmz3/email-header-600-ba4854fd150223b3-urdoqtleipfkm2qiuawgb2bnio.jpg"},"favicon":{"favicon-180":"http://localhost:8123/public/variants/fil_aqmiqlrk4xcb2wsnp2qitu4nnx/favicon-180-2d459430bf4886b9-blwpasuxciq6ut2ppheiu5tubv.png","favicon-32":"http://localhost:8123/public/variants/fil_aqmiqlrk4xcb2wsnp2qitu4nnx/favicon-32-2816e837351dbe18-7elzpza4aafkpgmfevndg3wjak.png"},"logo":{"logo-header":"http://localhost:8123/public/variants/fil_ugeurcs5gnbq5utjhh6iggpwyt/logo-header-bc56e177905909a7-l4wrzw6c52xri42se65eoj54pd.png"}}
```

2. A 300 × 200 favicon, and a slot that does not exist, are refused:

```text
{"error":"image is 300x200; its shape must be 1.00:1 (within 1%), not 1.50:1","code":"rejected"}
415
{"error":"Not Found","code":"not_found"}
404
```

3. `DELETE /tenants/acme/branding/favicon` answers `204`; `GET /tenants/globex/branding` answers `{}`, since another tenant has none.

## The code

The branding model, and the sizes made from it:

<!-- ds:block id=slots-rcnmhvks -->
**slots** · [`examples/recipe-tenant-branding/main.go:87-91`](main.go#L87-L91)

```go
var slots = map[string]slot{
	slotLogo:        {limits: imaging.DimensionLimits{MinHeight: 64, MaxWidth: 4000, MaxHeight: 4000}, sizes: []string{"logo-header"}},
	slotFavicon:     {limits: imaging.DimensionLimits{MinWidth: 180, MinHeight: 180, Aspect: 1}, sizes: []string{"favicon-32", "favicon-180"}},
	slotEmailHeader: {limits: imaging.DimensionLimits{MinWidth: 600, MaxPixels: 4_000_000}, sizes: []string{"email-header-600"}},
}
```
<!-- /ds:block hash=1a56f3 -->

<!-- ds:block id=sizes-qs9ewxs7 -->
**sizes** · [`examples/recipe-tenant-branding/main.go:96-101`](main.go#L96-L101)

```go
var sizes = map[string]imaging.Transform{
	"logo-header":      {Mode: imaging.ModeFit, Width: 512, Height: 128, Format: imaging.FormatPNG},
	"favicon-32":       {Mode: imaging.ModeFill, Width: 32, Height: 32, Format: imaging.FormatPNG},
	"favicon-180":      {Mode: imaging.ModeFill, Width: 180, Height: 180, Format: imaging.FormatPNG},
	"email-header-600": {Mode: imaging.ModeFit, Width: 1200, Height: 600, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=c947dd -->

One policy per slot:

<!-- ds:block id=policies-n3qm6xw3 -->
**policies** · [`examples/recipe-tenant-branding/main.go:118-130`](main.go#L118-L130)

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

Setting a slot: upload and attach in one transaction, then generate that slot's sizes:

<!-- ds:block id=app-setslot-v4yntvfq -->
**App.setSlot** · [`examples/recipe-tenant-branding/main.go:194-215`](main.go#L194-L215)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The slots, sizes and policies, the store, the three routes and the server. |
| `main_test.go` | All three slots with each size's real dimensions, a logo replacement that leaves the object count unchanged, a reset, tenant isolation, and every slot's refusals. |

## Next

- The guide: [Tenant branding](../../docs/recipes/tenant-branding.md).
- [recipe-avatars](../recipe-avatars) and [recipe-cover-images](../recipe-cover-images): the one-slot versions.
- [records-attachments](../records-attachments): roles and single roles on their own.
