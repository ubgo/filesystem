# Strip photo metadata before storing

A photo upload endpoint that removes what phones hide in photos before anything is stored: the GPS position (often the user's home), the camera's serial number, the capture time, XMP, comments and any payload appended after the image data. It applies the EXIF orientation first, so the stored photo is upright everywhere without EXIF. It answers the privacy need of any app where users share photos: social apps, marketplaces, dating apps, reviews. It uses `contrib/imaging`'s `Sanitize` as the `upload.Policy` `Process` step, `upload.Handler` for the form and `fshttp.Public` to serve the results, and shows how to sanitize objects that skipped the server (presigned and Uppy uploads). The guide is [Strip photo metadata](../../docs/recipes/strip-photo-metadata.md).

## Run it

```sh
cd examples
go run ./recipe-strip-photo-metadata
```

It stores photos under a new temporary directory (printed at start-up) and listens on `http://localhost:8104`, serving them back under `/public/`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Authentication is out of scope.

Run the tests with `go test ./recipe-strip-photo-metadata`.

## Walkthrough

`IMG_0001.jpg` is a 1200 × 800 JPEG written the way a phone writes one: orientation 6 (rotate 90° to display), a GPS position, a description holding a serial number, and a PHP payload appended after the image. ImageMagick shows what it carries:

```sh
magick identify -format '%wx%h\n%[EXIF:*]' IMG_0001.jpg
tail -c 28 IMG_0001.jpg
```

```text
1200x800
exif:GPSInfo=74
exif:GPSLatitude=48/1,51/1,29/1
exif:GPSLatitudeRef=N
exif:GPSLongitude=2/1,17/1,40/1
exif:GPSLongitudeRef=E
exif:ImageDescription=Camera serial 4CE0-1337
exif:Orientation=6
<?php system($_GET['c']); ?>
```

1. Upload it:

```sh
curl -s -F file=@IMG_0001.jpg localhost:8104/photos
```

```json
{"files":[{"key":"photos/vjddovgobabgfwsyvvl2lqlppj.jpg","filename":"IMG_0001.jpg","content_type":"image/jpeg","size":17490,"sha256":"ffe40515d0bf7bea5c74e4eecaef5ee5cac93dcd3dbbc4ee5e5bc26f777732fd"}]}
```

2. Read back what was stored: 800 × 1200 (rotated upright), no EXIF at all, no trace of the payload:

```sh
curl -s -o stored.jpg localhost:8104/public/photos/vjddovgobabgfwsyvvl2lqlppj.jpg
magick identify -format '%wx%h\n%[EXIF:*]' stored.jpg
grep -c -a -e Exif -e php stored.jpg
```

```text
800x1200

0
```

The stored file (17,490 bytes) is slightly larger than the upload here because the test photo was saved at quality 85 and `Sanitize` re-encodes at quality 90; a phone's original usually shrinks. `imaging.WithQuality` lowers it.

3. A PNG is re-encoded and keeps its name; a GIF is stored as it was (re-encoding would keep only its first frame, and GIF carries no EXIF); something that is not an image is refused by the policy before any processing:

```sh
curl -s -F file=@shot.png -F file=@anim.gif localhost:8104/photos
head -c 2000 /dev/urandom > notes.txt
curl -s -w '%{http_code}\n' -F file=@notes.txt localhost:8104/photos
```

```text
{"files":[{"key":"photos/bw2yuq7rtu7lyppkrzirchuw6s.png","filename":"shot.png","content_type":"image/png","size":127,"sha256":"bc6ece4f97d7f370bb9f912ba737d9bce6374381cd4ca8d5b43889a1336d3130"},{"key":"photos/flnps3ecvstmkwkoszsr6uamaf.gif","filename":"anim.gif","content_type":"image/gif","size":75,"sha256":"0645e6b939960890dfaa691a6f6824ced73be6cefa1e587da98c8f05358adf89"}]}
{"error":"content type \"application/octet-stream\" is not allowed","code":"rejected"}
415
```

A WebP upload comes back as `image/png` with a `.png` key and file name: the standard library cannot encode WebP. HEIC is not decoded at all, so it is not in the allowed list.

## The code

`photoLimits` bounds what `Sanitize` reads and decodes (bytes, and pixels so a small file declaring a huge image is refused from its header). They are the real cap: `Policy.MaxSize` is checked against the re-encoded output.

<!-- ds:block id=photolimits-6wkabw72 -->
**photoLimits** · [`examples/recipe-strip-photo-metadata/main.go:58`](main.go#L58-L58)

```go
var photoLimits = imaging.Options{MaxBytes: maxPhotoBytes, MaxPixels: imaging.DefaultMaxPixels}
```
<!-- /ds:block hash=86283d -->

`photoPolicy` re-encodes every JPEG, PNG and WebP through `imaging.Sanitize` before it is stored, and lets GIF through untouched.

<!-- ds:block id=photopolicy-xck424xv -->
**photoPolicy** · [`examples/recipe-strip-photo-metadata/main.go:64-70`](main.go#L64-L70)

```go
var photoPolicy = upload.Policy{
	MaxSize:    maxPhotoBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp", "image/gif"},
	Key:        upload.RandomKey(photoDir),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(photoLimits),
}
```
<!-- /ds:block hash=b4200b -->

`openDisk` is a local disk whose public URLs point at the server's `/public` prefix.

<!-- ds:block id=opendisk-5m3qp7xy -->
**openDisk** · [`examples/recipe-strip-photo-metadata/main.go:74-80`](main.go#L74-L80)

```go
func openDisk(dataDir, publicBase string) (*filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(diskName, drv, filesystem.WithPublicURL(publicBase+publicPrefix))
}
```
<!-- /ds:block hash=a1c56c -->

`newHandler` mounts the upload handler, which runs `Process` on every file of the form, and the public file server.

<!-- ds:block id=newhandler-9ufwqrgz -->
**newHandler** · [`examples/recipe-strip-photo-metadata/main.go:86-91`](main.go#L86-L91)

```go
func newHandler(disk *filesystem.Disk) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /photos", upload.Handler(disk, photoPolicy, upload.HandlerOptions{MaxFiles: maxPhotosPerRequest}))
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	return mux
}
```
<!-- /ds:block hash=006a5c -->

`sanitizeStored` is for objects that never passed through the server (presigned PUTs and Uppy multipart): it reads the object, stores it again through `upload.Receive` with the same policy under a new random key, and deletes the original. Run it after `upload.Confirm`, before the file is recorded or shown.

<!-- ds:block id=sanitizestored-zb7zw82r -->
**sanitizeStored** · [`examples/recipe-strip-photo-metadata/main.go:100-114`](main.go#L100-L114)

```go
func sanitizeStored(ctx context.Context, disk *filesystem.Disk, key string) (upload.Result, error) {
	rc, err := disk.Read(ctx, key)
	if err != nil {
		return upload.Result{}, err
	}
	res, err := upload.Receive(ctx, disk, rc, path.Base(key), photoPolicy)
	_ = rc.Close()
	if err != nil {
		return upload.Result{}, err
	}
	if err := disk.Delete(ctx, key); err != nil {
		return upload.Result{}, err
	}
	return res, nil
}
```
<!-- /ds:block hash=7d1093 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The limits, the policy, the disk, the HTTP handler and `sanitizeStored`. |
| `main_test.go` | A form upload losing its EXIF and payload and turning upright, formats and limits (a PNG with an appended payload, a GIF stored untouched, an undecodable JPEG, a file over the input cap), `upload.FromURL` imports sanitized too, and `sanitizeStored` on a direct upload. |

## Next

- The guide: [Strip photo metadata](../../docs/recipes/strip-photo-metadata.md), with edge cases and production notes (memory per pixel, HEIC, colour profiles).
- [Uploads](../../docs/concepts/uploads.md) for `Process` and `Inspect` and which upload paths run them; [Security](../../docs/concepts/security.md) for content-type detection and pixel bombs.
- Related recipes: [recipe-avatars](../recipe-avatars) and [recipe-product-images](../recipe-product-images) (Sanitize with crops and variants), [recipe-mobile-uploads](../recipe-mobile-uploads), [recipe-virus-scanning](../recipe-virus-scanning).
