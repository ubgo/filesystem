# Security: encryption at rest

Every file encrypted before it reaches the backend, with a master key per tenant: two tenants' files are written through an encrypting disk, the backend is shown to hold only ciphertext, a ranged read is served, one tenant's master key is rotated (data keys re-wrapped, chunks untouched), and the other tenant's key is deleted, which makes its files unreadable. It uses `contrib/crypt` and the `memory` driver. The guide is [Encrypting files at rest](../../docs/plugins/crypt.md).

## Run it

```sh
cd examples
go run ./security-crypt
go test ./security-crypt
```

The KMS is `crypt.MemoryKMS` with keys generated at start-up; in production an adapter over AWS KMS, Cloud KMS or Vault implements `crypt.KMS`. Files go to the memory driver, so nothing is written to disk.

```text
acme/finance/q3.txt: 260000 bytes written, 261088 bytes stored
backend holds the plaintext: false
ranged read at 200000: "ue: confidential. Q3 reven"
rotated 1 acme object(s); readable without the old key: true
globex after its key was deleted: filesystem: read vault:globex/hr/salaries.csv: permission: crypt: no master key "tenant-globex-v0"
printing the KMS: crypt.MemoryKMS{1 keys, redacted}
```
