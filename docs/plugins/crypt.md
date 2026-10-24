# Encrypting files at rest

You will have every file encrypted before it reaches the backend, under a master key that can differ per tenant and live in a key management service, so the storage provider, a leaked bucket or a stolen disk image holds only ciphertext. The package is `contrib/crypt`: a wrapper driver that encrypts in chunks of AES-256-GCM, a `Keyring` that wraps each file's own data key, and `MemoryKMS`, an in-process key store.

## Before you start

- `go get github.com/ubgo/filesystem/contrib/crypt`. It is its own module, uses only the standard library's `crypto/aes` and `crypto/cipher` and the core (`deps_test.go` enforces it), and needs no cgo.
- The example is [`examples/security-crypt`](../../examples/security-crypt/main.go): `go run ./security-crypt` from `examples` writes two tenants' files, shows the backend holds only ciphertext, rotates one tenant's key and deletes the other's.
- Server-side encryption by the provider (S3 SSE-KMS, GCS CMEK, Azure encryption scopes) protects against a stolen disk in the provider's data centre; this package also protects against anyone with read access to the bucket, the provider included. Use both where it matters.

## How a file is stored

Each write draws a fresh 256-bit data key and a random object ID. The keyring wraps the data key under the master key it chooses for the object, and the wrapped key goes into a fixed 1024-byte header; the plaintext follows in 64 KiB chunks, each sealed with the data key and a 16-byte tag. The data key is never stored in the clear and is cleared from memory after use.

| Property | How |
|---|---|
| Nonces never repeat | The data key is new for every write, and each chunk's nonce is its index. |
| Chunks cannot be reordered or swapped between objects | Each chunk's additional data holds its index, the object ID and the format; another object's chunk was sealed under another data key. |
| An object cannot be moved to another key | The storage key is in every chunk's additional data, and in the data key's wrapping, so a file copied from `tenant-a/` to `tenant-b/` in the bucket does not decrypt. |
| Truncation is detected | The last chunk is sealed as final; an object cut at a chunk boundary ends on a chunk that is not, and one cut inside a chunk has a size no plaintext encrypts to. |
| Ranged reads decrypt only their chunks | A range reads the header, a Stat, and only the chunks it covers. |

Anything that fails these checks is `ErrTampered` (code `unknown`), never wrong bytes. What stays visible to the backend: keys, sizes (plaintext size plus 1024 bytes and 16 per chunk), modification times, content types, metadata, headers and tags.

## Step 1 — Wrap the driver

`crypt.New(backend, keyring)` returns the wrapper; put it in `filesystem.NewDisk` like any driver. `crypt.NewKMSKeyring(kms, keyID)` is the keyring over any `crypt.KMS` (encrypt and decrypt under a named key, the shape of AWS KMS, Cloud KMS, Azure Key Vault and Vault transit), with `keyID` choosing the master key per object. Per-tenant keys are a `keyID` that reads the tenant from the key, which is an application decision:

