# Strip photo metadata

A photo straight from a phone carries an EXIF block: the GPS position where it was taken (for most photos, the user's home), the capture time, the camera model and serial number, sometimes a thumbnail of the uncropped original. Publishing it publishes all of that. Files can also carry a payload appended after the image data, which an image viewer ignores and a misconfigured server may execute. This recipe is for every app that accepts photos from users and shows them to anyone else: social feeds, marketplaces, dating, real estate, support tickets with screenshots.

The program is [examples/recipe-strip-photo-metadata](../../examples/recipe-strip-photo-metadata). Every code block below is copied from it, and every transcript is from running it.

## What you get

- Every JPEG, PNG and WebP is decoded and re-encoded before it is stored. Nothing the original carried besides its pixels survives: EXIF, GPS, XMP, colour profiles, comments, embedded thumbnails, appended data.
- The EXIF orientation is applied first, so a portrait photo the phone stored sideways is stored upright, and displays correctly in every browser and image library, including ones that ignore EXIF.
- The same processing runs on form uploads, imports from a URL and resumable (tus) uploads, because all three go through `upload.Receive`.
- Direct-to-bucket uploads, which never pass through the server, are sanitized after the fact by a small promote step.

## The design

| Piece | Why |
|---|---|
| `imaging.Sanitize(opts)` as `Policy.Process` | An `upload.Processor` that re-encodes. JPEG stays JPEG at quality 90 (`imaging.SanitizeQuality`, or another with `imaging.WithQuality(q)`); PNG, WebP, BMP and TIFF become PNG, the lossless format Go's standard library can write, and the file name's extension follows. GIF and non-image types pass through untouched. |
| `imaging.Options.MaxBytes` and `MaxPixels` | The processor reads the whole upload and decodes it, so these are the real limits: bytes read, and pixels decoded (a small file can declare a huge image). |
| Re-checking after processing | `upload.Receive` checks the type the processor reports against the policy again, and applies `MaxSize` to what it returns. |
| `sanitizeStored` | For presigned and Uppy uploads, which go straight to the bucket: read the object back, store it again through the same policy, delete the original. |

## Step 1: The policy

<!-- ds:block id=photolimits-6wkabw72 -->
**photoLimits** · [`examples/recipe-strip-photo-metadata/main.go:58`](../../examples/recipe-strip-photo-metadata/main.go#L58-L58)

```go
var photoLimits = imaging.Options{MaxBytes: maxPhotoBytes, MaxPixels: imaging.DefaultMaxPixels}
```
<!-- /ds:block hash=86283d -->

<!-- ds:block id=photopolicy-xck424xv -->
**photoPolicy** · [`examples/recipe-strip-photo-metadata/main.go:64-70`](../../examples/recipe-strip-photo-metadata/main.go#L64-L70)

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

The pitfall is `MaxSize`. With a processor set, it is checked against the processed output, not against what the client sent: a 40 MB upload that re-encodes to 3 MB passes a 10 MB `MaxSize`. The bound on the input is `imaging.Options.MaxBytes` (refused with `CodeTooLarge` before decoding), plus any `http.MaxBytesReader` you put in front. Set `MaxBytes` explicitly; its default is 50 MiB.

GIF is in `Allowed` and passes through: re-encoding would keep only the first frame of an animation, and GIF has no EXIF. If you do not want animations, leave `image/gif` out. Types `Sanitize` does not handle (a PDF on a policy that also allows documents) pass through as they are; strip document metadata separately if it matters to you.

## Step 2: The upload endpoint

<!-- ds:block id=opendisk-5m3qp7xy -->
**openDisk** · [`examples/recipe-strip-photo-metadata/main.go:74-80`](../../examples/recipe-strip-photo-metadata/main.go#L74-L80)

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

<!-- ds:block id=newhandler-9ufwqrgz -->
**newHandler** · [`examples/recipe-strip-photo-metadata/main.go:86-91`](../../examples/recipe-strip-photo-metadata/main.go#L86-L91)

```go
func newHandler(disk *filesystem.Disk) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /photos", upload.Handler(disk, photoPolicy, upload.HandlerOptions{MaxFiles: maxPhotosPerRequest}))
	mux.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(disk)))
	return mux
}
```
<!-- /ds:block hash=006a5c -->

`upload.Handler` streams each file of the form through the policy. With `Sanitize` the file is held in memory while it is decoded, because a JPEG cannot be re-encoded as a stream. The same policy given to `records.Store.Upload`, `recordshttp`, `upload.FromURL` or `contrib/tus` sanitizes there too; the example's tests check the import path ([import from URL](import-from-url.md)).

## Step 3: Uploads that bypass the server

`upload.Presign` and `upload.Uppy` hand the browser a URL on the bucket, so the bytes never pass through `Process`. `upload.Confirm` checks their size and type, and runs `Policy.Inspect`, but it cannot change them. Two options:

- Do not use direct uploads for photos that will be shown to other people. Use form uploads, or tus for large files on bad networks ([resumable uploads](resumable-uploads.md)).
- Or promote each confirmed upload through the policy before it is recorded:

<!-- ds:block id=sanitizestored-zb7zw82r -->
**sanitizeStored** · [`examples/recipe-strip-photo-metadata/main.go:100-114`](../../examples/recipe-strip-photo-metadata/main.go#L100-L114)

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

Point direct uploads at a staging prefix (`incoming/`) that is never public and never recorded, run `upload.Confirm`, then `sanitizeStored`, and record only the result. `upload.Sweep` on the staging prefix removes uploads nobody confirmed ([browser direct uploads](browser-direct-uploads.md)).

## Try it

```sh
cd examples
go run ./recipe-strip-photo-metadata
```

```text
listening on http://localhost:8104 data in /var/folders/sk/.../T/recipe-strip-photo-metadata-2112750197
```

`IMG_0001.jpg` is a 1200 × 800 JPEG as a phone writes it, with orientation 6 (rotate 90° to display), a GPS position, a description holding a serial number, and a PHP payload appended after the image. ImageMagick shows the EXIF:

```sh
magick identify -format '%wx%h\n%[EXIF:*]' IMG_0001.jpg
tail -c 28 IMG_0001.jpg
```

```text
1200x800
exif:GPSInfo=74
exif:GPSLatitude=48/1,51/1,29/1
exif:GPSLatitudeRef=N
exif:ImageDescription=Camera serial 4CE0-1337
exif:Orientation=6
<?php system($_GET['c']); ?>
```

Upload it and read back what was stored:

```sh
curl -s -F file=@IMG_0001.jpg localhost:8104/photos
```

```json
{"files":[{"key":"photos/yoddvadvxtx6nsch6ntufjws64.jpg","filename":"IMG_0001.jpg","content_type":"image/jpeg","size":65648,"sha256":"7631a7b78a0fb16341a9076ade97ba5823bab4ef73a1a2e9353969ac09d5a88b"}]}
```

```sh
curl -s -o stored.jpg localhost:8104/public/photos/yoddvadvxtx6nsch6ntufjws64.jpg
magick identify -format '%wx%h\n%[EXIF:*]' stored.jpg
grep -c -a -e Exif -e php stored.jpg
```

```text
800x1200

0
```

Check: the stored photo is 800 × 1200, upright, with no EXIF at all and no trace of the payload.

A PNG is re-encoded and keeps its name; a GIF is stored as it was; something that is not an image is refused by the policy before any processing:

```sh
curl -s -F file=@shot.png -F file=@anim.gif localhost:8104/photos
head -c 2000 /dev/urandom > notes.txt
curl -s -F file=@notes.txt localhost:8104/photos
```

```json
{"files":[{"key":"photos/zqn2nuy2skijbfh4yvmxv3tztg.png","filename":"shot.png","content_type":"image/png","size":2127,"sha256":"8139709e2e596b629e5d8f3a0492cae66e5d381491668985253d3d29fd7842f2"},{"key":"photos/qcobkisnd3tyygped53falfzme.gif","filename":"anim.gif","content_type":"image/gif","size":80,"sha256":"e0de52c5edbff196112fa736c3c65ef2200a24139614e43c36346c9008cfb843"}]}
{"error":"content type \"application/octet-stream\" is not allowed","code":"rejected"}
```

A WebP upload would come back as `image/png` with a `.png` key and file name: the standard library cannot encode WebP.

## Edge cases this handles

- A file that claims to be a JPEG but does not decode: `415` (`CodeRejected`), nothing stored.
- A pixel bomb (a small file declaring 100,000 × 100,000 pixels): `415` (`CodeRejected`), refused from its header by `MaxPixels` before decoding.
- An upload over `MaxBytes`: `413`, refused while reading, before decoding.
- A payload appended after the image, or hidden in an EXIF comment: gone, because only the decoded pixels are written out.
- Orientations 2 to 8 (mirrored and rotated): all applied before re-encoding, so the stored file needs no EXIF to display correctly.
- A JPEG re-encoded at quality 90 is usually smaller than the phone's original, but not always; a stored file can be slightly larger than the upload. Photos already compressed harder grow; `imaging.WithQuality(q)` lowers the quality to keep them from growing.

## For production

- Memory: `Sanitize` holds the upload and the decoded image in memory, about 4 bytes per pixel, so a 50-megapixel photo can need around 200 MB while it is processed. Lower `MaxPixels` to what your product needs (a 12-megapixel phone camera is 4000 × 3000) and limit concurrent uploads per instance.
- HEIC (iPhone's default format) is not decoded by the standard library or `golang.org/x/image`; leave it out of `Allowed`, or have the app send JPEG (the iOS photo picker can be asked for a compatible representation; see [mobile uploads](mobile-uploads.md)).
- Keep the original only if you must, on a private disk, and never serve it: the point of the recipe is that nobody can retrieve the metadata.
- Colour profiles are dropped with the rest of the metadata; wide-gamut photos may look slightly less saturated. If that matters, process with a libvips-based `Processor` that keeps the ICC profile and drops the rest.

## Related

- [Avatars](avatars.md) and [product images](product-images.md): Sanitize together with crops and variants.
- [Uploads](../concepts/uploads.md): `Process`, `Inspect`, and which upload paths run them.
- [Security](../concepts/security.md): content-type detection, active content, pixel bombs.
- [Virus scanning](virus-scanning.md): inspecting files after they are stored.
