# Product images

A product page needs several photos in an order the merchant controls, a cover, a square thumbnail for the grid, a card image that never cuts off a tall bottle, and a large image for zoom, all served from a CDN. This recipe is for e-commerce catalogs, marketplaces, real-estate and classifieds listings, and any app where a record owns an ordered gallery of public images.

The program is [examples/recipe-product-images](../../examples/recipe-product-images). Every code block below is copied from it, and every transcript is from running it.

## What you get

- The merchant uploads up to 10 photos in one request; either all of them are stored and attached, or none.
- Images keep the order they were uploaded in until the merchant reorders them; the first one is the cover.
- A reorder from a stale list (an image missing or added since the client loaded the gallery) is refused instead of silently dropping an image.
- Three declared sizes: `thumb` (200 px square, cropped), `card` (600 px square, padded), `zoom` (up to 1600 px, never enlarged). The two on every listing page are generated right after the upload; `zoom` only when someone opens it.
- Every image and size has a public URL a CDN can cache. Removing an image deletes it, its sizes and its URLs.
- Photos are re-encoded on the way in, so the camera's GPS position and serial number never reach a public bucket.

## The design

| Piece | Why |
|---|---|
| A `gallery` role, not single | A product has many images; `Attachment.Position` orders them, from 1. The cover is position 1, so "make this the cover" is a reorder, not a second role to keep in sync. |
| `recordshttp.Handler` | Upload-and-attach, list, reorder, detach, replace and trash are the endpoint set every gallery needs; recordshttp serves them for any router. The app supplies `Scope` and `Authorize`. |
| `Store.Reorder` | Takes the complete new order and refuses anything else with `CodeInvalidArgument`, in one transaction. |
| `variants` plugin, `WithEager(thumb, card)` | Only declared sizes exist. Eager sizes are made after the upload commits; `zoom` goes through `variants.Handler`, which makes it on first request and redirects. |
| `imaging.Sanitize` as `Policy.Process` | Re-encodes every photo before it is stored. |
| `Store.Purge` for removal | Deletes the attachment, row and variant rows in one transaction and the objects after it, so nothing of a removed photo stays public. |
| `WithPublicURL` + `fshttp.Public` | The demo serves public files itself. On S3 or R2 the same option points at the CDN domain ([public CDN](public-cdn.md)). |

## Step 1: The policy and the sizes

