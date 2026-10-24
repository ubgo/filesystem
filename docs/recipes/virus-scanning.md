# Virus scanning

Any product where one user's upload reaches another user's machine (a shared drive, a chat, a support desk, a job board taking CVs, a portal where clients send documents to an accountant) will eventually store malware. This recipe scans every upload with ClamAV before the upload is reported as stored, and refuses uploads when the scanner cannot answer.

The program is [examples/recipe-virus-scanning](../../examples/recipe-virus-scanning). Every code block below is copied from it, and every transcript is from running it.

## What you get

- Every file is scanned after it is stored and before the client hears "201". An infected file is deleted and the request fails with `415` and code `rejected`, naming the signature.
- A request with several files stores all of them or none: one infected file deletes the clean ones too.
- A second check runs after the scan: an upload that claims to be an image must have a readable image header.
- When clamd is down, uploads fail with `503` (`unavailable`). Nothing is stored unscanned.
- `GET /readyz` answers `503` when clamd does not respond, so a load balancer stops sending uploads to that instance.

## The design

| Piece | Why |
|---|---|
| `Policy.Inspect` | Runs on every stored upload, on every path in the `upload` module: form uploads (`Handler`, `Receive`, `records.Store.Upload`), imports (`FromURL`), tus, and `Confirm` for presigned and Uppy uploads. An error deletes the object and becomes the upload's error. |
| `scan.Inspect(scan.Clamd{...})` | Streams the stored file to clamd with the `INSTREAM` command, so clamd needs no access to your disk or bucket. |
| `upload.Inspectors(...)` | Chains inspections in order and stops at the first failure: the scan, then the image header check. A moderation call or a full decode could follow. |
| Fail closed | A scanner error (connection refused, timeout) is `CodeUnavailable`, never a pass. To stay up when the scanner is down, wrap the `Scanner` and decide there, knowingly. |
| A stand-in scanner without `CLAMD_ADDR` | So the demo and its tests run without Docker. It only knows the EICAR test string, and says so at start-up. |

## Step 1: Choose the scanner

