# Uploads: every way bytes arrive, under one policy

Every app that accepts files needs the same guarantees: a size limit enforced on the server, the type decided from the bytes rather than the client's claim, a random key instead of the client's file name, and nothing left behind when an upload is refused. This program walks each path through the `upload` package under one `upload.Policy`: `Receive` for a stream, the multipart form `Handler` (all or nothing, and `Partial`), direct uploads with `Presign` then `Confirm`, the Uppy endpoints, imports with `FromURL` and its SSRF guard, `Process` and `Inspect` hooks, `Sweep` for uploads nobody finished, then key layouts and collisions, base64 and data URIs, the size limits besides `MaxSize`, limits per plan with a rate limit, declared sizes and checksums, cache then promote, and the dry run of the abandoned-upload sweep. It uses the core `filesystem` package, `memory`, `upload`, `signedurl` and `fshttp`. The guide [Uploads](../../docs/concepts/uploads.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-uploads
go test ./concepts-uploads
```

It needs no environment variables and creates no files: each step uses a fresh memory disk. The direct-upload and URL-import steps start in-process `httptest` servers on random loopback ports and close them before the program exits. The direct-upload "bucket" is a memory disk whose links are signed by `signedurl` and served by `fshttp`, standing in for S3. The Uppy endpoints need a disk that can sign part uploads (the S3 driver), so on a memory disk they answer 501, which is what the program shows.

## What it does

```sh
go run ./concepts-uploads
```

```text
receive: key uploads/<random>.png name "Holiday Photo.PNG" type image/png size 29
receive html named photo.png: rejected
receive over MaxSize: too_large, objects stored: 1
form partial=false: 415, stored 0, recorded 0
  body {"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
form partial=true: 200, stored 2, recorded 2
  partial body: {"files":[{"key":"uploads/rkadxegj4waqpxdhrhi2y7x4c3.pdf","filename":"b.pdf","content_type":"application/pdf","size":24,"sha256":"9cc0121057382813f19c17ac79edbe548c4d06b0f374754e1565c2f3185d8f13"}],"failed":[{"filename":"page.html","error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}]}
direct: PUT 200, confirm "" type "image/png", object kept: true
direct: PUT 200, confirm "rejected" type "", object kept: false
presign declared text/html: rejected
uppy on memory: 501 {"error":"Not Implemented","code":"unsupported"}
from loopback: filesystem: invalid_argument: address is not on the public internet
from allowed server: "scan.pdf" application/pdf 24 bytes
PublicAddr(169.254.169.254): false
file URL: invalid_argument
processed: 5 bytes
inspected: rejected, objects stored: 1
swept 1, left 1
sweep the root: invalid_argument
sharded: 2 directory levels under photos/
collision rename: drive/rename/photo.png, drive/rename/photo (1).png
collision fail: drive/fail/photo.png, exists
base64: image/png, 29 bytes
data URI declared text/plain: stored as image/png
bad base64: invalid_argument
empty file: rejected
two files over MaxTotal 60: 413
body of 8388608 bytes: 413 after reading under 131072
plan free: 413 retry-after=""
plan paid: 201 retry-after=""
plan spammer: 503 retry-after="60"
declared sha256, sent 29 bytes: confirm "", kept true
declared sha256, sent 29 bytes: confirm "rejected", kept false
ticket for 24h0m0s: <nil>
ticket past MaxPresignExpiry: invalid_argument
promoted to avatars/u1.png, still in cache: false
promote a key outside the cache: permission
would remove: multipart upload of videos/big.mp4
swept 1 abandoned upload(s)
key from the request, tenant "acme": 201 tenants/acme/<random>.png
key from the request, tenant "globex": 201 tenants/globex/<random>.png
key from the request, tenant "": 403 (objects stored in all: 2)
through the proxy: "q3.pdf" application/pdf; proxy asked for addresses, never the name: true; User-Agent sent: true
metadata address through the proxy: invalid_argument, proxy asked: false
envelope respond=false: upload 201 files[1], records 201 files[1], share listing 200 [1]
envelope respond=true: upload 201 data.files[1], records 201 data.files[1], share listing 200 data[1]
public ticket: signs x-amz-acl true, client sends X-Amz-Acl: public-read
public ticket without AllowPublic: permission
ticket asking for nothing: signs x-amz-acl false
public ticket on a bucket with ACLs off: unsupported
```

The random part of each key changes on every run.

1. `Receive` stores a PNG under a random key in `uploads/`; the client's name `..\..\Holiday Photo.PNG` is reduced to a display name and never reaches the key. HTML named `photo.png` is refused, and a file over `MaxSize` is `too_large`; neither leaves an object (the one stored object is the accepted PNG).
2. The form handler stores all of a request's files or none of them, answering 415 when one is refused; with `Partial` it stores what it can and lists each failure with its own code. `OnStored` is where an app records the files.
3. A direct upload is checked twice: `Presign` refuses a declared `text/html`, and `Confirm` reads the stored bytes, so a client that declared a PNG and sent HTML is caught and its object deleted.
4. The Uppy endpoints refuse a disk without the `Multipart` and `PresignPart` capabilities with 501 `unsupported`.
5. `FromURL` refuses a loopback server like any non-public address, and a `file://` URL; `AllowAddr` lets a known internal server through on purpose. The type comes from the bytes, not the server's `Content-Type`, and the name from its `Content-Disposition`.
6. A `Process` hook rewrites the file before it is stored (trimmed to 5 bytes); an `Inspect` hook reads the stored file and refuses it, which deletes the object.
7. `Sweep` deletes objects under a directory older than a cutoff and refuses to sweep the root.
8. Key layouts: `ShardedKey` puts two directory levels in front of the random name; `NameKey` keeps the client's name, and a second `photo.png` is renamed or refused as `Collision` says.
9. A base64 body and a data URI are stored like any upload; the data URI's declared type is ignored, and text that is not base64 is `invalid_argument`.
10. `MinSize` refuses an empty file, `MaxTotal` caps a request's files together, and an 8 MiB body is refused after one buffer of it was read.
11. A resolver sets the limit by plan (free 413, paid 201) and refuses a rate-limited caller with 503 and `Retry-After`, before reading the body.
12. `ConfirmExpected` holds a direct upload to the size and SHA-256 the client declared: one changed byte is caught and the object deleted. A ticket may live up to `MaxPresignExpiry`, not longer.
13. `Promote` moves a cached upload to its permanent key and refuses a key outside the cache area.
14. `FindAbandoned` lists an unfinished multipart upload without removing it; `SweepAbandoned` removes it.
15. `Policy.KeyContext` builds the key from the request's context, where the app's middleware put the tenant: one policy stores `acme`'s upload under `tenants/acme/` and `globex`'s under `tenants/globex/`, and a request without a tenant is refused with 403 before anything is stored.
16. `FetchOptions.Proxy` sends an import through an egress proxy (a minimal CONNECT proxy here) and `FetchOptions.Header` adds the importer's `User-Agent`. The fetch resolves `localhost` itself and asks the proxy for the address, never the name; the metadata address is refused before the proxy is asked at all.
17. `envelope.go` answers the same requests through the upload form handler, the records endpoints (`recordshttp`) and a shared folder's listing (`share.Handler`), first with each package's default JSON, then with one `{"data": …}` envelope: `HandlerOptions.Respond`, `recordshttp.Config.Respond` and `share.WithRespond`, each given the generic `dataEnvelope` instantiated with that package's `SuccessBody`. The printout shows each body's keys, since the keys inside are random: the defaults are unchanged, and the envelope wraps all three.
18. `public.go` presigns an avatar upload on an S3 disk in ACL mode (the `PresignVisibility` capability; presigning is local, so no bucket is contacted). The client asks for `Visibility: public` and the policy has `AllowPublic`, so the ticket signs `x-amz-acl` and its `Headers` carry `X-Amz-Acl: public-read` for the client to send. The same request under a policy without `AllowPublic` is refused with `permission` before anything is signed; a ticket asking for nothing signs no ACL; and a bucket with ACLs off cannot pin a visibility, so it answers `unsupported`. The test `TestAllowPublicIsWhatAllows` shows the policy switch alone decides it.

## The code

The policy every step uses: `Allowed` is required, and active types stay refused even when a pattern matches them.

<!-- ds:block id=policy-e9b5m967 -->
**policy** · [`examples/concepts-uploads/main.go:43-47`](main.go#L43-L47)

```go
var policy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"image/*", "application/pdf"},
	Key:     upload.RandomKey(uploadDir),
}
```
<!-- /ds:block hash=c35128 -->

`Receive`, the core of every server-side path.

<!-- ds:block id=receive-eszpqjvz -->
**receive** · [`examples/concepts-uploads/main.go:91-108`](main.go#L91-L108)

```go
func receive(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), `..\..\Holiday Photo.PNG`, policy)
	if err != nil {
		return err
	}
	shape := path.Dir(res.Key) + "/<random>" + path.Ext(res.Key)
	fmt.Fprintf(out, "receive: key %s name %q type %s size %d\n", shape, res.Filename, res.ContentType, res.Size)
	_, err = upload.Receive(ctx, disk, strings.NewReader(htmlFile), "photo.png", policy)
	fmt.Fprintf(out, "receive html named photo.png: %s\n", filesystem.CodeOf(err))
	big := strings.NewReader(pngFile + strings.Repeat("x", maxUpload))
	_, err = upload.Receive(ctx, disk, big, "big.png", policy)
	fmt.Fprintf(out, "receive over MaxSize: %s, objects stored: %d\n", filesystem.CodeOf(err), count(ctx, disk))
	return nil
}
```
<!-- /ds:block hash=a2ec4e -->

The multipart form handler, all or nothing and `Partial`, and the JSON a partial request answers.

<!-- ds:block id=form-bav4dakx -->
**form** · [`examples/concepts-uploads/form.go:67-93`](form.go#L67-L93)

```go
func form(ctx context.Context, out io.Writer) error {
	files := []formFile{{"a.png", pngFile}, {"page.html", htmlFile}, {"b.pdf", pdfFile}}
	for _, partial := range []bool{false, true} {
		disk, err := newDisk()
		if err != nil {
			return err
		}
		recorded := 0
		h := upload.Handler(disk, policy, upload.HandlerOptions{
			MaxFiles: maxFilesPerRequest,
			Partial:  partial,
			OnStored: func(_ context.Context, _ *http.Request, stored []upload.Result) error {
				recorded = len(stored) // a real app inserts rows here
				return nil
			},
		})
		status, body, err := postForm(ctx, h, files...)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "form partial=%t: %d, stored %d, recorded %d\n", partial, status, count(ctx, disk), recorded)
		if !partial {
			fmt.Fprintf(out, "  body %s\n", body)
		}
	}
	return nil
}
```
<!-- /ds:block hash=925491 -->

<!-- ds:block id=formfailures-8rzt54v2 -->
**formFailures** · [`examples/concepts-uploads/form.go:98-110`](form.go#L98-L110)

```go
func formFailures(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{MaxFiles: maxFilesPerRequest, Partial: true})
	_, body, err := postForm(ctx, h, formFile{"page.html", htmlFile}, formFile{"b.pdf", pdfFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  partial body: %s\n", body)
	return nil
}
```
<!-- /ds:block hash=7e3ba7 -->

Direct uploads: `Presign`, the client's PUT, then `Confirm`.

<!-- ds:block id=direct-gtjfeh86 -->
**direct** · [`examples/concepts-uploads/direct.go:83-106`](direct.go#L83-L106)

```go
func direct(ctx context.Context, out io.Writer) error {
	b, err := newBucket()
	if err != nil {
		return err
	}
	defer b.srv.Close()
	for _, body := range []string{pngFile, htmlFile} {
		req := upload.TicketRequest{Filename: "photo.png", ContentType: "image/png", Size: int64(len(body))}
		t, err := upload.Presign(ctx, b.disk, req, policy, ticketLifetime)
		if err != nil {
			return err
		}
		status, err := put(ctx, t, body)
		if err != nil {
			return err
		}
		res, err := upload.Confirm(ctx, b.disk, t.Key, req.Filename, policy)
		left, _ := b.disk.Exists(ctx, t.Key)
		fmt.Fprintf(out, "direct: PUT %d, confirm %q type %q, object kept: %t\n", status, filesystem.CodeOf(err), res.ContentType, left)
	}
	_, err = upload.Presign(ctx, b.disk, upload.TicketRequest{Filename: "page.html", ContentType: "text/html", Size: 1}, policy, ticketLifetime)
	fmt.Fprintf(out, "presign declared text/html: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=a7b318 -->

The Uppy endpoints on a disk that cannot sign parts.

<!-- ds:block id=uppy-gknjx429 -->
**uppy** · [`examples/concepts-uploads/direct.go:112-125`](direct.go#L112-L125)

```go
func uppy(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Uppy(disk, policy, upload.UppyOptions{
		Authorize: func(*http.Request, string) error { return nil }, // check the key belongs to the user
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/multipart", bytes.NewReader([]byte(`{"filename":"a.png","type":"image/png"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Fprintf(out, "uppy on memory: %d %s", rec.Code, rec.Body.String())
	return nil
}
```
<!-- /ds:block hash=84dd36 -->

Imports from a URL, with the SSRF guard and its one escape hatch.

<!-- ds:block id=fromurl-bxrwf6up -->
**fromURL** · [`examples/concepts-uploads/remote.go:23-46`](remote.go#L23-L46)

```go
func fromURL(ctx context.Context, out io.Writer) error {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html") // ignored: the bytes decide
		w.Header().Set("Content-Disposition", `attachment; filename="scan.pdf"`)
		_, _ = io.WriteString(w, pdfFile)
	}))
	defer remote.Close()
	disk, err := newDisk()
	if err != nil {
		return err
	}
	_, err = upload.FromURL(ctx, disk, remote.URL+"/files/7", policy, upload.FetchOptions{})
	fmt.Fprintf(out, "from loopback: %v\n", err)
	loopback := func(a netip.Addr) bool { return a.IsLoopback() }
	res, err := upload.FromURL(ctx, disk, remote.URL+"/files/7", policy, upload.FetchOptions{AllowAddr: loopback})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "from allowed server: %q %s %d bytes\n", res.Filename, res.ContentType, res.Size)
	fmt.Fprintf(out, "PublicAddr(%s): %t\n", metadataService, upload.PublicAddr(metadataService))
	_, err = upload.FromURL(ctx, disk, "file:///etc/passwd", policy, upload.FetchOptions{})
	fmt.Fprintf(out, "file URL: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=b2c752 -->

A `Processor`, an `Inspector`, and both wired into a policy.

<!-- ds:block id=trimtext-5f5xcut2 -->
**trimText** · [`examples/concepts-uploads/hooks.go:27-36`](hooks.go#L27-L36)

```go
func trimText(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
	if !strings.HasPrefix(in.ContentType, "text/plain") {
		return body, in, nil
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, upload.Info{}, err
	}
	return bytes.NewReader(bytes.TrimSpace(b)), in, nil
}
```
<!-- /ds:block hash=95c293 -->

<!-- ds:block id=refusemarked-3nej68ju -->
**refuseMarked** · [`examples/concepts-uploads/hooks.go:41-50`](hooks.go#L41-L50)

```go
func refuseMarked(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
	b, err := disk.ReadAll(ctx, r.Key)
	if err != nil {
		return err
	}
	if bytes.Contains(b, []byte(blockedMarker)) {
		return filesystem.Errorf(filesystem.CodeRejected, "inspection refused %s", r.Filename)
	}
	return nil
}
```
<!-- /ds:block hash=0d9074 -->

<!-- ds:block id=processandinspect-2ryw5smr -->
**processAndInspect** · [`examples/concepts-uploads/hooks.go:55-69`](hooks.go#L55-L69)

```go
func processAndInspect(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	p := upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Process: trimText, Inspect: upload.Inspectors(refuseMarked)}
	res, err := upload.Receive(ctx, disk, strings.NewReader("  notes  \n"), "notes.txt", p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "processed: %d bytes\n", res.Size)
	_, err = upload.Receive(ctx, disk, strings.NewReader("this is blocked"), "bad.txt", p)
	fmt.Fprintf(out, "inspected: %s, objects stored: %d\n", filesystem.CodeOf(err), count(ctx, disk))
	return nil
}
```
<!-- /ds:block hash=27418c -->

`Sweep` for uploads nobody finished.

<!-- ds:block id=sweep-52zp4ugj -->
**sweep** · [`examples/concepts-uploads/hooks.go:75-97`](hooks.go#L75-L97)

```go
func sweep(ctx context.Context, out io.Writer) error {
	now := time.Now()
	clock := now.Add(-2 * abandonedAfter)
	disk, err := filesystem.NewDisk("uploads", memory.New(memory.WithClock(func() time.Time { return clock })))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "incoming/old.png", []byte(pngFile), filesystem.WriteOptions{}); err != nil {
		return err
	}
	clock = now
	if _, err := disk.WriteBytes(ctx, "incoming/new.png", []byte(pngFile), filesystem.WriteOptions{}); err != nil {
		return err
	}
	n, err := upload.Sweep(ctx, disk, "incoming", now.Add(-abandonedAfter))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept %d, left %d\n", n, count(ctx, disk))
	_, err = upload.Sweep(ctx, disk, "", now)
	fmt.Fprintf(out, "sweep the root: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=3e3c8e -->

Key layouts and name collisions.

<!-- ds:block id=layouts-fecj8ewm -->
**layouts** · [`examples/concepts-uploads/more.go:57-85`](more.go#L57-L85)

```go
func layouts(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	sharded := policy
	sharded.Key = upload.ShardedKey("photos", shardDepth)
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "a.png", sharded)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sharded: %d directory levels under photos/\n", strings.Count(path.Dir(res.Key), "/"))
	for _, c := range []upload.Collision{upload.CollisionRename, upload.CollisionFail} {
		named := policy
		named.Key = upload.NameKey("drive/" + string(c))
		named.Collision = c
		var got []string
		for range 2 {
			res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "photo.png", named)
			if err != nil {
				got = append(got, string(filesystem.CodeOf(err)))
				continue
			}
			got = append(got, res.Key)
		}
		fmt.Fprintf(out, "collision %s: %s\n", c, strings.Join(got, ", "))
	}
	return nil
}
```
<!-- /ds:block hash=8ca20f -->

Base64 bodies and data URIs.

<!-- ds:block id=encoded-kb86vpvx -->
**encoded** · [`examples/concepts-uploads/more.go:92-111`](more.go#L92-L111)

```go
func encoded(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(pngFile))
	res, err := upload.FromBase64(ctx, disk, b64, "avatar.png", policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "base64: %s, %d bytes\n", res.ContentType, res.Size)
	res, err = upload.FromDataURI(ctx, disk, "data:text/plain;base64,"+b64, "paste.png", policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "data URI declared text/plain: stored as %s\n", res.ContentType)
	_, err = upload.FromBase64(ctx, disk, "not base64!", "x.png", policy)
	fmt.Fprintf(out, "bad base64: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=ccf2c3 -->

The size checks besides `MaxSize`.

<!-- ds:block id=sizelimits-hkgs2nwg -->
**sizeLimits** · [`examples/concepts-uploads/more.go:118-146`](more.go#L118-L146)

```go
func sizeLimits(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	nonEmpty := policy
	nonEmpty.MinSize = 1
	_, err = upload.Receive(ctx, disk, strings.NewReader(""), "empty.png", nonEmpty)
	fmt.Fprintf(out, "empty file: %s\n", filesystem.CodeOf(err))
	two := upload.Handler(disk, policy, upload.HandlerOptions{MaxFiles: 2, MaxTotal: requestTotal})
	status, _, err := postForm(ctx, two, formFile{"a.png", pngFile}, formFile{"b.png", pngFile + pngFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "two files over MaxTotal %d: %d\n", requestTotal, status)
	small := policy
	small.MaxSize = freeLimit
	one := upload.Handler(disk, small, upload.HandlerOptions{})
	req := formRequest(ctx, "", formFile{"big.png", pngFile + strings.Repeat("x", hugeBody)})
	body := &countingReader{r: req.Body}
	req.Body = io.NopCloser(body)
	rec := httptest.NewRecorder()
	one.ServeHTTP(rec, req)
	fmt.Fprintf(out, "body of %d bytes: %d after reading under %d\n", hugeBody, rec.Code, readBound)
	if body.n >= readBound {
		return fmt.Errorf("the handler read %d bytes of an oversized body", body.n)
	}
	return nil
}
```
<!-- /ds:block hash=64c05e -->

Limits per plan, and a rate limit before the body is read.

<!-- ds:block id=perrequest-jrzefwkt -->
**perRequest** · [`examples/concepts-uploads/more.go:166-191`](more.go#L166-L191)

```go
func perRequest(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	limited := map[string]bool{planSpammer: true}
	resolve := func(r *http.Request, p upload.Policy) (upload.Policy, error) {
		if limited[r.Header.Get(planHeader)] {
			slow := filesystem.NewError(filesystem.CodeUnavailable, errors.New("too many uploads"))
			return p, slow.WithRetryAfter(retryAfter)
		}
		p.MaxSize = freeLimit
		if r.Header.Get(planHeader) == planPaid {
			p.MaxSize = paidLimit
		}
		return p, nil
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{Resolve: resolve})
	body := pngFile + strings.Repeat("x", freeLimit)
	for _, plan := range []string{planFree, planPaid, planSpammer} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, formRequest(ctx, plan, formFile{"photo.png", body}))
		fmt.Fprintf(out, "plan %s: %d retry-after=%q\n", plan, rec.Code, rec.Header().Get("Retry-After"))
	}
	return nil
}
```
<!-- /ds:block hash=d23ec6 -->

A direct upload held to its declared size and SHA-256.

<!-- ds:block id=declared-n3nu9wxw -->
**declared** · [`examples/concepts-uploads/more.go:198-224`](more.go#L198-L224)

```go
func declared(ctx context.Context, out io.Writer) error {
	b, err := newBucket()
	if err != nil {
		return err
	}
	defer b.srv.Close()
	sum := sha256.Sum256([]byte(pngFile))
	req := upload.TicketRequest{Filename: "photo.png", ContentType: "image/png", Size: int64(len(pngFile)), SHA256: hex.EncodeToString(sum[:])}
	want := upload.Expected{Size: req.Size, SHA256: req.SHA256}
	for _, sent := range []string{pngFile, pngFile[:len(pngFile)-1] + "!"} {
		t, err := upload.Presign(ctx, b.disk, req, policy, ticketLifetime)
		if err != nil {
			return err
		}
		if _, err := put(ctx, t, sent); err != nil {
			return err
		}
		_, err = upload.ConfirmExpected(ctx, b.disk, t.Key, req.Filename, policy, want)
		kept, _ := b.disk.Exists(ctx, t.Key)
		fmt.Fprintf(out, "declared sha256, sent %d bytes: confirm %q, kept %t\n", len(sent), filesystem.CodeOf(err), kept)
	}
	_, err = upload.Presign(ctx, b.disk, req, policy, slowMobileLink)
	fmt.Fprintf(out, "ticket for %s: %v\n", slowMobileLink, err)
	_, err = upload.Presign(ctx, b.disk, req, policy, filesystem.MaxPresignExpiry+time.Hour)
	fmt.Fprintf(out, "ticket past MaxPresignExpiry: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=4aeaf2 -->

Cache, then promote.

<!-- ds:block id=cachethenpromote-ywrmuhf4 -->
**cacheThenPromote** · [`examples/concepts-uploads/more.go:230-250`](more.go#L230-L250)

```go
func cacheThenPromote(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	cached := policy
	cached.Key = upload.RandomKey(cacheDir)
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "avatar.png", cached)
	if err != nil {
		return err
	}
	e, err := upload.Promote(ctx, disk, cacheDir, res.Key, disk, "avatars/u1.png")
	if err != nil {
		return err
	}
	stillCached, _ := disk.Exists(ctx, res.Key)
	fmt.Fprintf(out, "promoted to %s, still in cache: %t\n", e.Key, stillCached)
	_, err = upload.Promote(ctx, disk, cacheDir, "avatars/u1.png", disk, "avatars/u2.png")
	fmt.Fprintf(out, "promote a key outside the cache: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=30674c -->

The dry run of the abandoned-upload sweep.

<!-- ds:block id=abandoned-hkvv4ks7 -->
**abandoned** · [`examples/concepts-uploads/more.go:255-277`](more.go#L255-L277)

```go
func abandoned(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	if _, err := disk.CreateMultipart(ctx, "videos/big.mp4", filesystem.WriteOptions{}); err != nil {
		return err
	}
	cutoff := time.Now().Add(time.Second) // in production: a day or more ago
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "would remove: %s upload of %s\n", a.Kind, a.Key)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept %d abandoned upload(s)\n", n)
	return nil
}
```
<!-- /ds:block hash=f34cc8 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The sample files, the policy, the program and `Receive` |
| `form.go` | The multipart form handler, all or nothing and `Partial` |
| `direct.go` | Direct uploads with `Presign` and `Confirm`, and the Uppy endpoints |
| `remote.go` | Imports with `FromURL` |
| `hooks.go` | The `Process` and `Inspect` hooks, and `Sweep` |
| `more.go` | Key layouts, base64, size limits, per-request limits, declared checksums, cache then promote, abandoned uploads |
| `tenants.go` | A key built from the request's tenant with `Policy.KeyContext`; code: [`keyFromRequest`](ds:block?id=keyfromrequest-t3x8c5vb) |
| `proxied.go` | An import through a CONNECT egress proxy, with the importer's own headers |
| `envelope.go` | One success envelope on the upload, records and share handlers; code: [`dataEnvelope`](ds:block?id=dataenvelope-q7w3n8kd), [`envelopeHandlers`](ds:block?id=envelopehandlers-h2m8v4xc), [`envelope`](ds:block?id=envelope-k5t9w2rb) |
| `public.go` | Public direct uploads with `Policy.AllowPublic` on an ACL bucket; code: [`avatars`](ds:block?id=avatarpolicy-v3k8m2qx), [`publicTicket`](ds:block?id=publicticket-n6w9t4hb) |
| `main_test.go` | Runs the program and checks the lines the guide quotes, and pins that `AllowPublic` alone allows a public ticket |

## Next

- The guide: [Uploads](../../docs/concepts/uploads.md), including resumable uploads with tus and the pitfalls.
- Related examples: [concepts-security](../concepts-security), [recipe-browser-direct-uploads](../recipe-browser-direct-uploads), [recipe-mobile-uploads](../recipe-mobile-uploads), [recipe-resumable-uploads](../recipe-resumable-uploads), [recipe-import-from-url](../recipe-import-from-url), [recipe-virus-scanning](../recipe-virus-scanning).
