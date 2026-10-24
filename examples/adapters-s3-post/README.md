# Browser form uploads with a presigned POST

A presigned POST lets a browser upload straight to the bucket with a plain HTML form, while the bucket itself enforces what may be uploaded. This program signs a form for one PNG avatar of at most 2 MiB under `users/42/`, stored with a `Cache-Control` header and valid for ten minutes; renders the HTML form a page would serve; submits it the way a browser does; and confirms the stored object with `upload.Confirm`. It uses the core `filesystem` package, `github.com/ubgo/filesystem/adapters/s3` and `github.com/ubgo/filesystem/upload`. The guide [Amazon S3](../../docs/adapters/s3.md#presigned-post-forms) explains each condition.

## Run it

```sh
cd examples
go run ./adapters-s3-post
go test ./adapters-s3-post
```

It runs against an in-process fake S3 ([gofakes3](https://github.com/johannesboyne/gofakes3)), which stores form uploads but does not check the policy, so this program shows the happy path only. That real stores refuse an upload outside the policy (too large, another type, another key, an expired form) is shown by the driver's `TestPresignPostAgainstARealStore`, which passes against SeaweedFS and RustFS (`task test:s3`, `task test:rustfs`). The fake also drops `Cache-Control`, so the confirmed line does not print it; a real store keeps it.

## What it does

```text
form posts to http://127.0.0.1:51709/media
key field: users/42/${filename}
<form action="http://127.0.0.1:51709/media" method="post" enctype="multipart/form-data">
  <input type="hidden" name="Cache-Control" value="public, max-age=86400">
  <input type="hidden" name="Content-Type" value="image/png">
  <input type="hidden" name="X-Amz-Algorithm" value="AWS4-HMAC-SHA256">
  <input type="hidden" name="X-Amz-Credential" value="test/20261006/us-east-1/s3/aws4_request">
  <input type="hidden" name="X-Amz-Date" value="20261006T060101Z">
  <input type="hidden" name="X-Amz-Signature" value="4978d35a…">
  <input type="hidden" name="key" value="users/42/${filename}">
  <input type="hidden" name="policy" value="eyJjb25k…">
  <input type="file" name="file" accept="image/png">
  <button>Upload</button>
</form>
browser upload: 200
confirmed: users/42/avatar.png 67 bytes image/png
```

The port, date and signature change on every run; the printout shortens the policy and signature.

1. The server signs the form. Every condition is in the policy S3 checks and is also a field the browser sends back unchanged:

<!-- ds:block id=signavatarform-trkzpzgm -->
**signAvatarForm** · [`examples/adapters-s3-post/main.go:124-133`](main.go#L124-L133)

```go
func signAvatarForm(ctx context.Context, disk *filesystem.Disk, dir string) (filesystem.PresignedPost, error) {
	return disk.PresignPost(ctx, filesystem.PostPolicy{
		KeyPrefix:   dir,
		ContentType: avatarType,
		MinSize:     1,
		MaxSize:     avatarMaxSize,
		Expires:     formLifetime,
		Headers:     filesystem.HTTPHeaders{CacheControl: avatarCacheFor},
	})
}
```
<!-- /ds:block hash=996b1a -->

2. The page serves the form: one hidden input per field, then the file input, which must be last. The key field ends in `${filename}`, which S3 replaces with the chosen file's name; a script may instead set any key under `users/42/`.

<!-- ds:block id=renderform-63j5hz9w -->
**renderForm** · [`examples/adapters-s3-post/main.go:151-166`](main.go#L151-L166)

```go
func renderForm(out io.Writer, post filesystem.PresignedPost) error {
	fields := make([]formField, 0, len(post.Fields))
	for name, value := range post.Fields {
		if name == "policy" || name == "X-Amz-Signature" {
			value = value[:8] + "…" // shortened for the printout only
		}
		fields = append(fields, formField{name, value})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return formTemplate.Execute(out, struct {
		URL    string
		Fields []formField
		File   string
		Accept string
	}{post.URL, fields, filesystem.PostFileField, avatarType})
}
```
<!-- /ds:block hash=5a099e -->

3. The browser posts the fields and the file. This program does the same with `mime/multipart`, choosing the key `users/42/avatar.png`:

<!-- ds:block id=submit-we8jrp97 -->
**submit** · [`examples/adapters-s3-post/main.go:171-203`](main.go#L171-L203)

```go
func submit(ctx context.Context, post filesystem.PresignedPost, key, filename string, file []byte) (int, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, value := range post.Fields {
		if name == filesystem.PostFieldKey {
			value = key
		}
		if err := w.WriteField(name, value); err != nil {
			return 0, err
		}
	}
	fw, err := w.CreateFormFile(filesystem.PostFileField, filename)
	if err != nil {
		return 0, err
	}
	if _, err := fw.Write(file); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, post.URL, &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
```
<!-- /ds:block hash=c4b2c3 -->

4. The upload is the client's until the server has looked at it: `upload.Confirm` re-reads the stored object, checks its size and its type from its bytes, and deletes it if either is wrong.

A real page also needs CORS on the bucket for `POST` from its origin: `s3.SetCORS(ctx, client, bucket, s3.DirectUploadCORS("https://app.example.com"))`, a deploy-time step described in the guide.