<!-- ds:block id=imagelimits-6yvh999c -->
**imageLimits** · [`examples/recipe-product-images/main.go:87`](../../examples/recipe-product-images/main.go#L87-L87)

```go
var imageLimits = imaging.Options{MaxBytes: maxImageBytes}
```
<!-- /ds:block hash=2bce3d -->

<!-- ds:block id=gallerypolicy-59s4f88q -->
**galleryPolicy** · [`examples/recipe-product-images/main.go:92-98`](../../examples/recipe-product-images/main.go#L92-L98)

```go
var galleryPolicy = upload.Policy{
	MaxSize:    maxImageBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp"},
	Key:        upload.RandomKey("products"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(imageLimits),
}
```
<!-- /ds:block hash=c61695 -->

`Policy.MaxSize` applies to what `Sanitize` returns, not to the upload, so the cap on what the merchant may send is `imaging.Options.MaxBytes`. `Visibility: filesystem.VisibilityPublic` makes `Store.URL` and the variants' URLs public URLs instead of signed ones.

<!-- ds:block id=sizes-yj3z9sby -->
**sizes** · [`examples/recipe-product-images/main.go:104-108`](../../examples/recipe-product-images/main.go#L104-L108)

```go
var sizes = map[string]imaging.Transform{
	sizeThumb: {Mode: imaging.ModeFill, Width: 200, Height: 200, Format: imaging.FormatJPEG},
	sizeCard:  {Mode: imaging.ModePad, Width: 600, Height: 600, Format: imaging.FormatJPEG},
	sizeZoom:  {Mode: imaging.ModeFit, Width: 1600, Height: 1600, Format: imaging.FormatJPEG, Quality: 90},
}
```
<!-- /ds:block hash=54474a -->

The three modes are the three things a storefront needs. `ModeFill` covers the box and crops the overflow, so grid tiles line up. `ModePad` fits the whole photo and centres it on a white square (transparent for PNG output), so a product is never cut off on its card. `ModeFit` keeps the aspect ratio and never enlarges: an 800 px photo stays 800 px in `zoom`. `ModeFill` takes a `Gravity` when the important part is not the centre (`GravityTop` for apparel on a model).

<!-- ds:block id=newvariants-22rj7var -->
**newVariants** · [`examples/recipe-product-images/main.go:137-142`](../../examples/recipe-product-images/main.go#L137-L142)

```go
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(sizes, imaging.Options{}, variants.WithEager(logFailure, sizeThumb, sizeCard))
}
```
<!-- /ds:block hash=a49d7b -->

## Step 2: The disk and the store

<!-- ds:block id=opendisk-jgkb2fbf -->
**openDisk** · [`examples/recipe-product-images/main.go:120-131`](../../examples/recipe-product-images/main.go#L120-L131)

```go
func openDisk(dataDir, publicBase string) (*filesystem.Manager, *filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithPublicURL(publicBase+publicPrefix))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, disk, err
}
```
<!-- /ds:block hash=f158bc -->

<!-- ds:block id=openstore-jhrmunv8 -->
**openStore** · [`examples/recipe-product-images/main.go:146-160`](../../examples/recipe-product-images/main.go#L146-L160)

```go
func openStore(ctx context.Context, dataDir string, disks *filesystem.Manager, v *variants.Plugin) (*records.Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "catalog.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerProduct),
		records.WithPlugins(v))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}
```
<!-- /ds:block hash=f32b94 -->

## Step 3: The gallery endpoints

`recordshttp.Handler` refuses to build without `Scope` and `Authorize`. Here every request acts in one shop; a marketplace returns the signed-in merchant's shop ID from `Scope`, and every query is then limited to it ([SaaS tenants](saas-tenants.md)).

<!-- ds:block id=ismerchant-uvt2f22h -->
**isMerchant** · [`examples/recipe-product-images/main.go:168`](../../examples/recipe-product-images/main.go#L168-L168)

```go
func isMerchant(r *http.Request) bool { return r.Header.Get(headerMerchant) == merchantToken }
```
<!-- /ds:block hash=e4894e -->

<!-- ds:block id=authorize-f9wmr8n3 -->
**authorize** · [`examples/recipe-product-images/main.go:176-189`](../../examples/recipe-product-images/main.go#L176-L189)

```go
func authorize(r *http.Request, action recordshttp.Action, t recordshttp.Target) error {
	if t.Owner.Type != "" && (t.Owner.Type != ownerProduct || t.Role != roleGallery) {
		return filesystem.Errorf(filesystem.CodePermission, "only product galleries")
	}
	switch action {
	case recordshttp.ActionList, recordshttp.ActionRead:
		return nil
	case recordshttp.ActionUpload, recordshttp.ActionReorder:
		if isMerchant(r) {
			return nil
		}
	}
	return filesystem.Errorf(filesystem.CodePermission, "not allowed")
}
```
<!-- /ds:block hash=faf242 -->

`Authorize` refuses `trash` and `detach` on purpose. A catalogue has no trash screen to restore from, so a trashed image would only keep its bytes (made private while trashed, but still stored and counted) until a retention sweep, and a detached one would stay recorded with no gallery to show it in. Removal goes through the purge route in step 5 instead.

<!-- ds:block id=galleryapi-dhdu2xrj -->
**galleryAPI** · [`examples/recipe-product-images/main.go:194-204`](../../examples/recipe-product-images/main.go#L194-L204)

```go
func galleryAPI(store *records.Store) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:        store,
		Disk:         diskName,
		Policy:       galleryPolicy,
		MaxFiles:     maxImagesPerRequest,
		Scope:        catalogScope,
		Authorize:    authorize,
		LinkLifetime: linkLifetime,
	})
}
```
<!-- /ds:block hash=0fd427 -->

<!-- ds:block id=new-y9s4sspz -->
**New** · [`examples/recipe-product-images/main.go:209-235`](../../examples/recipe-product-images/main.go#L209-L235)

```go
func New(ctx context.Context, dataDir, publicBase string) (*App, error) {
	disks, disk, err := openDisk(dataDir, publicBase)
	if err != nil {
		return nil, err
	}
	v, err := newVariants()
	if err != nil {
		return nil, err
	}
	store, err := openStore(ctx, dataDir, disks, v)
	if err != nil {
		return nil, err
	}
	api, err := galleryAPI(store)
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, Variants: v}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", api))
	mux.HandleFunc("GET /products/{id}", app.product)
	mux.HandleFunc("DELETE /products/{id}/images/{fileID}", app.removeImage)
	mux.Handle(variantsPrefix, http.StripPrefix(variantsPrefix, variants.Handler(store, v, catalogScope, variants.HandlerOptions{})))
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	app.Handler = mux
	return app, nil
}
```
<!-- /ds:block hash=0477fe -->

## Step 4: The storefront view

<!-- ds:block id=app-image-sgkz2taq -->
**App.image** · [`examples/recipe-product-images/main.go:257-267`](../../examples/recipe-product-images/main.go#L257-L267)

```go
func (a *App) image(ctx context.Context, f records.File) (productImage, error) {
	thumb, err := a.Variants.URL(ctx, a.Store, f.Scope, f.ID, sizeThumb, linkLifetime)
	if err != nil {
		return productImage{}, err
	}
	card, err := a.Variants.URL(ctx, a.Store, f.Scope, f.ID, sizeCard, linkLifetime)
	if err != nil {
		return productImage{}, err
	}
	return productImage{FileID: f.ID, Thumb: thumb, Card: card, Zoom: variantsPrefix + f.ID + "/" + sizeZoom}, nil
}
```
<!-- /ds:block hash=8c31cb -->

<!-- ds:block id=app-product-pjf7w76c -->
**App.product** · [`examples/recipe-product-images/main.go:273-293`](../../examples/recipe-product-images/main.go#L273-L293)

```go
func (a *App) product(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	list, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerProduct, ID: id}, roleGallery)
	if err != nil {
		writeError(w, err)
		return
	}
	view := productView{Product: id, Images: []productImage{}}
	for _, af := range list {
		img, err := a.image(r.Context(), af.File)
		if err != nil {
			writeError(w, err)
			return
		}
		view.Images = append(view.Images, img)
	}
	if len(view.Images) > 0 {
		view.Cover = &view.Images[0]
	}
	writeJSON(w, http.StatusOK, view)
}
```
<!-- /ds:block hash=fa7a49 -->

`Plugin.URL` generates a size when it does not exist yet, so calling it for `zoom` on every product view would resize every photo of every product page someone opens. The view hands out `/variants/<file>/zoom` instead, a link to `variants.Handler`, which generates on the first click and then redirects to the cached public object.

## Step 5: Removing an image

<!-- ds:block id=app-removeimage-m7ak2yp4 -->
**App.removeImage** · [`examples/recipe-product-images/main.go:300-321`](../../examples/recipe-product-images/main.go#L300-L321)

```go
func (a *App) removeImage(w http.ResponseWriter, r *http.Request) {
	if !isMerchant(r) {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "merchants only"))
		return
	}
	product := records.Owner{Type: ownerProduct, ID: r.PathValue("id")}
	list, err := a.Store.Attachments(r.Context(), "", product, roleGallery)
	if err != nil {
		writeError(w, err)
		return
	}
	fileID := r.PathValue("fileID")
	if !slices.ContainsFunc(list, func(af records.AttachedFile) bool { return af.File.ID == fileID }) {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "product %s has no image %s", product.ID, fileID))
		return
	}
	if err := a.Store.Purge(r.Context(), "", fileID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=63e16d -->

The check that the file belongs to this product's gallery matters: `Store.Purge` takes only a scope and a file ID, and the app decides who may purge what.

## Try it

```sh
cd examples
go run ./recipe-product-images
```

```text
listening on http://localhost:8102 data in /var/folders/sk/.../T/recipe-product-images-1960606155
```

A shopper cannot upload; the merchant uploads three photos in one request (a 1200 × 900 JPEG, a 600 × 1400 JPEG, a 1000 × 1000 PNG):

```sh
curl -s -F file=@front.jpg localhost:8102/api/owners/product/p1/gallery
curl -s -H 'X-Merchant: demo' -F file=@front.jpg -F file=@side.jpg -F file=@back.png localhost:8102/api/owners/product/p1/gallery
```

```json
{"error":"forbidden","code":"permission"}
{"files":[{"attachment_id":"att_zoiihfw2o25kmzvy6fwdyoaimm","file_id":"fil_ie6tlocmo6r4kqpppqhuzjhfin","filename":"front.jpg","content_type":"image/jpeg","size":372152,"position":1,"status":"ready"},{"attachment_id":"att_avfbe5a6czlr6dgack3q27z6dd","file_id":"fil_coaywfbnpriqxbzshdalznv6yq","filename":"side.jpg","content_type":"image/jpeg","size":293747,"position":2,"status":"ready"},{"attachment_id":"att_wobrpo4mqnbswocmcqr4q3skvc","file_id":"fil_35jj2yfmlwy4i37kif3jonuj7m","filename":"back.png","content_type":"image/png","size":1189913,"position":3,"status":"ready"}]}
```

The storefront view (first entries shown):

```sh
curl -s localhost:8102/products/p1
```

```json
{
    "product": "p1",
    "cover": {
        "file_id": "fil_ie6tlocmo6r4kqpppqhuzjhfin",
        "thumb": "http://localhost:8102/public/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/thumb-bc5aa5507109ef64-s4fer5fq77fg2rrcf7lt3emkgn.jpg",
        "card": "http://localhost:8102/public/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/card-740e6aabff1017f4-bmqkwizztdfjzisr3cvciip5dv.jpg",
        "zoom": "/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/zoom"
    },
    "images": [ ... ]
}
```

Opening the zoom generates it once and redirects to the public object; an undeclared size does not exist:

```sh
curl -s -i localhost:8102/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/zoom
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -L localhost:8102/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/zoom
curl -s -i localhost:8102/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/huge
```

```text
HTTP/1.1 302 Found
Cache-Control: private, max-age=60
Location: http://localhost:8102/public/variants/fil_ie6tlocmo6r4kqpppqhuzjhfin/zoom-16e182ee93caa12d-hmdhzgrj5q4qjrotqglc4v6ttq.jpg

200 image/jpeg 372179

HTTP/1.1 404 Not Found
```

Make the PNG the cover. A list with an image missing is refused; the complete list is applied:

```sh
curl -s -X PUT -H 'X-Merchant: demo' -d '{"ids":["att_wobrpo4mqnbswocmcqr4q3skvc","att_zoiihfw2o25kmzvy6fwdyoaimm"]}' localhost:8102/api/owners/product/p1/gallery/order
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H 'X-Merchant: demo' -d '{"ids":["att_wobrpo4mqnbswocmcqr4q3skvc","att_zoiihfw2o25kmzvy6fwdyoaimm","att_avfbe5a6czlr6dgack3q27z6dd"]}' localhost:8102/api/owners/product/p1/gallery/order
```

```text
{"error":"records: reorder must list each of the 3 attachments exactly once","code":"invalid_argument"}
204
```

Remove the side photo. The generic trash route is refused by `Authorize`; the purge route deletes it, and its thumbnail's public URL stops working:

```sh
curl -s -X DELETE -H 'X-Merchant: demo' localhost:8102/api/files/fil_coaywfbnpriqxbzshdalznv6yq
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H 'X-Merchant: demo' localhost:8102/products/p1/images/fil_coaywfbnpriqxbzshdalznv6yq
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8102/public/variants/fil_coaywfbnpriqxbzshdalznv6yq/thumb-bc5aa5507109ef64-553pyeu4cs7udomypccsfgbvu3.jpg
curl -s localhost:8102/api/owners/product/p1/gallery
```

```text
{"error":"forbidden","code":"permission"}
204
404
{"files":[{"attachment_id":"att_wobrpo4mqnbswocmcqr4q3skvc","file_id":"fil_35jj2yfmlwy4i37kif3jonuj7m","filename":"back.png","content_type":"image/png","size":1189913,"position":1,"url":"http://localhost:8102/public/products/7oaqymjfnao4vjgkr7mqbkz7qx.png","status":"ready"},{"attachment_id":"att_zoiihfw2o25kmzvy6fwdyoaimm","file_id":"fil_ie6tlocmo6r4kqpppqhuzjhfin","filename":"front.jpg","content_type":"image/jpeg","size":372152,"position":2,"url":"http://localhost:8102/public/products/tyyss6aonohqy54wqi3dv7evfl.jpg","status":"ready"}]}
```

Check: the PNG is now position 1, the cover.

A request with one bad file stores nothing:

```sh
printf '<svg onload=alert(1)>' > x.svg
curl -s -H 'X-Merchant: demo' -F file=@front.jpg -F file=@x.svg localhost:8102/api/owners/product/p2/gallery
curl -s localhost:8102/products/p2
```

```json
{"error":"content type \"image/svg+xml\" is not allowed","code":"rejected"}
{"product":"p2","cover":null,"images":[]}
```

## Edge cases this handles

- One bad file in a multi-file upload: the whole request fails and the files already stored are deleted on rollback. For "store what you can" bulk imports, see `HandlerOptions.Partial` in [uploads](../concepts/uploads.md).
- Two merchants reordering at once: each `Reorder` is one transaction over the complete list; the last one wins, and a list made stale by a concurrent upload or removal is refused.
- A shopper opening zoom on many images at once: concurrent first requests for one variant are safe; both may generate, one row wins.
- Changing a size: its digest changes, so new objects are made on the next request and the CDN never serves a stale image under a new name. `Plugin.Prune` removes the old ones.
- SVG, HTML and anything with script: refused by the default deny list, whatever the name.

## For production

- Authenticate: replace `isMerchant` with your session check, and in a marketplace check in `Authorize` that the product (`Target.Owner.ID`) belongs to the merchant's shop.
- Put the disk on S3, R2 or GCS with `WithPublicURL` set to the CDN, and drop `fshttp.Public`. Variant keys contain the transform's digest, so they can be cached for a year; see [public CDN](public-cdn.md) for `Cache-Control`.
- Run `Plugin.Prune` after deploying a changed size, and `Store.SweepTrash` on a schedule if other parts of the app trash files.
- Large catalog imports: generate eager sizes off the request path by importing in a job, or turn eager off for the import and let first requests generate.
- Per-shop storage limits: install the `quota` plugin ([quota](../plugins/quota.md)).

## Related

- [Avatars](avatars.md): one image per owner, with a user-chosen crop.
- [Public CDN](public-cdn.md): caching headers, public and private disks.
- [Strip photo metadata](strip-photo-metadata.md): what Sanitize removes, and where it does not run.
- [Attachments](../records/attachments.md): roles, positions, `Reorder`.
- [Variants](../plugins/variants.md) and [HTTP endpoints](../records/http-endpoints.md).
