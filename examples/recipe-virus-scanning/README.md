# Virus scanning: every upload scanned before it is accepted

An upload endpoint that scans every file for malware before the client hears "201", for any product where one user's upload reaches another user's machine (shared drives, chat, support desks, client portals). It uses `upload.Policy.Inspect` with `scan.Inspect` (ClamAV's clamd over `INSTREAM`), chains a second check with `upload.Inspectors` (an image must have a header `contrib/imaging` can read), and fails closed: when the scanner is down, uploads answer `503` and nothing is stored unscanned. Without `CLAMD_ADDR` it uses a stand-in scanner that only recognises the EICAR test file, so it runs without Docker. The guide is [Virus scanning](../../docs/recipes/virus-scanning.md).

## Run it

```sh
cd examples
go run ./recipe-virus-scanning
```

It stores files under a new temporary directory (printed at start-up) and listens on `http://localhost:8111`. Without `CLAMD_ADDR` it prints a warning that the stand-in scanner is in use. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory, so copy anything you want to inspect first.

To scan with a real ClamAV, start clamd (the first start downloads signatures, which takes a few minutes) and point the program at it:

```sh
docker run -d --name clamav -p 127.0.0.1:3310:3310 clamav/clamav-debian:stable
CLAMD_ADDR=127.0.0.1:3310 go run ./recipe-virus-scanning
```

Run the tests with `go test ./recipe-virus-scanning`. From the repository root, `task test:clamav` runs the `scan` package's tests against that container.

## Walkthrough

Create three test files: a clean text file, the EICAR test string (harmless, but reported by every scanner as infected) and a PNG cut off after its signature.

```sh
printf 'hello\n' > notes.txt
printf '%s' 'X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*' > eicar.txt
printf '\x89PNG\r\n\x1a\nbroken' > broken.png
```

1. The readiness probe answers, and says it is the stand-in:

```sh
curl -s localhost:8111/readyz
```

```text
ok (stand-in scanner)
```

2. A clean file is stored; the type comes from the bytes, the key is random:

```sh
curl -s -F file=@notes.txt localhost:8111/uploads
```

```json
{"files":[{"key":"uploads/g22lj57rhpuqoh2tuwfjq3gs25.txt","filename":"notes.txt","content_type":"text/plain; charset=utf-8","size":6,"sha256":"5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"}]}
```

3. The EICAR file is refused with `415` and the signature name, and deleted:

```sh
curl -s -w '%{http_code}\n' -F file=@eicar.txt localhost:8111/uploads
```

```text
{"error":"scan: the file is infected (Eicar-Test-Signature)","code":"rejected"}
415
```

4. The truncated PNG passes the scan but fails the second inspection:

```sh
curl -s -w '%{http_code}\n' -F file=@broken.png localhost:8111/uploads
```

```text
{"error":"imaging: unreadable png header: unexpected EOF","code":"rejected"}
415
```

5. One infected file among several fails the whole request, and the clean file stored with it is deleted too (afterwards the data directory still holds only the file from step 2):

```sh
curl -s -w '%{http_code}\n' -F file=@notes.txt -F file=@eicar.txt localhost:8111/uploads
```

```text
{"error":"scan: the file is infected (Eicar-Test-Signature)","code":"rejected"}
415
```

6. Fail closed: restarted with `CLAMD_ADDR` pointing at a port where nothing listens, the probe and every upload answer `503`:

```sh
CLAMD_ADDR=127.0.0.1:3399 go run ./recipe-virus-scanning
curl -s -w '%{http_code}\n' localhost:8111/readyz
curl -s -w '%{http_code}\n' -F file=@notes.txt localhost:8111/uploads
```

```text
scanner unavailable
503
{"error":"Service Unavailable","code":"unavailable"}
503
```

## The code

`newScanner` picks clamd when `CLAMD_ADDR` is set and the stand-in otherwise, warning loudly so nobody mistakes "nothing found" for a real scan.

<!-- ds:block id=newscanner-u7ybdejf -->
**newScanner** · [`examples/recipe-virus-scanning/main.go:73-79`](main.go#L73-L79)

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

`eicarOnly` is the stand-in, a `scan.ScannerFunc`: the same adapter a cloud scanning API would use. It is a test double, not protection.

<!-- ds:block id=eicaronly-9nrbej47 -->
**eicarOnly** · [`examples/recipe-virus-scanning/main.go:84-93`](main.go#L84-L93)

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

`imageHeader` is the second inspection: a file whose detected type is an image must have a readable header, which catches truncated or corrupt images before a thumbnailer sees them.

<!-- ds:block id=imageheader-rn6ydg6y -->
**imageHeader** · [`examples/recipe-virus-scanning/main.go:99-110`](main.go#L99-L110)

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

`newPolicy` chains the scan and the header check with `upload.Inspectors`, in order, stopping at the first failure. Any error deletes the stored object and becomes the upload's error.

<!-- ds:block id=newpolicy-52qncapy -->
**newPolicy** · [`examples/recipe-virus-scanning/main.go:116-123`](main.go#L116-L123)

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

`ready` answers `503` when clamd does not respond to a ping, for a load balancer's readiness check: an instance whose scanner is down should not receive uploads.

<!-- ds:block id=ready-dxq3dmmu -->
**ready** · [`examples/recipe-virus-scanning/main.go:129-144`](main.go#L129-L144)

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

`New` wires a local disk, the upload handler (at most five files per request) and the probe.

<!-- ds:block id=new-d27eer4u -->
**New** · [`examples/recipe-virus-scanning/main.go:148-161`](main.go#L148-L161)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The scanner choice, the stand-in, the inspections, the policy, the readiness probe and the server. |
| `main_test.go` | Clean and infected uploads, a mixed batch that stores nothing, fail-closed with a broken scanner, `upload.Confirm` scanning a direct upload, and the readiness probe. |

## Next

- The guide: [Virus scanning](../../docs/recipes/virus-scanning.md), including direct, resumable and imported uploads, and production notes (signature age, clamd sizing, `StreamMaxLength`).
- [Uploads](../../docs/concepts/uploads.md) and [Security](../../docs/concepts/security.md) for the policy and what the library checks on every upload.
- [Write a scanner](../../docs/extending/write-a-scanner.md) and the [extending-scanner](../extending-scanner) example for a cloud scanning API.
- Related recipes: [recipe-browser-direct-uploads](../recipe-browser-direct-uploads), [recipe-resumable-uploads](../recipe-resumable-uploads), [recipe-import-from-url](../recipe-import-from-url).
