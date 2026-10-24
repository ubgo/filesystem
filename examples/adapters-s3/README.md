# Amazon S3 driver

The S3 driver stores files in an S3 bucket on AWS or any S3-compatible store. This program builds the client the way an application does, opens a bucket from a Config and from a DSN, stores metadata and a `Content-Disposition` header, issues SigV4 signed download and upload links and uses them over plain HTTP, runs a multipart upload whose part goes straight from the client to the bucket, deletes by key list and by prefix, and shows the two options the default configuration refuses rather than fakes. It then walks through the object-store features the fake can model or record: storage classes, bucket versions, a content-type override on a signed link, checksums S3 verifies and stores, one KMS key per tenant on a shared disk, customer-provided keys (SSE-C), lifecycle and replication rules, credentials scoped to one prefix, and a bring-your-own bucket reached through a role in the customer's account. It uses the core `filesystem` package and `github.com/ubgo/filesystem/adapters/s3` (built on `aws-sdk-go-v2`). The guide [Amazon S3](../../docs/adapters/s3.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-s3
go test ./adapters-s3
```

It runs against an in-process fake S3 ([gofakes3](https://github.com/johannesboyne/gofakes3)) on a random local port, with the access key and secret `test` (the program sets `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` for the DSN opener, which reads the AWS default chain). It needs no account, Docker or network, and the fake's bucket disappears when the program exits. The program has no switch to a real bucket: against a real provider only the client's endpoint, region and credentials change. To prove the driver against a real store, `task test:s3` (from the repo root) runs the driver's conformance suite against SeaweedFS on `127.0.0.1:8333`, and `FILESYSTEM_S3_TEST_DSN` points that suite at any other provider, as the guide describes.

## What it does

```text
capabilities: [copy,delete-prefix,delete-keys,presign,multipart,presign-part,presign-post,metadata,content-type,headers,presign-headers,checksum-sha256]
stat: reports/q3.csv text/csv owner=finance disposition=attachment; filename="q3.csv"
signed GET: 200 "month,total"
signed PUT: 200, stored 19 bytes as text/plain
multipart: 1 part(s), videos/clip.bin is 23 bytes
after deletes: 0 keys under tmp/
public write: unsupported
conditional write: unsupported
via DSN: reports/q3.csv is 20 bytes
storage class: STANDARD_IA, then GLACIER_IR
versions: 2, newest latest=true, oldest reads "draft"
signed GET with a type override: 200 text/plain
checksum sent: x-amz-checksum-sha256=true, payload hash is the digest=true
wrong digest: invalid_argument
stored crc32c: mnG7TA==, presign declared: false
kms: tenants/acme/logo.txt -> aws:kms arn:aws:kms:eu-west-1:111122223333:key/acme-key
kms: tenants/globex/logo.txt -> aws:kms arn:aws:kms:eu-west-1:111122223333:key/globex-key
kms: shared/terms.txt -> aws:kms arn:aws:kms:eu-west-1:111122223333:key/default-key
sse-c: read "for your eyes only" with algorithm AES256, key printed as s3.CustomerKey{[redacted]}, presign declared: false
lifecycle: tmp,uploads,logs; logs move to GLACIER after 90 days
replication: contracts-dr contracts/ -> arn:aws:s3:::history-eu-west-1, deletes replicated: false
delegated: arn:aws:s3:::media/users/42/*
credentials print as: s3.ScopedCredentials{AccessKeyID: "ASIAROLEEXAMPLE", SecretAccessKey: [REDACTED], SessionToken: [REDACTED], Bucket: "media", Prefix: "users/42"}
byo bucket: STS saw external ID ext-7f3c9a; PUT signed by ASIAROLEEXAMPLE with a session token: true
role provider prints as: s3.RoleCredentials{RoleARN: "arn:aws:iam::444455556666:role/app-storage", SessionName: "tenant-acme", [REDACTED]}
tags on write: class=billing,tenant=acme; after SetTags: retain=7y,tenant=acme; the copy keeps: retain=7y,tenant=acme
legal hold: true; delete: permission; overwrite: permission
hold lifted, delete: <nil>
archived read: conflict, ErrArchived: true
restore asked at tier Bulk: ongoing
restored: reads "old logs", restored until 2040-12-21T00:00:00Z
govcloud: objects arn:aws-us-gov:s3:::media/users/42/*; kms [kms:Decrypt kms:GenerateDataKey] on arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab
china partition for a govcloud role: invalid_argument
kms alias instead of a key: invalid_argument
```

1. With the defaults, the disk declares neither `set-visibility` nor `conditional-write`: both depend on the provider and are switched on explicitly.
2. A CSV is written with a content type, user metadata and a `Content-Disposition` header, and `Stat` reads all three back.
3. A signed GET link forcing a download is fetched with a plain HTTP client, as a browser would.
4. A signed PUT link binds the content type into the signature; the client sends the headers in `SignedRequest.Header` and the object is stored as `text/plain`.
5. A multipart upload: the server creates it and signs part 1, the client PUTs the part directly to the bucket, and the server completes it from the parts S3 recorded.
6. Two keys are deleted in one batch request and the rest of `tmp/` by prefix.
7. A public write and a conditional write are both `unsupported` in the default configuration: never a silently private file or a silent overwrite.
8. The same bucket opened from a DSN sees the CSV written through the Config disk.
9. Storage classes (`features.go`, `storageClasses`): a disk with `StorageClasses` and `StorageClass: STANDARD_IA` writes into that class, and `SetStorageClass` moves the object to `GLACIER_IR` by copying it onto itself; `Stat` reports the class each time.
10. Bucket versions (`bucketVersions`): versioning is turned on, `CheckBucket` proves the bucket has it, the same key is written twice, `Versions` lists both newest first and `ReadVersion` reads the older one.
11. A signed GET whose `PresignOptions.ContentType` overrides the stored `application/octet-stream`: the fake answers with `text/plain`, as S3 does for `response-content-type`.
12. A write with `WriteOptions.SHA256` and `SizeHint` sends the digest as `x-amz-checksum-sha256` and as the SigV4 payload hash (read from the fake's request log); a wrong digest is `invalid_argument`. Here the refusal comes from the Disk's own check, because gofakes3 does not verify checksums; S3 refuses the same body on its side.
13. A disk with `ChecksumAlgorithm: CRC32C` has the store keep a CRC32C of every write, which `Stat` returns under `s3:checksum-crc32c`; the disk declares no presigning, because a checksum cannot be signed before the body exists.
14. One KMS key per tenant: `SSEKMSKeyFor` picks the key from the driver key under each scoped disk's prefix, and a key outside any tenant falls back to the disk's `SSEKMSKeyID`. The lines show the encryption headers each PUT carried.
15. SSE-C: `NewCustomerKey` holds a 32-byte key that goes on every request (the read carried `AES256` and the key), formats as a placeholder, and switches presigning off.
16. Lifecycle rules (`SetLifecycle`, `Lifecycle`) and cross-region replication (`SetReplication`, `Replication`) are set and read back.
17. `DelegatePrefix` issues credentials whose session policy reaches only `users/42/` of the bucket; they print with the secret and token redacted.
18. Bring your own bucket: `RoleCredentials` assumes a role in the customer's account with an external ID, and every S3 request is signed with the role's temporary key and session token; the provider prints only the role and session name.
19. Object tags (`objects.go`, [`objectTags`](ds:block?id=objecttags-u5m8k2ws)): a disk with `Tagging` stores the tags a write carried in `RequestOptions.Tags`, `SetTags` replaces them, and a copy keeps its source's tags.
20. A legal hold ([`legalHold`](ds:block?id=legalhold-c9r3t6vy)): `CheckBucket` proves the bucket has Object Lock, a write with `RequestOptions.LegalHold` is held, so deleting or overwriting it is `permission` (the driver refuses before S3 could hide the held version behind a delete marker), and once `SetHold` lifts the hold the delete goes through.
21. An archive restore ([`archiveRestore`](ds:block?id=archiverestore-k7w4n9pd)): an object written into `GLACIER` cannot be read (`conflict`, `s3.ErrArchived`) until `Disk.Restore` has run; the restore is asked at the disk's `RestoreTier`, `Stat` reports it `ongoing`, then `restored` with its expiry, and the object reads.
22. Delegation in GovCloud on an SSE-KMS bucket ([`govCloudDelegation`](ds:block?id=govclouddelegation-q2h6x8bm)): the partition comes from the role's ARN, so every ARN of the session policy names `aws-us-gov`, and the listed key gets one statement with `kms:Decrypt` and `kms:GenerateDataKey`. A partition that disagrees with the role's and a KMS alias instead of a key ARN are refused.

## The code

The client is the application's: region, endpoint, credentials and path-style addressing are its business, not the driver's.

<!-- ds:block id=newclient-gwqpka5e -->
**newClient** · [`examples/adapters-s3/main.go:112-119`](main.go#L112-L119)

```go
func newClient(endpoint string) *s3.Client {
	return s3.New(s3.Options{
		Region:       fakeRegion,
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(fakeAccessKey, fakeSecretKey, ""),
	})
}
```
<!-- /ds:block hash=947337 -->

The driver from a Config, with a 5 MiB part size; visibility, conditional writes and prefix pruning stay at their defaults.

<!-- ds:block id=openwithconfig-3yavfaxb -->
**openWithConfig** · [`examples/adapters-s3/main.go:132-142`](main.go#L132-L142)

```go
func openWithConfig(client *s3.Client) (*filesystem.Disk, error) {
	drv, err := fss3.New(fss3.Config{
		Client:   client,
		Bucket:   bucketName,
		PartSize: partSize,
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}
```
<!-- /ds:block hash=4a193c -->

The DSN of the same bucket. Nothing secret is in it: credentials come from the AWS default chain.

<!-- ds:block id=mediadsn-6tm5jvuy -->
**mediaDSN** · [`examples/adapters-s3/main.go:147-154`](main.go#L147-L154)

```go
func mediaDSN(endpoint string) string {
	q := url.Values{}
	q.Set(fss3.ParamRegion, fakeRegion)
	q.Set(fss3.ParamEndpoint, endpoint)
	q.Set(fss3.ParamPathStyle, "true")
	q.Set(fss3.ParamPartSize, fmt.Sprint(partSize))
	return fss3.Scheme + "://" + bucketName + "?" + q.Encode()
}
```
<!-- /ds:block hash=37f3bf -->

<!-- ds:block id=openwithdsn-wrexy9vp -->
**openWithDSN** · [`examples/adapters-s3/main.go:159-165`](main.go#L159-L165)

```go
func openWithDSN(ctx context.Context, endpoint string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return nil, err
	}
	return mux.Open(ctx, "media-dsn", mediaDSN(endpoint))
}
```
<!-- /ds:block hash=9718ba -->

Content type, metadata and a header stored with the object and read back.

<!-- ds:block id=writewithmetadata-83sa5aek -->
**writeWithMetadata** · [`examples/adapters-s3/main.go:170-185`](main.go#L170-L185)

```go
func writeWithMetadata(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "reports/q3.csv", []byte("month,total\njuly,42\n"), filesystem.WriteOptions{
		ContentType: "text/csv",
		Metadata:    map[string]string{"owner": "finance"},
		Headers:     filesystem.HTTPHeaders{ContentDisposition: `attachment; filename="q3.csv"`},
	})
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "reports/q3.csv")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %s owner=%s disposition=%s\n", e.Key, e.ContentType, e.Metadata["owner"], e.Headers.ContentDisposition)
	return nil
}
```
<!-- /ds:block hash=2bbfa8 -->

A SigV4 GET link that forces a download.

<!-- ds:block id=signeddownload-tqzmbcpn -->
**signedDownload** · [`examples/adapters-s3/main.go:190-205`](main.go#L190-L205)

```go
func signedDownload(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	req, err := disk.SignedURL(ctx, "reports/q3.csv", filesystem.PresignOptions{
		Method:             filesystem.MethodGet,
		Expires:            linkLifetime,
		ContentDisposition: "attachment",
	})
	if err != nil {
		return err
	}
	status, body, err := send(ctx, http.MethodGet, req.URL, nil, req.Header)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET: %d %q\n", status, firstLine(body))
	return nil
}
```
<!-- /ds:block hash=03d384 -->

A SigV4 PUT link: the content type is part of the signature, so the client must send exactly the headers it was given.

<!-- ds:block id=signedupload-k55hrh6n -->
**signedUpload** · [`examples/adapters-s3/main.go:210-229`](main.go#L210-L229)

```go
func signedUpload(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	req, err := disk.SignedURL(ctx, "inbox/note.txt", filesystem.PresignOptions{
		Method:      filesystem.MethodPut,
		Expires:     linkLifetime,
		ContentType: "text/plain",
	})
	if err != nil {
		return err
	}
	status, _, err := send(ctx, http.MethodPut, req.URL, strings.NewReader("sent by the browser"), req.Header)
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "inbox/note.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed PUT: %d, stored %d bytes as %s\n", status, e.Size, e.ContentType)
	return nil
}
```
<!-- /ds:block hash=c394cb -->

A multipart upload with a presigned part, the shape a browser uploader such as Uppy uses for large files.

<!-- ds:block id=presignedparts-6m8rgtgq -->
**presignedParts** · [`examples/adapters-s3/main.go:235-258`](main.go#L235-L258)

```go
func presignedParts(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	const key = "videos/clip.bin"
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return err
	}
	part, err := disk.PresignPart(ctx, key, id, 1, linkLifetime)
	if err != nil {
		return err
	}
	if _, _, err := send(ctx, http.MethodPut, part.URL, strings.NewReader("one part, sent directly"), part.Header); err != nil {
		return err
	}
	parts, err := disk.ListParts(ctx, key, id)
	if err != nil {
		return err
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %d part(s), %s is %d bytes\n", len(parts), e.Key, e.Size)
	return nil
}
```
<!-- /ds:block hash=326818 -->

Batch deletes: `DeleteKeys` sends up to 1,000 keys per request, and `DeletePrefix` removes everything under a prefix.

<!-- ds:block id=deletemany-9kakcjsx -->
**deleteMany** · [`examples/adapters-s3/main.go:263-282`](main.go#L263-L282)

```go
func deleteMany(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	keys := []string{"tmp/a.txt", "tmp/b.txt", "tmp/nested/c.txt"}
	for _, k := range keys {
		if _, err := disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	if err := disk.DeleteKeys(ctx, keys[:2]); err != nil {
		return err
	}
	if err := disk.DeletePrefix(ctx, "tmp"); err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "tmp", filesystem.ListOptions{Recursive: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after deletes: %d keys under tmp/\n", len(page.Entries))
	return nil
}
```
<!-- /ds:block hash=00f6d6 -->

What the default configuration refuses, as error codes.

<!-- ds:block id=refusedoptions-5dwdvy3h -->
**refusedOptions** · [`examples/adapters-s3/main.go:289-295`](main.go#L289-L295)

```go
func refusedOptions(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "public.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	fmt.Fprintf(out, "public write: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "once.txt", []byte("x"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "conditional write: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=8f1194 -->

## What the example cannot show

The fake is gofakes3 with two thin wrappers: `fake.go` records request headers and stores lifecycle and replication documents, and `objectfake.go` emulates object tags, Object Lock and archive restores, which gofakes3 does not model (that emulation shows how the driver drives those calls; it is not a model of S3's rules). These features need a real S3 or a richer fake, so they stay in the guide, where each is described from the driver's source and its tests (`adapters/s3/lifecycle_test.go` emulates them in a test proxy, and `task test:s3` runs several against SeaweedFS and RustFS):

- **WORM retention** (`RetainUntil`, `ObjectLockDays`, `COMPLIANCE` mode): the emulation keeps a retention date but enforces none of S3's rules on shortening it.
- **`RestoreVersion`**: gofakes3 ignores the `versionId` of a copy source, so the copy brings back the latest version instead of the one named. `Versions` and `ReadVersion` do work and are shown.
- **The backend verifying a checksum**: gofakes3 stores checksums but never checks them, so the example shows the digest being sent and the Disk's own refusal, not S3's.
- **Encryption itself** (SSE-S3, SSE-KMS, SSE-C): gofakes3 ignores the encryption headers and serves an SSE-C object without its key, so the example shows the headers each request carried, not encrypted storage.
- **IAM enforcing a scoped or assumed role**: the STS stand-in returns fixed credentials and gofakes3 checks no policy, so the example shows the session policy and the signing key, not a refused request outside the prefix.

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the client, Config and DSN disks, metadata, signed links, presigned parts, deletes, refusals |
| `features.go` | The object-store feature steps: storage classes, versions, content-type override, checksums, KMS per tenant, SSE-C, lifecycle, replication, scoped credentials, bring your own bucket |
| `fake.go` | The fake S3: gofakes3 plus a request log and stored lifecycle and replication documents |
| `objects.go` | Object tags, a legal hold, an archive restore, and delegation in GovCloud with a KMS key |
| `objectfake.go` | What gofakes3 lacks for those steps: tags, Object Lock and archive restores, emulated in front of it |
| `main_test.go` | Runs the program against a fresh fake and checks each line the guide quotes |

## Next

- The guide: [Amazon S3](../../docs/adapters/s3.md), and the driver reference [adapters/s3/README.md](../../adapters/s3/README.md).
- [S3-compatible providers](../adapters-s3-compatible): the DSN for R2, B2, Spaces, Wasabi, SeaweedFS and others.
- Other backends: [GCS](../adapters-gcs), [Azure](../adapters-azure), [local](../adapters-local), [memory](../adapters-memory), [SFTP](../adapters-sftp).
