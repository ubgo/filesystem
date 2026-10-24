# Security: archive uploads

Zip uploads accepted safely: every upload is walked under limits by an `upload.Policy` inspector before it is reported stored, and a clean one is extracted into the importing user's folder. A zip bomb, a zip-slip archive and an archive hiding a program are each refused, and nothing of them is kept. It uses `contrib/archive`, `upload`, `sniff` and the `memory` driver. The guide is [Accepting archives safely](../../docs/plugins/archive.md).

## Run it

```sh
cd examples
go run ./security-archive
go test ./security-archive
```

The archives are built in memory by the program, so each attack is readable in `main.go`; files go to the memory driver, so nothing is written to disk.

```text
documents.zip  stored users/42/imported/contracts/acme.txt (6 bytes)
documents.zip  stored users/42/imported/notes.md (7 bytes)
bomb.zip       refused: filesystem: rejected: archive: limit exceeded: decompresses to more than 100 times its size
slip.zip       refused: filesystem: rejected: archive: unsafe entry: name "../../etc/cron.d/job": filesystem: invalid_key: key contains a ".." segment
invoice.zip    refused: filesystem: rejected: archive entry "invoice.pdf.exe" is a program
preview        contracts/acme.txt (6 bytes)
preview        notes.md (7 bytes)
on the disk: [users/42/imported/contracts/acme.txt users/42/imported/notes.md]
```