<!-- ds:block id=newscanner-u7ybdejf -->
**newScanner** · [`examples/recipe-virus-scanning/main.go:73-79`](../../examples/recipe-virus-scanning/main.go#L73-L79)

```go
func newScanner(lookup func(string) (string, bool)) scan.Scanner {
	if addr, ok := lookup(envClamdAddr); ok && addr != "" {
		return scan.Clamd{Network: "tcp", Address: addr, Timeout: scanTimeout}
	}
	fmt.Fprintf(os.Stderr, "WARN: %s is not set: using a stand-in scanner that detects only the EICAR test file\n", envClamdAddr)
	return scan.ScannerFunc(eicarOnly)
}
```
<!-- /ds:block hash=4a551f -->

`scan.Clamd` speaks clamd's protocol over TCP (`"tcp", "127.0.0.1:3310"`) or a Unix socket (`"unix", "/run/clamav/clamd.ctl"`). `Timeout` bounds one scan, connection included; 0 means `scan.DefaultTimeout`, two minutes.

The stand-in is a `scan.ScannerFunc`, the same adapter you would use for a cloud scanning API:

<!-- ds:block id=eicaronly-9nrbej47 -->
**eicarOnly** · [`examples/recipe-virus-scanning/main.go:84-93`](../../examples/recipe-virus-scanning/main.go#L84-L93)

```go
func eicarOnly(_ context.Context, r io.Reader) (scan.Verdict, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1))
	if err != nil {
		return scan.Verdict{}, err
	}
	if bytes.Contains(data, []byte(eicarSignature)) {
		return scan.Verdict{Clean: false, Signature: eicarName}, nil
	}
	return scan.Verdict{Clean: true}, nil
}
```
<!-- /ds:block hash=884a38 -->

The EICAR string is a harmless file every antivirus reports as infected, which is how you test a scanning pipeline end to end without handling real malware.

## Step 2: Chain the inspections

<!-- ds:block id=imageheader-rn6ydg6y -->
**imageHeader** · [`examples/recipe-virus-scanning/main.go:99-110`](../../examples/recipe-virus-scanning/main.go#L99-L110)

```go
func imageHeader(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
	if !strings.HasPrefix(r.ContentType, "image/") {
		return nil
	}
	rc, err := disk.Read(ctx, r.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = imaging.Probe(rc)
	return err
}
```
<!-- /ds:block hash=6bedc2 -->

<!-- ds:block id=newpolicy-52qncapy -->
**newPolicy** · [`examples/recipe-virus-scanning/main.go:116-123`](../../examples/recipe-virus-scanning/main.go#L116-L123)

```go
func newPolicy(s scan.Scanner) upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadBytes,
		Allowed: []string{"image/png", "image/jpeg", "application/pdf", "text/plain", "application/zip"},
		Key:     upload.RandomKey("uploads"),
		Inspect: upload.Inspectors(scan.Inspect(s), imageHeader),
	}
}
```
<!-- /ds:block hash=511f3d -->

Inspections run after the type check and the size cap, on the bytes actually stored, and read the object back from the disk (`Inspector` receives the disk and the `upload.Result`). The scan runs first so that an infected file is reported as infected, whatever else is wrong with it.

`Policy.MaxSize` must not exceed clamd's `StreamMaxLength` (100 MiB by default in current ClamAV, 25 MB in old releases). A file over that limit is refused by clamd, which `scan.Clamd` reports as `CodeTooLarge`: the upload fails rather than passing unscanned.

## Step 3: Wire the endpoint and the probe

<!-- ds:block id=ready-dxq3dmmu -->
**ready** · [`examples/recipe-virus-scanning/main.go:129-144`](../../examples/recipe-virus-scanning/main.go#L129-L144)

```go
func ready(s scan.Scanner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.(pinger)
		if !ok {
			_, _ = io.WriteString(w, "ok (stand-in scanner)\n")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := p.Ping(ctx); err != nil {
			http.Error(w, "scanner unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	}
}
```
<!-- /ds:block hash=bb9979 -->

<!-- ds:block id=new-d27eer4u -->
**New** · [`examples/recipe-virus-scanning/main.go:148-161`](../../examples/recipe-virus-scanning/main.go#L148-L161)

```go
func New(dataDir string, s scan.Scanner) (http.Handler, *filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		return nil, nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /uploads", upload.Handler(disk, newPolicy(s), upload.HandlerOptions{MaxFiles: maxFilesPerRequest}))
	mux.HandleFunc("GET /readyz", ready(s))
	return mux, disk, nil
}
```
<!-- /ds:block hash=89fde6 -->

## Try it

With the stand-in scanner:

```sh
cd examples
go run ./recipe-virus-scanning
```

```text
WARN: CLAMD_ADDR is not set: using a stand-in scanner that detects only the EICAR test file
listening on http://localhost:8111 data in /var/folders/sk/.../T/recipe-virus-scanning-1618145978
```

```sh
printf '%s' 'X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*' > eicar.txt
echo "meeting notes" > notes.txt
curl -s -F file=@notes.txt localhost:8111/uploads
```

```json
{"files":[{"key":"uploads/ooxcmsqln3cghdwvnux7ico74n.txt","filename":"notes.txt","content_type":"text/plain; charset=utf-8","size":14,"sha256":"2f961146136b3a277868c6769ff925bda87e49946e5e6b842ad359d6b27aada4"}]}
```

```sh
curl -s -w '%{http_code}\n' -F file=@eicar.txt localhost:8111/uploads
```

```text
{"error":"scan: the file is infected (Eicar-Test-Signature)","code":"rejected"}
415
```

A clean and an infected file in one request:

```sh
curl -s -F file=@notes.txt -F file=@eicar.txt localhost:8111/uploads
```

```json
{"error":"scan: the file is infected (Eicar-Test-Signature)","code":"rejected"}
```

A truncated PNG passes the scan and fails the header check:

```sh
printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRbroken' > broken.png
curl -s -F file=@broken.png localhost:8111/uploads
```

```json
{"error":"imaging: unreadable png header: unexpected EOF","code":"rejected"}
```

Check: the data directory holds exactly one object, the first `notes.txt`. The `notes.txt` sent next to `eicar.txt` was stored, then deleted when its sibling failed.

Now point it at an address where nothing listens, which is what a crashed clamd looks like:

```sh
CLAMD_ADDR=127.0.0.1:3399 go run ./recipe-virus-scanning
curl -s -w '%{http_code}\n' -F file=@notes.txt localhost:8111/uploads
curl -s -w '%{http_code}\n' localhost:8111/readyz
```

```text
{"error":"Service Unavailable","code":"unavailable"}
503
scanner unavailable
503
```

## With a real clamd

The repository's Taskfile starts the image ClamAV publishes for both amd64 and arm64:

```sh
docker run -d --name clamav -p 127.0.0.1:3310:3310 clamav/clamav-debian:stable
```

The first start downloads the signature database, which takes a few minutes; until then clamd refuses connections and uploads answer `503`. Then:

```sh
CLAMD_ADDR=127.0.0.1:3310 go run ./recipe-virus-scanning
```

From the repository root, `task test:clamav` starts a throwaway clamd from the repository's `compose.yml`, runs the `scan` package's tests against it once its signatures are loaded, and removes it afterwards.

## Direct, resumable and imported uploads

The scan is part of the policy, so every path that applies the policy scans:

- Presigned PUTs and Uppy multipart: the bytes go straight to the bucket, and `upload.Confirm` (which `upload.Uppy` calls on completion) runs `Inspect` on the stored object and deletes it when refused. Until `Confirm` runs, the object sits unscanned under its key: never hand out a link to an unconfirmed upload, and sweep unconfirmed ones with `upload.Sweep` ([browser direct uploads](browser-direct-uploads.md)). The example's test confirms an EICAR object written straight to the disk and sees it deleted.
- tus: the finished upload goes through `upload.Receive`, so it is scanned before the final `PATCH` succeeds ([resumable uploads](resumable-uploads.md)).
- `upload.FromURL`: scanned like a form upload ([import from URL](import-from-url.md)).
- `records.Store.Upload` and `Replace`: scanned, and a refused file leaves neither a row nor bytes.

## Scan later: quarantine

Scanning inside the request holds the client until clamd answers. When that is too slow (large files, a scanning service with a queue), store the file at once and scan it afterwards, keeping it unusable meanwhile: record it with `records.Quarantine()` (or `recordshttp.Config{Quarantine: true}`, or `share.WithQuarantinedDeposits()` for request links), and run `store.Scan(ctx, scope, id, scanner)` from a worker with the same `scan.Scanner`. Until the scan passes, no link, view, share, stream or public visibility is handed out for the file, and `GET /files/{id}` answers 409 while the client polls the status route. A failed scan keeps the file quarantined with the verdict, or purges it with `records.WithScanFailure(records.ScanFailurePurge)`. The full list of guarded paths is in [Processing status](../records/processing-status.md#quarantine-until-scanned).

## Edge cases this handles

- Scanner down, slow or restarting: `503`, nothing stored. Clients retry later.
- A file larger than clamd's `StreamMaxLength`: `413` (`CodeTooLarge`), nothing stored. Keep `Policy.MaxSize` at or below it.
- An infected file among several: the whole request fails and every file it stored is deleted (use `HandlerOptions{Partial: true}` to keep the clean ones and list the refused one in `failed`).
- A truncated or corrupt image: refused by the second inspection before any thumbnailer sees it.
- An infected file with an innocent name or type header: the type is detected from the bytes and the scan reads the bytes; names play no part.

## For production

- Run clamd as a sidecar or a small service next to the app, with `freshclam` updating signatures (the official image does both). Monitor signature age.
- Size clamd for your peak: each scan holds a clamd thread. clamd's `MaxThreads` and your instances' upload concurrency should agree, and `scan.Clamd.Timeout` should be shorter than your HTTP server's write timeout.
- Wire `/readyz` (or `scan.Clamd.Ping`) into the load balancer's readiness check, not the liveness check: a scanner outage should drain uploads, not restart the app.
- Re-scan stored files when signatures update if your threat model requires it; the library scans at upload time only. A job that walks `Disk.List` and calls `Scanner.Scan` on each object is enough.
- Remove the stand-in scanner from production builds, or make a missing `CLAMD_ADDR` a start-up error.

## Related

- [Uploads](../concepts/uploads.md): policies, processors, inspectors and every upload path.
- [Security](../concepts/security.md): what the library checks on every upload, and what remains the app's job.
- [Write a scanner](../extending/write-a-scanner.md): plugging a cloud scanning API into `scan.Scanner`.
- [Team drive](team-drive.md) and [chat attachments](chat-attachments.md): the products that need this most.