<!-- ds:block id=newvault-hmqdk5pv -->
**newVault** · [`examples/security-crypt/main.go:73-83`](../../examples/security-crypt/main.go#L73-L83)

```go
func newVault(backend filesystem.Driver, kms crypt.KMS, keys *tenantKeys) (*filesystem.Disk, *crypt.Driver, error) {
	drv, err := crypt.New(backend, crypt.NewKMSKeyring(kms, keys.keyID))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, nil, err
	}
	return disk, drv, nil
}
```
<!-- /ds:block hash=a360d9 -->

`crypt.MemoryKMS` holds AES-256 master keys in process memory, for tests, development, and applications that load one key from a secret manager at start-up. It and the driver print as `redacted` under `%v`, `%+v`, `%#v` and `slog`, and no error carries a data key or a master key.

## Write, and check what the backend holds

Write through the disk as usual. To see what encryption does, compare the disk's view with the backend's:

<!-- ds:block id=writeencrypted-m5c9w2tq -->
**writeEncrypted** · [`examples/security-crypt/main.go:107-129`](../../examples/security-crypt/main.go#L107-L129)

```go
func writeEncrypted(ctx context.Context, disk *filesystem.Disk, backend filesystem.Driver, key string, body []byte) (written, stored int64, plaintext bool, err error) {
	if _, err := disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
		return 0, 0, false, err
	}
	st, err := disk.Stat(ctx, key)
	if err != nil {
		return 0, 0, false, err
	}
	raw, err := backend.Stat(ctx, key)
	if err != nil {
		return 0, 0, false, err
	}
	rc, err := backend.Read(ctx, key, filesystem.ByteRange{})
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = rc.Close() }()
	onDisk, err := io.ReadAll(rc)
	if err != nil {
		return 0, 0, false, err
	}
	return st.Size, raw.Size, bytes.Contains(onDisk, body[:min(len(body), 16)]), nil
}
```
<!-- /ds:block hash=fe1662 -->

Check: for 260,000 bytes the example prints `acme/finance/q3.txt: 260000 bytes written, 261088 bytes stored` (a header and one 16-byte tag per 64 KiB chunk) and `backend holds the plaintext: false`. A ranged read decrypts only the chunks it covers: `ranged read at 200000: "ue: confidential. Q3 reven"`.

## Capabilities

The wrapper declares only what holds on ciphertext. Forwarded: delete by prefix or key list, visibility, modification time, tags, holds, storage class and restore, metadata, content types, headers, conditional writes and compare-and-swap (on the ciphertext's ETag), sweeping abandoned uploads and its dry run (`FindAbandoned`), and real directories (`MakeDir`, `DirExists`; a directory holds no bytes). Not forwarded: backend-signed and public URLs and browser uploads (they would serve or accept ciphertext), multipart and append, server-side copy and move (the storage key is authenticated, so the Disk's fallback streams and re-encrypts), version reads, backend SHA-256 checks and conditional reads. Serve encrypted files through the application, with `fshttp` or [signed URLs](../concepts/serving-files.md) checked by the application. The conformance suite runs over the wrapper on the memory driver.

`Stat` and `List` report the plaintext size; the ETag is the ciphertext's, which changes on every write, even of the same bytes.

## Step 2 — Rotate keys

`(*crypt.Driver).Rotate(ctx, key)` re-wraps one object's data key under the master key the keyring now chooses, and nothing else: no chunk is decrypted, and the chunks are copied byte for byte behind the new header. Make the keyring choose the new key, rotate every object, then retire the old key:

<!-- ds:block id=rotatetenant-ns8jhjmy -->
**rotateTenant** · [`examples/security-crypt/main.go:87-100`](../../examples/security-crypt/main.go#L87-L100)

```go
func rotateTenant(ctx context.Context, disk *filesystem.Disk, drv *crypt.Driver, keys *tenantKeys, tenant string) (int, error) {
	keys.rotate(tenant)
	n := 0
	for e, err := range disk.List(ctx, tenant, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return n, err
		}
		if err := drv.Rotate(ctx, e.Key); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
```
<!-- /ds:block hash=e0b4da -->

Object stores cannot rewrite part of an object, so each rotation writes the object again through the wrapped driver, keeping its content type, visibility, metadata and headers. With compare-and-swap (S3, GCS, Azure, local, memory) a write that lands during a rotation makes it fail with `CodeConflict` instead of being overwritten; rotate that key again.

Deleting a tenant's master key makes every file of that tenant unreadable at once (crypto-shredding): the KMS answers `permission`, and the ciphertext left in backups is useless.

Output of `go run ./security-crypt`:

```text
acme/finance/q3.txt: 260000 bytes written, 261088 bytes stored
backend holds the plaintext: false
ranged read at 200000: "ue: confidential. Q3 reven"
rotated 1 acme object(s); readable without the old key: true
globex after its key was deleted: filesystem: read vault:globex/hr/salaries.csv: permission: crypt: no master key "tenant-globex-v0"
printing the KMS: crypt.MemoryKMS{1 keys, redacted}
```

## Try it

```sh
cd examples
go run ./security-crypt
go test ./security-crypt
```

```text
acme/finance/q3.txt: 260000 bytes written, 261088 bytes stored
backend holds the plaintext: false
ranged read at 200000: "ue: confidential. Q3 reven"
rotated 1 acme object(s); readable without the old key: true
globex after its key was deleted: filesystem: read vault:globex/hr/salaries.csv: permission: crypt: no master key "tenant-globex-v0"
printing the KMS: crypt.MemoryKMS{1 keys, redacted}
```

Deleting a tenant's master key makes all of its files unreadable at once: crypto-shredding, the way to honour an erasure request for data you cannot find object by object.

## Pitfalls

- The disk must hold only objects this package wrote: `Stat`, `List` and `Read` refuse anything else with `ErrTampered`. Encrypt an existing bucket by copying through the wrapper (`filesystem.CopyAcross` from the plain disk to the encrypting one).
- A ranged read costs three requests (header, Stat, chunks); a whole read costs one.
- Losing a master key loses every file wrapped under it. Keep master keys in a KMS with its own backups, never only in `MemoryKMS`.
