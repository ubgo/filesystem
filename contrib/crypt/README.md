# contrib/crypt

`github.com/ubgo/filesystem/contrib/crypt` (package `crypt`) encrypts every object before it reaches a backend: a wrapper driver sealing 64 KiB chunks with AES-256-GCM under a fresh data key per object, which a `Keyring` wraps under a master key chosen per object (per tenant, from a KMS). Ranged reads decrypt only the chunks they need; reordered, swapped, moved or truncated ciphertext fails with `ErrTampered`. It imports nothing but the standard library (`crypto/aes`, `crypto/cipher`) and the core (`deps_test.go` enforces it), and the conformance suite runs over it.

```sh
go get github.com/ubgo/filesystem/contrib/crypt
```

The guide, with output from a run: [encrypting files at rest](../../docs/plugins/crypt.md). The disk of [`examples/security-crypt`](../../examples/security-crypt/main.go):

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

## API

| Name | What it is |
|---|---|
| `New(inner filesystem.Driver, keyring Keyring) (*Driver, error)` | the encrypting wrapper; declares only the capabilities that hold on ciphertext |
| `(*Driver).Rotate(ctx, key) error` | re-wraps one object's data key under the keyring's current master key; chunks are copied unchanged |
| `Keyring` | `WrapKey` / `UnwrapKey` of per-object data keys: the extension point for key policy |
| `KMS`, `NewKMSKeyring(kms, keyID KeyIDFunc) Keyring` | the shape of a KMS's encrypt and decrypt calls, and the keyring over it; `keyID` chooses the master key per object |
| `MemoryKMS`, `NewMemoryKMS()`, `Add`, `Remove` | AES-256 master keys in process memory; prints as `redacted` |
| `WrappedKey{KeyID, Ciphertext}` | a data key as stored in the header |
| `CiphertextSize`, `PlaintextSize`, `HeaderSize`, `ChunkSize`, `TagSize` | the size arithmetic of the format |
| `ErrTampered` | wrapped by every error for ciphertext that does not authenticate |
