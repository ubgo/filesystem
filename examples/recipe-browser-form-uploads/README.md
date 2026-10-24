# Browser form uploads the bucket checks

A browser uploads an avatar straight to the bucket with a presigned POST form, so the file never passes through the application and the bucket itself refuses a file of the wrong size or type. The application issues the form (`upload.Presign` with `Policy.PostForm`), serves a page whose script posts it, and confirms the stored object afterwards (`upload.ConfirmExpected`). It uses the core `filesystem` package, `github.com/ubgo/filesystem/upload`, `github.com/ubgo/filesystem/fshttp` and `github.com/ubgo/filesystem/adapters/s3`. The guides [Uploads](../../docs/concepts/uploads.md#form-uploads-the-bucket-checks) and [Amazon S3](../../docs/adapters/s3.md#presigned-post-forms) explain each step.

## Run it

```sh
cd examples
go run ./recipe-browser-form-uploads
go test ./recipe-browser-form-uploads
```

By default it runs against an in-process fake S3 ([gofakes3](https://github.com/johannesboyne/gofakes3)). The fake accepts form uploads but does not check the policy, so the oversize upload is stored, and only `Confirm` catches it (and deletes it):

```text
page: 823 bytes, form posts to the bucket from script
ticket: POST 127.0.0.1 key=avatars/<random>.png
bucket accepted the form: 200
confirm: 200 image/png 67 bytes, key avatars/<random>.png
oversize upload: accepted by the store (200), which does not check the policy
confirm: 415 stored size 1091 is not the 67 bytes declared (object kept: false)
```

Against a real store, the bucket refuses the oversize upload itself. Set `FILESYSTEM_S3_TEST_DSN` (and the AWS key variables) to run it there; this is RustFS from the repository's `compose.yml`, started and removed around the run:

```sh
docker compose --profile rustfs up -d
cd examples
AWS_ACCESS_KEY_ID=filesystem AWS_SECRET_ACCESS_KEY=filesystem-secret \
FILESYSTEM_S3_TEST_DSN='s3://avatars-demo?region=us-east-1&endpoint=http://127.0.0.1:19333&path_style=true' \
go run ./recipe-browser-form-uploads
docker compose --profile rustfs down -v
```

```text
page: 823 bytes, form posts to the bucket from script
ticket: POST 127.0.0.1 key=avatars/<random>.png
bucket accepted the form: 204
confirm: 200 image/png 67 bytes, key avatars/<random>.png
oversize upload: refused by the bucket (400)
```

(The AWS SDK also prints a `DEBUG Response has no supported checksum` line to standard error with RustFS; it is the SDK's, not an error.) `TestPresignPostAgainstARealStore` in the S3 driver checks the other refusals against SeaweedFS and RustFS: another content type, another key, and an expired form.

## What it does

1. The policy: one PNG of at most 2 MiB, stored under a random name in `avatars/`, uploaded as a form the bucket checks:

<!-- ds:block id=avatarpolicy-4w4dsk4r -->
**avatarPolicy** · [`examples/recipe-browser-form-uploads/main.go:145-153`](main.go#L145-L153)

```go
func avatarPolicy() upload.Policy {
	return upload.Policy{
		MaxSize:  avatarMaxSize,
		MinSize:  1,
		Allowed:  []string{avatarType},
		Key:      upload.RandomKey("avatars"),
		PostForm: true,
	}
}
```
<!-- /ds:block hash=0717e3 -->

2. The ticket. The browser declares the file's name, type and size; `upload.Presign` checks them against the policy and signs a form for exactly that size and type under a key the application chooses:

<!-- ds:block id=issueticket-8tsaxjxf -->
**issueTicket** · [`examples/recipe-browser-form-uploads/main.go:172-184`](main.go#L172-L184)

```go
func issueTicket(w http.ResponseWriter, r *http.Request, disk *filesystem.Disk) {
	var req upload.TicketRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad ticket request", http.StatusBadRequest)
		return
	}
	ticket, err := upload.Presign(r.Context(), disk, req, avatarPolicy(), ticketLifetime)
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	writeJSON(w, ticket)
}
```
<!-- /ds:block hash=66b9ba -->

3. The page. Its script asks for a ticket, posts every field and then the file (last) to the bucket, and asks for confirmation. A real deployment also sets CORS on the bucket for `POST` from the page's origin (`s3.SetCORS` with `s3.DirectUploadCORS`):

<!-- ds:block id=page-vt4vb36v -->
**page** · [`examples/recipe-browser-form-uploads/main.go:221-238`](main.go#L221-L238)

```go
var page = template.Must(template.New("page").Parse(`<!doctype html>
<input type="file" id="file" accept="{{.Accept}}">
<script>
document.getElementById("file").onchange = async (e) => {
  const file = e.target.files[0];
  const ticket = await (await fetch("{{.Ticket}}", {method: "POST",
    body: JSON.stringify({filename: file.name, content_type: file.type, size: file.size})})).json();
  const form = new FormData();
  for (const [name, value] of Object.entries(ticket.fields)) form.append(name, value);
  form.append("file", file);
  const sent = await fetch(ticket.url, {method: "POST", body: form});
  if (!sent.ok) throw new Error("upload refused: " + sent.status);
  const done = await fetch("{{.Confirm}}", {method: "POST",
    body: JSON.stringify({key: ticket.key, filename: file.name, size: file.size})});
  console.log(await done.json());
};
</script>
`))
```
<!-- /ds:block hash=2b99c5 -->

4. The confirmation re-reads the stored object, checks its type from its bytes and its size against both the policy and what the browser declared, and deletes it when either is wrong. A real application also checks that the key is one it issued to this user:

<!-- ds:block id=confirmupload-nxz3g54m -->
**confirmUpload** · [`examples/recipe-browser-form-uploads/main.go:198-210`](main.go#L198-L210)

```go
func confirmUpload(w http.ResponseWriter, r *http.Request, disk *filesystem.Disk) {
	var req confirmRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad confirm request", http.StatusBadRequest)
		return
	}
	res, err := upload.ConfirmExpected(r.Context(), disk, req.Key, req.Filename, avatarPolicy(), upload.Expected{Size: req.Size})
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	writeJSON(w, res)
}
```
<!-- /ds:block hash=003e02 -->

The program then plays the browser over HTTP (the `browser` type does what the page's script does): one avatar inside the policy, then one whose bytes are 1 KiB larger than the size it declared.
