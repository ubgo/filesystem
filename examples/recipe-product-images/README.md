# Product images: an ordered e-commerce gallery with resized variants

The image gallery of an e-commerce catalog: a merchant uploads several photos of a product in one request, puts them in order (position 1 is the cover), and removes one for good, while the storefront gets a thumbnail grid, a product card and a zoomable large image, all from public URLs a CDN can cache. It uses `records` (files attached to a product under the `gallery` role, with positions), `records/recordshttp` (the upload, list and reorder endpoints behind an `Authorize` hook), `contrib/imaging` (`Sanitize` re-encodes every upload, dropping metadata) and its `variants` plugin (thumb and card made eagerly at upload, zoom made on first request), `fshttp.Public` and a public `local` disk. The guide is [Product images](../../docs/recipes/product-images.md).

## Run it

```sh
cd examples
go run ./recipe-product-images
```

It creates a temporary directory (printed at start-up) holding the files and `catalog.db` (SQLite) and listens on `http://localhost:8102`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Authentication is a stand-in: a request carrying `X-Merchant: demo` is the merchant, anything else a shopper.

Run the tests with `go test ./recipe-product-images`.

## Walkthrough

The photos used below are a 1200 × 900 JPEG (`front.jpg`), a 600 × 1400 JPEG (`side.jpg`) and a 1000 × 1000 PNG (`back.png`). Sizes in the responses are after `Sanitize` re-encoded them.

1. A shopper cannot upload; the merchant uploads three photos in one request:

```sh
curl -s -F file=@front.jpg localhost:8102/api/owners/product/p1/gallery
curl -s -H 'X-Merchant: demo' -F file=@front.jpg -F file=@side.jpg -F file=@back.png localhost:8102/api/owners/product/p1/gallery
```

```json
{"error":"forbidden","code":"permission"}
{"files":[{"attachment_id":"att_5okfy6ql22jtx4fz4pad3qvsn2","file_id":"fil_pukfvxl5ezhn4lkioudyxecbk7","filename":"front.jpg","content_type":"image/jpeg","size":372152,"position":1,"status":"ready"},{"attachment_id":"att_sg5tqytzkuku5uhmca57ek4fnh","file_id":"fil_aeno7ztzpcfqqddco2csnw3pvp","filename":"side.jpg","content_type":"image/jpeg","size":293747,"position":2,"status":"ready"},{"attachment_id":"att_kqd5hxganecwo47hbaxgw73pym","file_id":"fil_24xwaqjl7tafvu5atfa2texwzd","filename":"back.png","content_type":"image/png","size":1189913,"position":3,"status":"ready"}]}
```

2. The storefront view; the thumb and card are already public objects (first image shown):

```sh
curl -s localhost:8102/products/p1
```

