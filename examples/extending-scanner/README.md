# Extending: a malware scanner of your own

A malware scanner written from scratch: a client for an HTTP scanning service (the shape of most cloud scanning APIs), plugged into an upload policy with `scan.Inspect` so every upload path scans the file before it reports success. It also wraps a scanner to fail open, for an application that would rather stay up while the scanner is down, and adds a moderation hash list beside the malware scan. Use this shape when your scanning is a hosted API rather than ClamAV. It uses `github.com/ubgo/filesystem`, `scan`, `upload` and the `memory` driver. The guide is [Write a scanner](../../docs/extending/write-a-scanner.md).

## Run it

```sh
cd examples
go run ./extending-scanner
go test ./extending-scanner
```

The scanning service is simulated in-process with `httptest` on a random loopback port, closed when the program exits; files go to the memory driver, so nothing is written to disk. The simulated service flags the EICAR test string, which every antivirus product treats as a virus by convention, and answers 404 on any path but `/scan`, which stands in for a service that is down. The tests also check that an infected upload leaves no object behind.

## What it does

1. Uploads a clean text file: scanned, stored, and given a random key.
2. Uploads the EICAR string: the scanner reports it and the upload is refused with `CodeRejected`; the stored object is deleted.
3. Points the scanner at a path the service does not serve. The scan cannot complete, so the upload fails closed with `CodeUnavailable`.
4. Wraps the same broken scanner in `FailOpen`: the file is stored and the alert callback fires, because every call is an unscanned file.
5. Loads a moderation hash list (`moderation.go`) holding the SHA-256 of one removed file, and runs it after the malware scan with `upload.Inspectors`: the removed file is refused when uploaded again, a new file is stored.

```text
upload: key="kb5iadueirk7b2avrym3mjuf47.txt" err=<nil>
upload: key="" err=filesystem: rejected: scan: the file is infected (Eicar-Test-Signature)
scanner down, fail closed: filesystem: unavailable: scanner answered 404 Not Found
alert: unscanned upload: scanner answered 404 Not Found
scanner down, fail open: stored=true err=<nil>
hash list: 1 digest(s)
moderated upload: stored=false err=filesystem: rejected: scan: the file is infected (removed-by-moderation)
moderated upload: stored=true err=<nil>
```

The key changes on every run.

## The code

`HTTPScanner` implements `scan.Scanner`. An error from it means the scan did not complete and says nothing about the file; `scan.Inspect` turns that into `CodeUnavailable`, so the upload is refused rather than let through.

<!-- ds:block id=httpscanner-3zvefzbc -->
**HTTPScanner** · [`examples/extending-scanner/main.go:49-52`](main.go#L49-L52)

```go
type HTTPScanner struct {
	URL    string
	Client *http.Client
}
```
<!-- /ds:block hash=287c7a -->

`Scan` streams the file in the request body, so a large upload is never held in memory, and caps the service's JSON reply so a misbehaving service cannot make it read without end.

<!-- ds:block id=httpscanner-scan-mxa78qxc -->
**HTTPScanner.Scan** · [`examples/extending-scanner/main.go:63-82`](main.go#L63-L82)

```go
func (s HTTPScanner) Scan(ctx context.Context, r io.Reader) (scan.Verdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, r)
	if err != nil {
		return scan.Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.Client.Do(req)
	if err != nil {
		return scan.Verdict{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return scan.Verdict{}, fmt.Errorf("scanner answered %s", resp.Status)
	}
	var v verdictReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVerdictBytes)).Decode(&v); err != nil {
		return scan.Verdict{}, err
	}
	return scan.Verdict{Clean: v.Clean, Signature: v.Signature}, nil
}
```
<!-- /ds:block hash=a12396 -->

`FailOpen` lets a file through when the scan cannot run, after telling `onError`. The library fails closed on purpose; failing open is the application's decision, and it should alert on every call.

<!-- ds:block id=failopen-6aq3sfyu -->
**FailOpen** · [`examples/extending-scanner/main.go:89-98`](main.go#L89-L98)

```go
func FailOpen(s scan.Scanner, onError func(error)) scan.Scanner {
	return scan.ScannerFunc(func(ctx context.Context, r io.Reader) (scan.Verdict, error) {
		v, err := s.Scan(ctx, r)
		if err != nil {
			onError(err)
			return scan.Verdict{Clean: true}, nil
		}
		return v, nil
	})
}
```
<!-- /ds:block hash=15c5ea -->

`policyWith` puts the scanner in the policy's `Inspect` step. It runs on every stored upload (form, import, tus, presign confirm, Uppy complete) before success is reported, and its error deletes the file.

<!-- ds:block id=policywith-gx22ue2z -->
**policyWith** · [`examples/extending-scanner/main.go:105-111`](main.go#L105-L111)

```go
func policyWith(s scan.Scanner) upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadSize,
		Allowed: []string{"text/plain", "application/pdf", "image/*"},
		Inspect: scan.Inspect(s),
	}
}
```
<!-- /ds:block hash=25a146 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | `HTTPScanner`, `FailOpen`, `policyWith`, the simulated scanning service and the demo `run` |
| `main_test.go` | The demo output, and that an infected upload leaves no object |

## Next

- [Write a scanner](../../docs/extending/write-a-scanner.md): the guide for this program.
- [Virus scanning](../recipe-virus-scanning): ClamAV and the built-in `scan.Clamd`.
- [Concepts: uploads](../concepts-uploads): `Policy.Process` (before storage) and `Policy.Inspect` (after).
