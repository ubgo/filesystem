# Azure Blob Storage driver

The Azure driver stores files as block blobs in an Azure Storage container. This program opens a container from a DSN whose secret stays in the environment, writes once with a conditional write, issues SAS upload and download links and uses them over plain HTTP, assembles a file from server-side multipart blocks, and shows the two things Azure cannot do per object: public visibility and presigned parts. It uses the core `filesystem` package and `github.com/ubgo/filesystem/adapters/azure` (built on `azblob`). The guide [Azure Blob Storage](../../docs/adapters/azure.md) explains each step.

## Run it

There is no in-process Azure fake, so the program needs Azurite (the Azure emulator) or a real storage account. From the repo root, `task azure:up` starts a throwaway Azurite from the repository's `compose.yml` on `127.0.0.1:10000` (`task azure:down` removes it and its data); then point the program at it with its well-known development connection string:

```sh
cd examples
export AZURE_STORAGE_CONNECTION_STRING='DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;'
go run ./adapters-azure
```

For a real account, put that account's connection string in `AZURE_STORAGE_CONNECTION_STRING` instead. Without the variable the program runs only its first steps, block size and concurrency, per-call options and a front end, which need no account (see below), then exits with status 2. It creates the container `media` when missing and leaves its blobs there. `task azure:down` stops Azurite.

The test skips unless `FILESYSTEM_AZURE_TEST_CONNECTION` holds a connection string; with it, each run uses a fresh randomly named container:

```sh
FILESYSTEM_AZURE_TEST_CONNECTION="$AZURE_STORAGE_CONNECTION_STRING" go test ./adapters-azure
```

## What it does

First, with or without a connection string, block size and concurrency for large blobs, against an in-process stand-in that answers only the calls a `Write` makes and counts the blocks it stages:

```text
defaults: 9 MiB staged as 2 blocks (MiB: 8,1), at most 5 in flight: true
2 MiB blocks, one at a time: 5 MiB staged as 3 blocks (MiB: 2,2,1), at most 1 in flight: true
1 MiB blocks, three at once: 9 MiB staged as 9 blocks (MiB: 1,1,1,1,1,1,1,1,1), at most 3 in flight: true
512 KiB blocks: invalid_argument
4001 MiB blocks: invalid_argument
negative concurrency: invalid_argument
```

The default stages 8 MiB blocks (`DefaultBlockSize`), so 9 MiB is one 8 MiB block and the remainder. A backup disk with `BlockSize: 2 MiB, Concurrency: 1` stages 2 MiB blocks one at a time; `Concurrency: 3` lets three blocks be in flight at once and never more, so a `Write` holds at most `Concurrency` × `BlockSize` in memory, while `MaxBlocks` × `BlockSize` caps the largest blob. A block size outside [1 MiB, 4000 MiB] or a negative concurrency is refused when the disk opens. From a DSN the same settings are `azblob://backups?block_size=2097152&concurrency=1`.

Next, also without an account, per-call options and a front end, against an in-process stand-in (`frontdoor.go`) that records what each request carried:

```text
per call: contracts/2026/acme.pdf tier="Cool" scope="tenant-acme" tags="tenant=acme"
per call: thumbs/acme.png tier="" scope="" tags=""
per call: legal holds set on /legal/contracts/2026/acme.pdf
per call: unknown tier invalid_argument, request sent false
front door: key requests went to the configured host: true, signed URL keeps it: true
front door: signature differs from the host-label account's: true
front door: AccountName without Credential: invalid_argument
front door: AccountName "Files.Example": invalid_argument
```

`azure.WithRequestOptions` binds an encryption scope, the Cool tier, an index tag and a legal hold to `contracts/2026/acme.pdf`: its Put Blob carries the tier, scope and tags in the same request, and the hold follows as a second request. A thumbnail written with the same context carries none of them, because the options apply only to the key they are bound to, and an unknown tier is `invalid_argument` before any request is sent. Then a disk reached through a front end whose host does not start with the account name sets `AccountName` (with `Credential` and `DelegationClientOptions`): the user delegation key is still fetched from the configured host and the signed URL keeps that host, but the signature is made for `mystorageacct` rather than for the host's first label, which is what Azure checks. `AccountName` without `Credential`, or one that is not a valid account name, is refused when the disk opens. The adapter's own tests prove the signature equals the SDK's for that account.

Then against Azurite:

```text
capabilities: [copy,presign,multipart,sweep-abandoned,find-abandoned,metadata,content-type,headers,presign-ip,conditional-write,append,tags,hold,storage-class,restore,compare-and-swap]
second conditional write: exists
stat: 4 bytes, order=1042, cache-control="private, max-age=0"
signed PUT requires X-Ms-Blob-Type: BlockBlob
signed GET: "scanned"
multipart: exports/big.csv is 14 bytes, text/csv
public write: unsupported
presigned part: unsupported
checksums: matching digest stored, wrong digest invalid_argument, stored=false
```

1. The disk opens from `azblob://media`; the account name and key come from the connection string, and the container is created through the native client when missing.
2. A receipt is written with `IfNotExists`, metadata and a `Cache-Control` header; the second write is refused with `exists`, and `Stat` reads the metadata and header back.
3. A SAS PUT link is issued. Its `Header` carries `X-Ms-Blob-Type: BlockBlob`, which the client must send; the upload goes through it, and a SAS GET link downloads the same bytes.
4. Two parts are staged as blocks through the server and committed into one 14-byte CSV.
5. A public write and a presigned part are both `unsupported`: Azure visibility is a container setting, and browser part uploads are not offered.
6. A blob written with the SHA-256 its body has is stored; one written with a wrong digest is refused with `invalid_argument` by the Disk before the block list is committed, so nothing appears at its key. (This last line is the Disk's own check and is the same on every backend; it was added without an Azurite run, so `task test:azure` is what confirms it here.)

## The code

The DSN names only the container; the secret stays in `AZURE_STORAGE_CONNECTION_STRING` (or the variable named by the `connection_env` parameter).

<!-- ds:block id=openwithdsn-bq3tj2xf -->
**openWithDSN** · [`examples/adapters-azure/main.go:86-92`](main.go#L86-L92)

```go
func openWithDSN(ctx context.Context, container string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(azure.Scheme, azure.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "media", azure.Scheme+"://"+container)
}
```
<!-- /ds:block hash=f2ebd8 -->

The same disk built in code from an account client the application made itself. `SharedKey` is what enables SAS signing; without it `SignedURL` is `unsupported`.

<!-- ds:block id=openwithconfig-rqy7ht78 -->
**openWithConfig** · [`examples/adapters-azure/main.go:98-112`](main.go#L98-L112)

```go
func openWithConfig(accountURL, accountName, accountKey, container string) (*filesystem.Disk, error) {
	cred, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClientWithSharedKeyCredential(accountURL, cred, nil)
	if err != nil {
		return nil, err
	}
	drv, err := azure.New(azure.Config{Client: client, Container: container, SharedKey: cred})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}
```
<!-- /ds:block hash=5b3094 -->

The driver never creates containers; the program does it through `filesystem.As`, the escape hatch to the native `*azblob.Client`.

<!-- ds:block id=ensurecontainer-4darszyg -->
**ensureContainer** · [`examples/adapters-azure/main.go:117-127`](main.go#L117-L127)

```go
func ensureContainer(ctx context.Context, disk *filesystem.Disk, container string) error {
	client, ok := filesystem.As[*azblob.Client](disk)
	if !ok {
		return fmt.Errorf("disk %s is not an Azure disk", disk.Name())
	}
	_, err := client.CreateContainer(ctx, container, nil)
	if bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return nil
	}
	return err
}
```
<!-- /ds:block hash=a23285 -->

A conditional write with metadata and headers, then the refused second attempt.

<!-- ds:block id=writeonce-xwgv3s3k -->
**writeOnce** · [`examples/adapters-azure/main.go:132-150`](main.go#L132-L150)

```go
func writeOnce(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	key := "receipts/" + strings.ToLower(rand.Text()) + ".txt"
	opts := filesystem.WriteOptions{
		IfNotExists: true,
		Metadata:    map[string]string{"order": "1042"},
		Headers:     filesystem.HTTPHeaders{CacheControl: "private, max-age=0"},
	}
	if _, err := disk.WriteBytes(ctx, key, []byte("paid"), opts); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, key, []byte("paid twice"), opts)
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	e, err := disk.Stat(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %d bytes, order=%s, cache-control=%q\n", e.Size, e.Metadata["order"], e.Headers.CacheControl)
	return nil
}
```
<!-- /ds:block hash=29e2f5 -->

SAS links: the PUT link's required headers travel in `SignedRequest.Header`, so the client sends exactly what the signature expects.

<!-- ds:block id=saslinks-bfgr6paa -->
**sasLinks** · [`examples/adapters-azure/main.go:155-174`](main.go#L155-L174)

```go
func sasLinks(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	put, err := disk.SignedURL(ctx, "inbox/scan.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: linkLifetime, ContentType: "text/plain"})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed PUT requires %s: %s\n", headerBlobType, put.Header.Get(headerBlobType))
	if _, err := send(ctx, http.MethodPut, put.URL, strings.NewReader("scanned"), put.Header); err != nil {
		return err
	}
	get, err := disk.SignedURL(ctx, "inbox/scan.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	body, err := send(ctx, http.MethodGet, get.URL, nil, get.Header)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET: %q\n", body)
	return nil
}
```
<!-- /ds:block hash=fc67da -->

A server-side multipart upload on staged blocks. Parts may be any size; Azure keeps uncommitted blocks for seven days.

<!-- ds:block id=servermultipart-ayhqr3w8 -->
**serverMultipart** · [`examples/adapters-azure/main.go:180-200`](main.go#L180-L200)

```go
func serverMultipart(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	const key = "exports/big.csv"
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{ContentType: "text/csv"})
	if err != nil {
		return err
	}
	var parts []filesystem.Part
	for i, chunk := range []string{"id,total\n", "1,42\n"} {
		p, err := disk.UploadPart(ctx, key, id, i+1, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			return err
		}
		parts = append(parts, p)
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %s is %d bytes, %s\n", e.Key, e.Size, e.ContentType)
	return nil
}
```
<!-- /ds:block hash=a6285d -->

What Azure refuses, as error codes rather than silent fallbacks.

<!-- ds:block id=refusedoptions-wwr4ncm8 -->
**refusedOptions** · [`examples/adapters-azure/main.go:205-211`](main.go#L205-L211)

```go
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "public.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	fmt.Fprintf(out, "public write: %s\n", filesystem.CodeOf(err))
	_, err = disk.PresignPart(ctx, "k", "id", 1, linkLifetime)
	fmt.Fprintf(out, "presigned part: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=198374 -->

How a declared SHA-256 is checked: by the Disk, while streaming, before Azure commits anything.

<!-- ds:block id=checksums-bu9zpz2k -->
**checksums** · [`examples/adapters-azure/main.go:219-233`](main.go#L219-L233)

```go
func checksums(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	body := []byte("invoice 1042, total 42.00")
	sum := sha256.Sum256(body)
	if _, err := disk.WriteBytes(ctx, "invoices/1042.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(sum[:])}); err != nil {
		return err
	}
	wrong := sha256.Sum256([]byte("tampered"))
	_, err := disk.WriteBytes(ctx, "invoices/1043.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(wrong[:])})
	stored, existsErr := disk.Exists(ctx, "invoices/1043.txt")
	if existsErr != nil {
		return existsErr
	}
	fmt.Fprintf(out, "checksums: matching digest stored, wrong digest %s, stored=%t\n", filesystem.CodeOf(err), stored)
	return nil
}
```
<!-- /ds:block hash=f0a9dc -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: open by DSN or Config, create the container, conditional write, SAS links, multipart, refusals |
| `blocks.go` | Block size and concurrency for large blobs, against an in-process stand-in that counts staged blocks; code: [`backupDisk`](ds:block?id=backupdisk-f6m2x9kr) |
| `frontdoor.go` | Per-call options with `azure.RequestOptions`, and `AccountName` behind a front end, against an in-process stand-in; code: [`writeContract`](ds:block?id=writecontract-k9v4n2xs), [`perCall`](ds:block?id=percall-t5c8w3mq), [`frontDoorDisk`](ds:block?id=frontdoordisk-r2h6p9wd), [`frontDoor`](ds:block?id=frontdoor-a7m3x5qn) |
| `main_test.go` | Runs the program in a fresh container and checks each line the guide quotes; opens the disk from a Config; skips without `FILESYSTEM_AZURE_TEST_CONNECTION`, except the block-size, per-call and front-end tests, which always run |

## Next

- The guide: [Azure Blob Storage](../../docs/adapters/azure.md), and the driver reference [adapters/azure/README.md](../../adapters/azure/README.md).
- Other backends: [S3](../adapters-s3), [GCS](../adapters-gcs), [local](../adapters-local), [memory](../adapters-memory), [SFTP](../adapters-sftp).
- [Capabilities](../concepts-capabilities) explains why a missing feature is a code, not a fake.