```json
{
    "product": "p1",
    "cover": {
        "file_id": "fil_pukfvxl5ezhn4lkioudyxecbk7",
        "thumb": "http://localhost:8102/public/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/thumb-bc5aa5507109ef64-wzp5eslap2aohk467fqc6mzrba.jpg",
        "card": "http://localhost:8102/public/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/card-740e6aabff1017f4-ejboxkkiarjmy7lhb5v6k7a2dh.jpg",
        "zoom": "/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/zoom"
    },
    "images": [ ... ]
}
```

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' localhost:8102/public/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/thumb-bc5aa5507109ef64-wzp5eslap2aohk467fqc6mzrba.jpg
```

```text
200 image/jpeg 3005
```

3. Opening the zoom generates it once and redirects to the public object; an undeclared size does not exist:

```sh
curl -s -i localhost:8102/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/zoom | grep -iE 'HTTP|cache-control|location'
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -L localhost:8102/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/zoom
curl -s -i localhost:8102/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/huge | head -1
```

```text
HTTP/1.1 302 Found
Cache-Control: private, max-age=60
Location: http://localhost:8102/public/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/zoom-16e182ee93caa12d-smcz3ihbmzxjrowlakaktjdxlf.jpg
<a href="http://localhost:8102/public/variants/fil_pukfvxl5ezhn4lkioudyxecbk7/zoom-16e182ee93caa12d-smcz3ihbmzxjrowlakaktjdxlf.jpg">Found</a>.
200 image/jpeg 372179
HTTP/1.1 404 Not Found
```

4. Make the PNG the cover. A list with an image missing is refused; the complete list is applied:

```sh
curl -s -X PUT -H 'X-Merchant: demo' -d '{"ids":["att_kqd5hxganecwo47hbaxgw73pym","att_5okfy6ql22jtx4fz4pad3qvsn2"]}' localhost:8102/api/owners/product/p1/gallery/order
curl -s -o /dev/null -w '%{http_code}\n' -X PUT -H 'X-Merchant: demo' -d '{"ids":["att_kqd5hxganecwo47hbaxgw73pym","att_5okfy6ql22jtx4fz4pad3qvsn2","att_sg5tqytzkuku5uhmca57ek4fnh"]}' localhost:8102/api/owners/product/p1/gallery/order
```

```text
{"error":"records: reorder must list each of the 3 attachments exactly once","code":"invalid_argument"}
204
```

5. Remove the side photo. The generic trash route is refused by `Authorize`; the app's purge route deletes the file and its variants, so its thumbnail URL stops working:

```sh
curl -s -X DELETE -H 'X-Merchant: demo' localhost:8102/api/files/fil_aeno7ztzpcfqqddco2csnw3pvp
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H 'X-Merchant: demo' localhost:8102/products/p1/images/fil_aeno7ztzpcfqqddco2csnw3pvp
curl -s -o /dev/null -w '%{http_code}\n' localhost:8102/public/variants/fil_aeno7ztzpcfqqddco2csnw3pvp/thumb-bc5aa5507109ef64-dxmfhyfdxlkb4va6hzxqug433n.jpg
curl -s localhost:8102/api/owners/product/p1/gallery
```

```text
{"error":"forbidden","code":"permission"}
204
404
{"files":[{"attachment_id":"att_kqd5hxganecwo47hbaxgw73pym","file_id":"fil_24xwaqjl7tafvu5atfa2texwzd","filename":"back.png","content_type":"image/png","size":1189913,"position":1,"url":"http://localhost:8102/public/products/kcxrvvbyzn565n4qg25wejhgbn.png","status":"ready"},{"attachment_id":"att_5okfy6ql22jtx4fz4pad3qvsn2","file_id":"fil_pukfvxl5ezhn4lkioudyxecbk7","filename":"front.jpg","content_type":"image/jpeg","size":372152,"position":2,"url":"http://localhost:8102/public/products/qo4kp74wvr4mp5y5fkx7ccockn.jpg","status":"ready"}]}
```

6. A request with one bad file stores nothing:

```sh
printf '<svg onload=alert(1)>' > x.svg
curl -s -H 'X-Merchant: demo' -F file=@front.jpg -F file=@x.svg localhost:8102/api/owners/product/p2/gallery
curl -s localhost:8102/products/p2
```

```json
{"error":"content type \"image/svg+xml\" is not allowed","code":"rejected"}
{"product":"p2","cover":null,"images":[]}
```

## The code

`imageLimits` and `galleryPolicy`: JPEG, PNG and WebP up to 15 MB, detected from the bytes, stored public under random keys, and re-encoded by `imaging.Sanitize` so no EXIF or GPS data reaches the storefront.

<!-- ds:block id=imagelimits-6yvh999c -->
**imageLimits** · [`examples/recipe-product-images/main.go:87`](main.go#L87-L87)

```go
var imageLimits = imaging.Options{MaxBytes: maxImageBytes}
```
<!-- /ds:block hash=2bce3d -->

<!-- ds:block id=gallerypolicy-59s4f88q -->
**galleryPolicy** · [`examples/recipe-product-images/main.go:92-98`](main.go#L92-L98)

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

`sizes` declares the three variants the storefront uses; a name not listed here is a `404`.

<!-- ds:block id=sizes-yj3z9sby -->
**sizes** · [`examples/recipe-product-images/main.go:104-108`](main.go#L104-L108)

```go
var sizes = map[string]imaging.Transform{
	sizeThumb: {Mode: imaging.ModeFill, Width: 200, Height: 200, Format: imaging.FormatJPEG},
	sizeCard:  {Mode: imaging.ModePad, Width: 600, Height: 600, Format: imaging.FormatJPEG},
	sizeZoom:  {Mode: imaging.ModeFit, Width: 1600, Height: 1600, Format: imaging.FormatJPEG, Quality: 90},
}
```
<!-- /ds:block hash=54474a -->

`openDisk` makes the public disk, whose `URL` points at the `/public` route; `newVariants` makes thumb and card eagerly at upload (logging a failure rather than failing the upload) and leaves zoom for the first request.

<!-- ds:block id=opendisk-jgkb2fbf -->
**openDisk** · [`examples/recipe-product-images/main.go:120-131`](main.go#L120-L131)

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

<!-- ds:block id=newvariants-22rj7var -->
**newVariants** · [`examples/recipe-product-images/main.go:137-142`](main.go#L137-L142)

```go
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(sizes, imaging.Options{}, variants.WithEager(logFailure, sizeThumb, sizeCard))
}
```
<!-- /ds:block hash=a49d7b -->

`openStore` opens the records store with the variants plugin installed and runs its migrations.

<!-- ds:block id=openstore-jhrmunv8 -->
**openStore** · [`examples/recipe-product-images/main.go:146-160`](main.go#L146-L160)

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

`isMerchant` and `authorize` are the `recordshttp` hook: only product galleries, anyone may list and read, only the merchant may upload and reorder, and every other action (trash among them) is refused.

<!-- ds:block id=ismerchant-uvt2f22h -->
**isMerchant** · [`examples/recipe-product-images/main.go:168`](main.go#L168-L168)

```go
func isMerchant(r *http.Request) bool { return r.Header.Get(headerMerchant) == merchantToken }
```
<!-- /ds:block hash=e4894e -->

<!-- ds:block id=authorize-f9wmr8n3 -->
**authorize** · [`examples/recipe-product-images/main.go:176-189`](main.go#L176-L189)

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

`galleryAPI` is the generic records HTTP API configured with the policy, at most ten files per request, and the hook.

<!-- ds:block id=galleryapi-dhdu2xrj -->
**galleryAPI** · [`examples/recipe-product-images/main.go:194-204`](main.go#L194-L204)

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

`New` mounts the API, the storefront view, the removal route, the variants handler and the public files.

<!-- ds:block id=new-y9s4sspz -->
**New** · [`examples/recipe-product-images/main.go:209-235`](main.go#L209-L235)

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

`image` and `product` build the storefront view: the gallery in position order, the first image as the cover, each with its thumb, card and zoom URLs.

<!-- ds:block id=app-image-sgkz2taq -->
**App.image** · [`examples/recipe-product-images/main.go:257-267`](main.go#L257-L267)

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
**App.product** · [`examples/recipe-product-images/main.go:273-293`](main.go#L273-L293)

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

`removeImage` checks the image belongs to the product and purges it: row, object and variants go together, with no trash.

<!-- ds:block id=app-removeimage-m7ak2yp4 -->
**App.removeImage** · [`examples/recipe-product-images/main.go:300-321`](main.go#L300-L321)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, the variant sizes, the disk and store, the `Authorize` hook, the storefront view, image removal and the server. |
| `main_test.go` | The gallery flow (upload, view, variants, reorder, remove) and the refusals (shoppers, bad files, the trash route, unknown sizes). |

## Next

- The guide: [Product images](../../docs/recipes/product-images.md), with edge cases and production notes.
- [Attachments](../../docs/records/attachments.md), [Variants](../../docs/plugins/variants.md) and [HTTP endpoints](../../docs/records/http-endpoints.md).
- Related recipes: [recipe-avatars](../recipe-avatars) (one image per owner, with a crop), [recipe-public-cdn](../recipe-public-cdn) (caching headers, public and private disks), [recipe-strip-photo-metadata](../recipe-strip-photo-metadata).
