# scan

`github.com/ubgo/filesystem/scan` checks uploaded files for malware. It defines the `Scanner` contract, a client for ClamAV's clamd daemon, and `Inspect`, which plugs any scanner into an `upload.Policy` so every upload path scans the stored file before reporting success.

Scanning fails closed: a file is accepted only on a clean verdict, so a scanner that is down refuses uploads (`CodeUnavailable`) instead of letting them through unscanned.

The concepts are in [Security](../docs/concepts/security.md#scanning-fails-closed) and a complete set-up in [Virus scanning](../docs/recipes/virus-scanning.md). Writing a scanner for a cloud API is in [Write a scanner](../docs/extending/write-a-scanner.md).

Plugging a scanner into a policy (the guide uses a stand-in scanner; production passes `scan.Clamd{Network: "tcp", Address: "127.0.0.1:3310"}`):

<!-- ds:block id=scanning-rsgwpjcn -->
**scanning** · [`examples/concepts-security/main.go:178-194`](../examples/concepts-security/main.go#L178-L194)

```go
func scanning(ctx context.Context, _ *filesystem.Disk, out io.Writer) {
	for _, c := range []struct {
		data string
		down bool
	}{{"PNG EICAR", false}, {pngBytes, true}, {pngBytes, false}} {
		disk, err := filesystem.NewDisk("scanned", memory.New())
		if err != nil {
			fmt.Fprintln(out, err)
			return
		}
		p := images
		p.Allowed = []string{"image/*", "text/plain"}
		p.Inspect = scan.Inspect(demoScanner(c.down))
		_, err = upload.Receive(ctx, disk, strings.NewReader(c.data), "file", p)
		fmt.Fprintf(out, "scan (down=%t): %q stored=%d %v\n", c.down, filesystem.CodeOf(err), objects(ctx, disk), err)
	}
}
```
<!-- /ds:block hash=6dc2d6 -->

## API

| Name | What it is |
|---|---|
| `Scanner` | `Scan(ctx, io.Reader) (Verdict, error)`; an error means the scan did not complete and says nothing about the content |
| `ScannerFunc` | adapts a function to `Scanner`, for cloud scanning APIs and tests |
| `Verdict` | `Clean` (true when nothing was found) and `Signature` (what was found, such as `Win.Test.EICAR_HDB-1`) |
| `Inspect(scanner)` | an `upload.Inspector`: reads the stored file and scans it; infected is `CodeRejected`, a failed scan `CodeUnavailable` unless the scanner already classified its error; either way the upload deletes the file |
| `Clamd` | a clamd client: `Network` (`"tcp"` or `"unix"`), `Address` (`"127.0.0.1:3310"` or a socket path), `Timeout`, `ChunkSize`; streams with `INSTREAM`, so the daemon needs no access to the files |
| `Clamd.Scan(ctx, r)` | implements `Scanner` |
| `Clamd.Ping(ctx)` | checks that clamd answers, for a readiness probe |
| `DefaultTimeout` | two minutes, for one scan including the connection |
| `DefaultChunkSize` | 64 KiB per `INSTREAM` chunk |
| `Digest`, `Fingerprinter`, `MD5`, `SHA256` | a content fingerprint (algorithm and sum) and the functions that compute one while streaming |
| `HashMatcher`, `HashMatcherFunc` | looks a fingerprint up in a block list (a perceptual-hash service, a known-content list) and returns a `Verdict` |
| `HashSet`, `NewHashSet(signature, digests...)` | an in-memory `HashMatcher` over a fixed set of digests |
| `HashScanner(f, m)` | a `Scanner` that fingerprints the content with `f` and asks `m`, so moderation lists plug into `Inspect` like an antivirus |

To scan after the upload instead of inside it, record the file quarantined in records (`records.Quarantine()`) and let a worker call `records.Store.Scan(ctx, scope, id, scanner)` with any `Scanner`: the file is unusable until the scan passes ([Quarantine until scanned](../docs/records/processing-status.md#quarantine-until-scanned)).

## Errors from Clamd

| Situation | Code |
|---|---|
| clamd cannot be reached, the connection breaks, or it answers an error | `CodeUnavailable` |
| the file is larger than clamd's `StreamMaxLength` (100 MiB by default in current ClamAV) | `CodeTooLarge` |

Keep `StreamMaxLength` at least as large as the largest upload a policy allows, or such uploads fail. `task test:clamav` runs the client against a real clamd in Docker.
