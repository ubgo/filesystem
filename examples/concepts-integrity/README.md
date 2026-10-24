# Integrity: digests, declared sizes, ETags and content-addressed keys

A stored file is only useful if it is the file that was sent. This program shows the checks that prove it: the SHA-256 every write computes while it streams, a declared digest that keeps a corrupted body from replacing the good version, a declared size that refuses a truncated body, ETags for cheap change checks, and content-addressed keys that store two identical uploads once. It uses the core `filesystem` package, `memory` and `upload`. The guide [Integrity](../../docs/concepts/integrity.md) explains each step, the checksums each backend verifies, and scrubbing stored files against their records.

## Run it

```sh
cd examples
go run ./concepts-integrity
go test ./concepts-integrity
```

Everything runs on a memory disk: no network, no environment variables.

## What it does

```sh
go run ./concepts-integrity
```

```text
write: 29 bytes, sha256 matches an independent hash: true
corrupted body with a declared sha256: invalid_argument
stored version is still the first: true
good body with a declared sha256: stored
truncated body with a size hint: invalid_argument, stored: false
etag: same bytes keep it (memory): true, new bytes change it: true, stat reports the latest: true
content-addressed: same key twice: true, key is the sha256: true, objects: 1
digests: crc32c 79a02f78 (as declared: true), md5 8e8333e47b0d58a90c1c846824684368, sha256 as always: true
corrupted body with a declared crc32c: invalid_argument, stored version is still the first: true
```

1. A write returns the SHA-256 the Disk computed while streaming, equal to one computed independently.
2. A body that does not match its declared SHA-256 is refused before it replaces anything; the first version is still stored, and the good body is accepted.
3. A body shorter than its `SizeHint` is refused and nothing is stored.
4. On the memory driver the ETag is the content's MD5: rewriting the same bytes keeps it, new bytes change it, and `Stat` reports the latest.
5. Two uploads of one file under `upload.ContentAddressed` land on one key named by the content's SHA-256: one object.
6. `WriteOptions.Checksums` computes more digests in the same pass: a CRC32C held to the value the sender declared and an MD5 computed only, reported in `Entry.Checksums` beside the SHA-256. A body whose CRC32C does not match is refused with `invalid_argument` before it replaces anything.

## The code

How a client or an upstream store computes the digest it declares.

<!-- ds:block id=digest-fqnk4769 -->
**digest** · [`examples/concepts-integrity/main.go:63-66`](main.go#L63-L66)

```go
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
```
<!-- /ds:block hash=5cddbf -->

The digest every write returns.

<!-- ds:block id=streameddigest-898w4g6t -->
**streamedDigest** · [`examples/concepts-integrity/main.go:72-79`](main.go#L72-L79)

```go
func streamedDigest(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Write(ctx, reportKey, strings.NewReader(report), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "write: %d bytes, sha256 matches an independent hash: %t\n", e.Size, e.SHA256 == digest(report))
	return nil
}
```
<!-- /ds:block hash=281cf1 -->

A declared digest refusing a corrupted body.

<!-- ds:block id=declareddigest-74fh5emm -->
**declaredDigest** · [`examples/concepts-integrity/main.go:85-100`](main.go#L85-L100)

```go
func declaredDigest(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	next := "region,revenue\neu,125\nus,340\n"
	corrupted := strings.Replace(next, "125", "12\x00", 1)
	_, err := disk.Write(ctx, reportKey, strings.NewReader(corrupted), filesystem.WriteOptions{SHA256: digest(next)})
	fmt.Fprintf(out, "corrupted body with a declared sha256: %s\n", filesystem.CodeOf(err))
	b, err := disk.ReadAll(ctx, reportKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored version is still the first: %t\n", string(b) == report)
	if _, err := disk.Write(ctx, reportKey, strings.NewReader(next), filesystem.WriteOptions{SHA256: digest(next)}); err != nil {
		return err
	}
	fmt.Fprintln(out, "good body with a declared sha256: stored")
	return nil
}
```
<!-- /ds:block hash=3be559 -->

A declared size refusing a truncated body.

<!-- ds:block id=declaredsize-d6thjej9 -->
**declaredSize** · [`examples/concepts-integrity/main.go:106-112`](main.go#L106-L112)

```go
func declaredSize(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	truncated := report[:len(report)-5]
	_, err := disk.Write(ctx, "reports/q4.csv", strings.NewReader(truncated), filesystem.WriteOptions{SizeHint: int64(len(report))})
	ok, _ := disk.Exists(ctx, "reports/q4.csv")
	fmt.Fprintf(out, "truncated body with a size hint: %s, stored: %t\n", filesystem.CodeOf(err), ok)
	return nil
}
```
<!-- /ds:block hash=4fa891 -->

ETags.

<!-- ds:block id=etags-g9xa5g2g -->
**etags** · [`examples/concepts-integrity/main.go:121-141`](main.go#L121-L141)

```go
func etags(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	first, err := disk.WriteBytes(ctx, "a.txt", []byte("same"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	again, err := disk.WriteBytes(ctx, "a.txt", []byte("same"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	changed, err := disk.WriteBytes(ctx, "a.txt", []byte("different"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	now, err := disk.Stat(ctx, "a.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "etag: same bytes keep it (memory): %t, new bytes change it: %t, stat reports the latest: %t\n",
		first.ETag == again.ETag, changed.ETag != again.ETag, now.ETag == changed.ETag)
	return nil
}
```
<!-- /ds:block hash=21b1b6 -->

Content-addressed keys.

<!-- ds:block id=contentaddressed-fnuean6r -->
**contentAddressed** · [`examples/concepts-integrity/main.go:147-169`](main.go#L147-L169)

```go
func contentAddressed(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}
	owner := func(context.Context) (string, error) { return tenant, nil }
	p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Owner: owner})
	var keys []string
	for _, name := range []string{"logo.png", "logo-copy.png"} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), name, p)
		if err != nil {
			return err
		}
		keys = append(keys, res.Key)
	}
	n := 0
	for _, err := range disk.List(ctx, "blobs", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "content-addressed: same key twice: %t, key is the sha256: %t, objects: %d\n",
		keys[0] == keys[1], strings.TrimSuffix(path.Base(keys[0]), ".png") == digest(pngFile), n)
	return nil
}
```
<!-- /ds:block hash=18dfaa -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: one function per check |
| `digests.go` | CRC32C and MD5 beside the SHA-256, one declared and one computed; code: [`crc32c`](ds:block?id=crc32c-m3v7q9tk) |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Integrity](../../docs/concepts/integrity.md).
- Scrubbing stored files against their records: [maintenance](../maintenance).
- Deduplication with records: [records-files](../records-files).
