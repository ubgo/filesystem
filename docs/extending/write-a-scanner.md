# Write a scanner

You will have a malware or content scanner of your own plugged into every upload path: a client for an HTTP scanning service (the shape of most cloud scanning APIs), wired with `scan.Inspect` so a file is scanned after it is stored and before the upload reports success, and deleted when the scan refuses it. You will also see how to make the opposite choice from the library's default and fail open when the scanner is down.

Read this page when ClamAV (`scan.Clamd`, built in) is not your scanner: a cloud API, a commercial engine with its own protocol, an image-moderation service, or a check of your own such as refusing encrypted archives.

## Before you start

- [Virus scanning](../recipes/virus-scanning.md) sets up ClamAV with `scan.Clamd`; this page replaces the scanner and keeps the rest.
- The program is [`examples/extending-scanner`](../../examples/extending-scanner/main.go). `go run ./extending-scanner` in `examples/` uploads a clean file and the EICAR test string through a simulated scanning service; `go test ./extending-scanner` checks the verdicts and that a refused file leaves nothing stored.

## The contract

`scan.Scanner` has one method, `Scan(ctx context.Context, r io.Reader) (scan.Verdict, error)`:

| Outcome | Meaning | What `scan.Inspect` makes of it |
|---|---|---|
| `Verdict{Clean: true}`, nil | the scan ran and found nothing | the upload succeeds |
| `Verdict{Clean: false, Signature: "…"}`, nil | the scan ran and found something | `rejected` (`scan: the file is infected (…)`); the upload deletes the object; HTTP 415 |
| any verdict, an error | the scan did not complete; it says nothing about the file | `unavailable` (or the error's own code if it is a `*filesystem.Error`); the upload deletes the object; HTTP 503 |

Fail closed is the default on purpose: a scanner that is down refuses uploads instead of letting them through unscanned. `scan.ScannerFunc` adapts a plain function to the interface.

## Step 1 — Write the client

The scanner streams the file in the request body, so a large upload is never held in memory, and caps the size of the reply it reads. Any answer other than a verdict is an error:

<!-- ds:block id=httpscanner-3zvefzbc -->
**HTTPScanner** · [`examples/extending-scanner/main.go:49-52`](../../examples/extending-scanner/main.go#L49-L52)

```go
type HTTPScanner struct {
	URL    string
	Client *http.Client
}
```
<!-- /ds:block hash=287c7a -->

<!-- ds:block id=httpscanner-scan-mxa78qxc -->
**HTTPScanner.Scan** · [`examples/extending-scanner/main.go:63-82`](../../examples/extending-scanner/main.go#L63-L82)

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

## Step 2 — Put it in the policy

`scan.Inspect(s)` returns an `upload.Inspector`: it reads the stored object back from the disk and scans it. As `Policy.Inspect` it runs on every upload path in the module: form uploads (`upload.Handler`, `upload.Receive`), imports (`upload.FromURL`), tus (`contrib/tus`), presigned uploads at `upload.Confirm`, Uppy at complete, and `records.Store.Upload` and `recordshttp`, which go through `upload.Receive`:

<!-- ds:block id=policywith-gx22ue2z -->
**policyWith** · [`examples/extending-scanner/main.go:105-111`](../../examples/extending-scanner/main.go#L105-L111)

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

Several inspections chain with `upload.Inspectors(a, b)`, which runs them in order, skips nil entries and stops at the first error: a scan, then a full image decode, then a moderation call.

Check: `go run ./extending-scanner` stores the clean file and refuses the EICAR string:

```
upload: key="kkqykvikcoiq46p4rsu5ulhx7d.txt" err=<nil>
upload: key="" err=filesystem: rejected: scan: the file is infected (Eicar-Test-Signature)
scanner down, fail closed: filesystem: unavailable: scanner answered 404 Not Found
alert: unscanned upload: scanner answered 404 Not Found
scanner down, fail open: stored=true err=<nil>
```

`TestInfectedFileIsNotKept` uploads EICAR to a `filesystemtest.Fake` disk and asserts the disk is empty afterwards: the object was stored, scanned, refused and deleted before `Receive` returned.

## Step 3 — Decide what happens when the scanner is down

The third line is the default: the scanning service answered 404, the scan did not complete, and the upload was refused with `unavailable`. An application that prefers to stay up makes that choice explicitly, in a wrapper, and alerts on every file it lets through:

<!-- ds:block id=failopen-6aq3sfyu -->
**FailOpen** · [`examples/extending-scanner/main.go:89-98`](../../examples/extending-scanner/main.go#L89-L98)

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

The last two lines show it: the alert fires, and the file is stored.

## Moderation: hash lists

Content moderation often starts with a list of fingerprints of known material: hash lists shared by safety organisations and industry programmes, a list of files your own moderators removed, or a perceptual-hash service that matches resized and re-encoded copies. `scan/hashmatch.go` gives that the scanner shape, so it plugs into `scan.Inspect` and fails closed like a virus scan:

| Piece | Role |
|---|---|
| `scan.Digest` | One fingerprint: `Algorithm` (`scan.AlgorithmMD5`, `scan.AlgorithmSHA256`, or a perceptual algorithm's own name) and `Value`. |
| `scan.Fingerprinter` | Computes a digest from the stream. `scan.MD5` and `scan.SHA256` are built in; a perceptual hash (PDQ, PhotoDNA) needs an image decoder and comes from the application or a module of its own. |
| `scan.HashMatcher` | Looks a digest up and answers with a `scan.Verdict`: `Clean: false` with `Signature` naming the list on a match, an error when the lookup did not complete. `scan.HashMatcherFunc` adapts a call to a moderation API; a perceptual matcher compares by distance here. |
| `scan.NewHashSet(signature, digests...)` | An in-memory, immutable `HashMatcher` over a list loaded at start-up, case-insensitive on hex. The matched digest is not echoed in the signature, which reaches the uploader. |
| `scan.HashScanner(f, m)` | The `Scanner` that fingerprints with `f` and asks `m`. |

Step by step, as [`examples/extending-scanner/moderation.go`](../../examples/extending-scanner/moderation.go) does it:

1. Load the list at start-up. Lists are usually published as a file of hex digests, one per line; refuse a line that is not a digest, so a list in the wrong format fails loudly instead of matching nothing:

<!-- ds:block id=loadhashlist-jz8z8r2p -->
**loadHashList** · [`examples/extending-scanner/moderation.go:26-40`](../../examples/extending-scanner/moderation.go#L26-L40)

```go
func loadHashList(r io.Reader) ([]scan.Digest, error) {
	var list []scan.Digest
	lines := bufio.NewScanner(r)
	for n := 1; lines.Scan(); n++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if b, err := hex.DecodeString(line); err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("hash list line %d: not a SHA-256: %q", n, line)
		}
		list = append(list, scan.Digest{Algorithm: scan.AlgorithmSHA256, Value: line})
	}
	return list, lines.Err()
}
```
<!-- /ds:block hash=09b80d -->

2. Build an immutable `scan.HashSet` from it, named for the list: `scan.NewHashSet("removed-by-moderation", list...)`. To pick up a new list, build a new set and swap it in; never mutate one in use.

3. Run it beside the malware scan with `upload.Inspectors`. The malware scanner runs first; the hash lookup runs only if it passed, and either refusal deletes the file:

<!-- ds:block id=moderatedpolicy-734sh2n5 -->
**moderatedPolicy** · [`examples/extending-scanner/moderation.go:46-53`](../../examples/extending-scanner/moderation.go#L46-L53)

```go
func moderatedPolicy(malware scan.Scanner, removed *scan.HashSet) upload.Policy {
	p := policyWith(malware)
	p.Inspect = upload.Inspectors(
		scan.Inspect(malware),
		scan.Inspect(scan.HashScanner(scan.SHA256, removed)),
	)
	return p
}
```
<!-- /ds:block hash=526baa -->

Check: `go run ./extending-scanner` ends with:

```
hash list: 1 digest(s)
moderated upload: stored=false err=filesystem: rejected: scan: the file is infected (removed-by-moderation)
moderated upload: stored=true err=<nil>
```

The file the moderators removed is refused when uploaded again, under the list's name, and a new file is stored. The message says "infected" because `scan.Inspect` words every unclean verdict the same way; branch on `filesystem.CodeRejected` and show users your own text. An exact hash only matches a byte-identical copy: a re-encoded or resized image has a different SHA-256, which is why moderation services use perceptual hashes behind a `scan.HashMatcherFunc`.

Providers that report matches to an authority, or that must not see the file at all, stay in the application: the hook gives them the digest or the stream and acts on the verdict. Archives are a separate check: [`contrib/archive`](../plugins/archive.md) refuses bombs, zip-slip entries and programs inside zip and tar uploads.

## Pitfalls

- **Scanning happens after storage.** The object exists on the disk for the duration of the scan. A reader who guesses its random key in that window could fetch it; serve uploads only after the upload call returns, and never from the key alone.
- **Deletion after a refusal can fail.** If the delete fails too, the object stays, unrecorded; schedule `upload.Sweep` on the upload directory to remove what nothing refers to.
- **Size limits on the scanner.** clamd refuses streams over its `StreamMaxLength` (100 MiB by default in current ClamAV, 25 MB in old releases), and cloud APIs have their own caps; a policy that allows larger files fails every large upload with `too_large` or `unavailable`. Keep `Policy.MaxSize` under the scanner's limit or raise the scanner's.
- **Timeouts.** The scan runs inside the upload request. Give the HTTP client a timeout (the example uses the test server's client; production sets `http.Client.Timeout`), or a hung scanner holds every upload open.
- **Presigned and Uppy uploads are scanned at `Confirm` and complete.** The bytes reach the bucket before your server sees them; until confirmation they are unscanned. Keep them under an upload prefix nothing serves, and sweep what is never confirmed.

## Next

- [Virus scanning](../recipes/virus-scanning.md): ClamAV in Docker and `scan.Clamd`.
- [Uploads](../concepts/uploads.md): `Policy.Process` (before storage) and `Policy.Inspect` (after).
- [`scan` reference](../../scan/README.md): `Scanner`, `Verdict`, `Clamd`, `Inspect`.
