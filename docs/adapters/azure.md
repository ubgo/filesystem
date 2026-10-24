# Azure Blob Storage

The Azure driver stores files as block blobs in an Azure Storage container, or in Azurite, Azure's local emulator. It offers server-side copy, SAS signed URLs, conditional writes, stored metadata and headers, and server-side multipart uploads built on Azure's staged blocks. Visibility is a container setting in Azure, so there is no per-object public or private.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/azure`, a module of its own built on `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`. Its reference is [adapters/azure/README.md](../../adapters/azure/README.md).
- The runnable program is [`examples/adapters-azure`](../../examples/adapters-azure/). There is no in-process Azure fake, so it needs Azurite or a real account: `task azure:up` starts Azurite, then run it with the connection string in step 10. Its test skips unless `FILESYSTEM_AZURE_TEST_CONNECTION` is set, like the driver's own tests. The output quoted below is from a run against Azurite.
- You need a storage account and a container. The driver never creates containers; the example creates its own through the native client.

## 1. Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/azure
```

## 2. Open a disk from a DSN

The DSN names only the container: `azblob://<container>`. The account name and key come from a connection string in an environment variable, so the DSN never carries a secret.

<!-- ds:block id=openwithdsn-bq3tj2xf -->
**openWithDSN** · [`examples/adapters-azure/main.go:86-92`](../../examples/adapters-azure/main.go#L86-L92)

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

```sh
export AZURE_STORAGE_CONNECTION_STRING='DefaultEndpointsProtocol=https;AccountName=myaccount;AccountKey=…;EndpointSuffix=core.windows.net'
```

The parameters, read from `adapters/azure/opener.go`:

| Parameter | Value | Default | Sets |
|---|---|---|---|
| `connection_env` | name of an environment variable | `AZURE_STORAGE_CONNECTION_STRING` | where the connection string is read from, so two disks can use two accounts (`azblob://media?connection_env=MEDIA_AZURE`) |
| `encryption_scope` | an encryption scope name | the container's default | `EncryptionScope`: the scope this disk's blobs are encrypted with (also accepted by `TokenOpener`) |
| `block_size` | bytes, in [1 MiB, 4000 MiB] | 8 MiB | `BlockSize`: the size of each staged block, which caps one blob at 50,000 of them; see [large blobs](#large-blobs-block-size-and-concurrency) (also accepted by `TokenOpener`) |
| `concurrency` | a positive number | 5 | `Concurrency`: how many blocks one write stages at once (also accepted by `TokenOpener`) |

Plus the Mux's own `prefix`, `public_url`, `visibility` and `readonly` (see [configuration](../concepts/configuration.md)).

The fields the opener reads from the connection string:

| Field | Required | Use |
|---|---|---|
| `AccountName` | yes | the account, and the shared-key credential |
| `AccountKey` | yes | the shared-key credential, which also signs SAS URLs |
| `BlobEndpoint` | no | the blob endpoint; when present it wins (Azurite: `http://127.0.0.1:10000/devstoreaccount1`) |
| `DefaultEndpointsProtocol` | no | `https` by default; with `EndpointSuffix` builds `<protocol>://<AccountName>.blob.<suffix>` |
| `EndpointSuffix` | no | `core.windows.net` by default (sovereign clouds use their own suffix) |
| `SharedAccessSignature` | instead of `AccountKey` | a SAS token the application was given instead of the account key: every request carries it, and the disk declares no `Presign` (there is no key to sign with) |

A SAS connection string is the least-privilege setup: the application holds a token limited to the permissions, services and lifetime the account owner chose, never the key to the whole account.

```sh
export AZURE_STORAGE_CONNECTION_STRING='BlobEndpoint=https://myaccount.blob.core.windows.net/;SharedAccessSignature=sv=2022-11-02&ss=b&srt=co&sp=rwdlc&se=2027-01-01T00:00:00Z&sig=…'
```

The disk stops working when the token expires: issue a new one and reopen the disk. Validation, all `CodeInvalidArgument` at open time: no container (`azblob://`), an unknown parameter, and a connection string with neither `AccountName` and `AccountKey` nor a `SharedAccessSignature` (with `BlobEndpoint` or `AccountName`), which includes an unset variable. The error names the variable, never its value. Opening sends no request; run `disk.Check(ctx)` at start-up to prove the account and container work.

## 3. Open a disk in code

Use `New` when the client is not built from an account key: Microsoft Entra ID through `azidentity`, a managed identity, or a SAS-token connection string. `Opener` only knows shared keys; for Entra ID from a DSN, see [`TokenOpener`](#entra-id-and-user-delegation-sas) below.

<!-- ds:block id=openwithconfig-rqy7ht78 -->
**openWithConfig** · [`examples/adapters-azure/main.go:98-112`](../../examples/adapters-azure/main.go#L98-L112)

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

| Field | Default | What it does |
|---|---|---|
| `Client` | required | The account-level `*azblob.Client`. |
| `Container` | required | The container name. |
| `SharedKey` | nil | The `*azblob.SharedKeyCredential` that signs SAS URLs. |
| `UserDelegation` | false | Sign user delegation SAS URLs instead, for a client authenticated with Entra ID. Exclusive with `SharedKey` (`CodeInvalidArgument`). With neither, there is no `Presign` capability: the disk works, and `SignedURL` is `CodeUnsupported`. |
| `AccountName` | empty | The account a user delegation SAS is signed for, when the endpoint is a custom domain, Front Door or a private-endpoint name; needs `Credential` too. See [behind a custom domain](#behind-a-custom-domain-or-front-door). |
| `Credential`, `DelegationClientOptions` | nil | The `azcore.TokenCredential` the client was built with, and its options; used only with `AccountName`, to fetch the key. |
| `EncryptionScope` | empty | The encryption scope this disk's blobs are encrypted with; see [encryption](#encryption-scopes). Server-side `Copy` is then not declared. |
| `EncryptionScopeFor` | nil | `func(key string) string` picking the encryption scope per blob (per tenant prefix) on a disk shared by many tenants; `""` falls back to `EncryptionScope`. Like it, server-side `Copy` and `VersionRead` are then not declared. |
| `Clock` | `time.Now` | The time the driver dates from: SAS expiries and `SignedRequest.ExpiresAt`, user delegation key validity and `RetainFor` policies. Retry-After waits stay on the wall clock. |
| `CopyPollInterval` | `DefaultCopyPollInterval` (50 ms) | How often `Copy` checks a pending server-side copy; raise it to spend fewer requests on long copies. |
| `BlockSize` | `DefaultBlockSize` (8 MiB) | The size of each block `Write` stages, in [`MinBlockSize`, `MaxBlockSize`] (1 MiB to 4000 MiB); see [large blobs](#large-blobs-block-size-and-concurrency). |
| `Concurrency` | `DefaultConcurrency` (5) | How many blocks one `Write` stages at once. |

### Entra ID and user delegation SAS

A client authenticated with Entra ID has no account key, but it can still sign: Azure issues it a user delegation key, and a SAS signed with that key carries the identity's object and tenant IDs (`skoid`, `sktid`). Set `UserDelegation: true` with a client built by `azblob.NewClient(url, cred, nil)`, where `cred` is any `azcore.TokenCredential` (`azidentity.NewDefaultAzureCredential`, a managed identity, a workload identity).

From a DSN, bind the credential to an opener. The credential is code, so it is not named in the DSN, and this module does not depend on `azidentity`:

```go
cred, err := azidentity.NewDefaultAzureCredential(nil)
if err != nil {
	return err
}
mux.Register(azure.Scheme, azure.TokenOpener(cred))
disk, err := mux.Open(ctx, "media", "azblob://media?account=mystorageacct")
```

| Parameter | Value | Meaning |
|---|---|---|
| `account` | storage account name | endpoint `https://<account>.blob.core.windows.net/` |
| `endpoint` | URL | the blob endpoint for a sovereign cloud, `https://<account>.blob.<suffix>`; alone, any other host (a custom domain, an IP, a port, a path) is refused, because the account name in a user delegation SAS comes from the host |

One of the two is required. Both together name an endpoint behind a front end and the account it serves; see [behind a custom domain](#behind-a-custom-domain-or-front-door). A missing container, a nil credential, a non-https endpoint or an unknown parameter is `CodeInvalidArgument`. Disks from `TokenOpener` always sign with user delegation.

How the key is managed:

- The driver requests a key on the first `SignedURL`, valid for `MaxUserDelegationKeyLifetime` less five minutes (Azure grants at most seven days; the margin covers a clock running ahead) and starting five minutes back to absorb a clock running behind. It reuses that key while it lasts at least until the URL would expire, less an hour, and a URL never outlives its key: a link asking for close to seven days is clamped to the key's expiry, up to about an hour earlier than asked, and `SignedRequest.ExpiresAt` reports when it really ends. A process signing a link per request makes about one key request a week, not one per link. Concurrent signers wait for a single request.
- A failed request (most often a missing role) is `CodePermission` and is not cached, so granting the role fixes the next call without a restart.
- The identity needs the `Microsoft.Storage/storageAccounts/blobServices/generateUserDelegationKey` action, which Storage Blob Delegator and every Storage Blob Data role include, plus a data role that allows what the URL allows (Storage Blob Data Reader for GET links, Contributor for PUT links). Azure checks the data role when the URL is used, not when it is signed.
- The SAS cannot outlive the key, and revoking the identity's user delegation keys (`az storage account revoke-delegation-keys`) invalidates every URL signed with them.
- The account name in the signature is the first label of the endpoint's host, so the endpoint must be `<account>.blob.<suffix>` unless `AccountName` names the account (next section). Azurite's path-style endpoint does not fit, and Azurite does not issue user delegation keys there, so this path is tested against a fake of the key request; confirm it once against a real account.

Check: with `SharedKey`, `disk.Capabilities()` prints `[copy,presign,multipart,sweep-abandoned,find-abandoned,metadata,content-type,headers,presign-ip,conditional-write,append,tags,hold,storage-class,restore,compare-and-swap]`.

#### Behind a custom domain or Front Door

Azure checks the account named in a user delegation SAS, and the SDK takes that name from the first label of the endpoint's host: `files.example.com` would sign for an account called `files`, and every URL would be refused. When the client reaches the account through a custom domain, Azure Front Door or a private-endpoint DNS name, name the account and pass the credential again (the driver cannot read it back from the client):

```go
opts := &azblob.ClientOptions{} // the options the client is built with
client, err := azblob.NewClient("https://files.example.com/", cred, opts)
if err != nil {
	return err
}
drv, err := azure.New(azure.Config{
	Client: client, Container: "media", UserDelegation: true,
	AccountName: "mystorageacct", Credential: cred, DelegationClientOptions: opts,
})
```

From a DSN: `azblob://media?account=mystorageacct&endpoint=https%3A%2F%2Ffiles.example.com` with `TokenOpener`.

- The key request still goes to `files.example.com`, exactly as the client's own requests do; only the account the key is bound to changes. The driver does this with a second client whose URL host is `mystorageacct.invalid`, rewritten to the real host before each request, because azblob v1.8.2 has no public way to bind a key to another account. `.invalid` is a reserved name that never resolves, so a missed rewrite would fail rather than send the request elsewhere.
- Signed URLs keep the configured host, so they work only if the front end forwards the request (path and query) to the account unchanged. Check one GET and one PUT link through it before relying on this; Azurite cannot show it, and the tests prove the signature against the SDK's own signing for that account.
- `AccountName` must be a valid account name (3 to 24 lower-case letters and digits) and needs `UserDelegation` and `Credential`; `Credential` or `DelegationClientOptions` without it is `CodeInvalidArgument` rather than silently unused.

Try it: [`examples/adapters-azure`](../../examples/adapters-azure) runs this section in `frontdoor.go` ([`frontDoorDisk`](ds:block?id=frontdoordisk-r2h6p9wd)): `go run ./adapters-azure` from `examples/`, with no account needed for this step, then look for `front door: signature differs from the host-label account's: true`.

### Encryption scopes

Azure encrypts every blob at rest, with the container's default encryption scope (the account's key unless the container names another). A disk can use its own scope, a named key on the account (Microsoft-managed or a Key Vault key you manage):

```text
azblob://media?encryption_scope=tenant-a
```

- The scope is sent on every request that creates blob data: `Write`, each staged part, the commit that completes a multipart upload, and the presigned PUT, whose SAS carries it (`ses`), so a direct upload is encrypted with it too.
- The SDK's server-side copy cannot name a scope, so a disk with one does not declare `Copy`: the Disk copies by reading and writing through `Write`, which applies the scope (one more transfer through your application, never a blob in the wrong scope).
- Create the scope on the account first (`az storage account encryption-scope create`). A container created with `--prevent-encryption-scope-override` refuses any scope but its default.
- A scope name is not a secret, so it is safe in a DSN. Customer-provided keys (CPK) are the other option, `Config.CustomerKey`; the two are exclusive.
- Azurite accepts the header without encrypting anything; the tests check that every write path sends it and that the SAS carries it, and real encryption needs an account.

One disk can also serve many tenants that each have their own scope (a customer-managed Key Vault key per tenant). `Config.EncryptionScopeFor` gets the key as the driver sees it, under a scoped disk's prefix, and returns that tenant's scope, or `""` for `EncryptionScope`:

```go
drv, err := azure.New(azure.Config{
	Client: client, Container: "shared", SharedKey: cred,
	EncryptionScopeFor: func(key string) string {
		tenant, _, _ := strings.Cut(strings.TrimPrefix(key, "tenants/"), "/")
		return tenantScopes[tenant] // "" outside every tenant
	},
})
```

It runs on every write, staged block, commit, append and presigned PUT, so decide by prefix and keep it free of I/O. Reads need no scope.

### Per-call options

When the choice depends on the request rather than on the key (one upload goes straight to Archive, one tenant's scope comes from a database row, a legal hold for one file), attach `azure.RequestOptions` to the context for that key. They override the disk's `Config` for the calls on that exact key only:

```go
ctx, err := azure.WithRequestOptions(ctx, disk, "contracts/2026/acme.pdf", azure.RequestOptions{
	EncryptionScope: tenant.Scope,                        // this tenant's customer-managed key
	AccessTier:      blob.AccessTierCool,
	Tags:            map[string]string{"tenant": tenant.ID}, // stored with the blob, in the same request
	LegalHold:       true,                                   // set right after it is stored
})
if err != nil {
	return err // an invalid key
}
_, err = disk.Write(ctx, "contracts/2026/acme.pdf", body, filesystem.WriteOptions{})
```

| Field | Overrides | Honoured by |
|---|---|---|
| `EncryptionScope` | `EncryptionScopeFor`, `EncryptionScope` | `Write`, multipart, `Append`, presigned PUT (`ses`); a server-side `Copy` or `RestoreVersion` refuses it |
| `AccessTier` | `AccessTier` | `Write`, multipart, `Copy`, `RestoreVersion` |
| `Tags` | none | `Write`, multipart, `Copy`, `RestoreVersion`, and `Append` when it creates the blob |
| `LegalHold`, `RetainUntil` | `RetainFor`'s expiry | `Write`, multipart, `Copy`, `RestoreVersion`, as a second request after the blob is stored |
| `RehydrateTier`, `RehydratePriority` | the same `Config` fields | `Restore`, `SetStorageClass` |

- The options bind to the key the driver sees (`disk.DriverKey`: cleaned, under a scoped disk's prefix), so a context reused for a thumbnail or a staging file never inherits them.
- A multipart upload takes them from `CreateMultipart`'s context and records them in its marker, so every part is staged with the scope the completion commits with, whatever context those calls get.
- A field a call cannot honour (a tier on `Append`, tags on a presigned PUT, a scope on a server-side copy) is `CodeUnsupported`, and an unknown tier or priority is `CodeInvalidArgument`, both before any request; nothing is dropped silently. `EncryptionScope` on a disk with `CustomerKey` is `CodeInvalidArgument`.

Try it: [`examples/adapters-azure`](../../examples/adapters-azure) runs this section in `frontdoor.go` ([`writeContract`](ds:block?id=writecontract-k9v4n2xs)): `go run ./adapters-azure` from `examples/`, with no account needed for this step.

### Large blobs: block size and concurrency

`Write` streams the body as staged blocks and commits the block list at the end. A block blob holds at most `azure.MaxBlocks` (50,000) blocks, so the block size caps how large one `Write` can be:

| `BlockSize` | Largest blob | Memory per `Write` at `Concurrency` 5 |
|---|---|---|
| 1 MiB (`MinBlockSize`, the SDK's own default) | about 48.8 GiB | 5 MiB |
| 8 MiB (`DefaultBlockSize`) | about 390.6 GiB | 40 MiB |
| 100 MiB | about 4.8 TiB | 500 MiB |
| 4000 MiB (`MaxBlockSize`) | about 190.7 TiB | about 19.5 GiB |

A `Write` holds up to `Concurrency` blocks in memory at once, so the two fields together bound an upload's memory. `DefaultConcurrency` is a fixed 5 rather than the SDK's default, which grows with the host's core count (8 to 96), so the same disk uses the same memory on a laptop and on a large server. A body smaller than one block is read whole and sent as a single Put Blob; one of exactly one block, or larger, is staged and committed.

For a disk that stores backups or video archives, raise the block size, and lower the concurrency to keep memory in check:

```go
drv, err := azure.New(azure.Config{
	Client:      client,
	Container:   "backups",
	SharedKey:   cred,
	BlockSize:   100 << 20, // 100 MiB blocks: blobs up to about 4.8 TiB
	Concurrency: 2,         // at most 200 MiB in flight per write
})
```

From a DSN: `azblob://backups?block_size=104857600&concurrency=2`. A block size outside [1 MiB, 4000 MiB] or a negative concurrency is `CodeInvalidArgument` at open time; zero means the default. Multipart uploads (`UploadPart`) are not affected: each part is one staged block of whatever size the caller sends.

Try it: [`examples/adapters-azure`](../../examples/adapters-azure) runs this section in `blocks.go` ([`largeBlobs`](ds:block?id=largeblobs-n4w8c3tp)): `go run ./adapters-azure` from `examples/`; this part needs no Azurite.

## 4. Visibility is a container setting

Azure has no per-object ACL: anonymous read access is set on the container (`private`, `blob` or `container` access level), and storage accounts can disallow it altogether. The driver therefore does not declare `SetVisibility`. Writes use the disk's default visibility, and a write asking for another one is `CodeUnsupported`.

To serve public files, make one container publicly readable (or put Azure Front Door or a CDN in front of it) and open it with `visibility=public&public_url=https://<account>.blob.core.windows.net/<container>`; keep private files in another container. See [public and private files](../concepts/public-and-private.md).

<!-- ds:block id=refusedoptions-wwr4ncm8 -->
**refusedOptions** · [`examples/adapters-azure/main.go:205-211`](../../examples/adapters-azure/main.go#L205-L211)

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

Check: `public write: unsupported` and `presigned part: unsupported`.

## 5. Conditional writes, metadata and headers

`IfNotExists` sends `If-None-Match: *` on the commit, and Azure refuses it when the blob exists (`CodeExists`). Metadata, the content type and the three stored headers are written with the blob:

<!-- ds:block id=writeonce-xwgv3s3k -->
**writeOnce** · [`examples/adapters-azure/main.go:132-150`](../../examples/adapters-azure/main.go#L132-L150)

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

Check: `second conditional write: exists` and `stat: 4 bytes, order=1042, cache-control="private, max-age=0"`.

- Azure may change the case of metadata names, so the driver returns them lower-cased. Use lower-case names to get back exactly what you wrote.
- Azure's documentation requires metadata names to be valid C# identifiers: letters, digits and underscores, not starting with a digit. A name with a hyphen is rejected by Azure.
- A blob stored with `Content-Encoding: gzip` reads back as the stored bytes: the driver sends `Accept-Encoding: identity` so Go's HTTP transport does not decompress it.
- `List` entries include metadata (requested from Azure) as well as size, time, content type and ETag.

## 6. SAS signed URLs

`SignedURL` issues a SAS for one blob: a service SAS signed with the shared key, or a user delegation SAS (see [Entra ID](#entra-id-and-user-delegation-sas)), both computed locally once a key is at hand. The lifetime is required and at most 7 days.

<!-- ds:block id=saslinks-bfgr6paa -->
**sasLinks** · [`examples/adapters-azure/main.go:155-174`](../../examples/adapters-azure/main.go#L155-L174)

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

Check: `signed PUT requires X-Ms-Blob-Type: BlockBlob` and `signed GET: "scanned"`.

- A GET grants read permission, and can override the response's content type and disposition (both are in the signature). For active content the Disk forces `attachment` when you pass the content type.
- A PUT grants create and write on that one blob. The client must send `x-ms-blob-type: BlockBlob`, which is in `SignedRequest.Header`; Azure refuses a PUT without it.
- Unlike S3 and GCS, the content type is not part of a PUT's signature, and cannot be: Azure SAS signs no request headers, with a shared key or a user delegation key alike. `SignedRequest.Header` carries it, and the client must send exactly that `Content-Type`. Azure would store whatever type the client sends, and `Stat` reports the stored type, so `upload.Confirm` compares it with the type sniffed from the bytes and refuses (and deletes) an upload whose stored type differs. Always run `upload.Confirm` after a presigned upload.
- For the same reason a signed PUT cannot pin the headers stored with the blob (Cache-Control, Content-Disposition, Content-Encoding): the driver does not declare `PresignHeaders`, and a `SignedURL` PUT with `PresignOptions.Headers` is refused with `CodeUnsupported` rather than issued without them. `upload.Presign` with a policy that sets headers is refused the same way; upload through the server (`upload.Receive`, the form or Uppy handlers) to store them.
- SAS URLs require HTTPS, except for an `http://` endpoint (Azurite), where the driver allows both.
- Browser uploads from another origin need CORS rules on the storage account (blob service), outside this library.

### No presigned POST

Azure Blob Storage has no form upload with a server-enforced policy, so the driver does not declare `PresignPost` and `disk.PresignPost` is `CodeUnsupported`. Use a signed PUT with `upload.Presign` and `upload.Confirm`, which checks size and type after the upload.

## 7. Multipart uploads

Multipart maps onto Azure's blocks: each part is a staged block, and completion commits the block list. Parts go through your server (`UploadPart`); there are no presigned parts, because Azure's Put Block returns no ETag, which browser uploaders such as Uppy's S3 plugin depend on.

<!-- ds:block id=servermultipart-ayhqr3w8 -->
**serverMultipart** · [`examples/adapters-azure/main.go:180-200`](../../examples/adapters-azure/main.go#L180-L200)

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

Check: `multipart: exports/big.csv is 14 bytes, text/csv`.

- `CreateMultipart` records the key and the write options (content type, metadata, headers, `IfNotExists`) in a small marker blob under the reserved prefix `.filesystem-multipart/`. Keys under that prefix are refused with `CodeInvalidKey`, and listings skip it.
- Parts have no minimum size. A body that cannot seek is read into memory first, because Azure needs a seekable body to retry a block.
- A part's `ETag` is its block ID. `CompleteMultipart` needs the parts in ascending order with the ETags `UploadPart` or `ListParts` returned; anything else is `CodeInvalidArgument`.
- Conditional multipart works: `IfNotExists` recorded at creation is applied when the block list is committed.
- `AbortMultipart` deletes the marker. Staged blocks cannot be deleted individually; Azure discards uncommitted blocks after seven days.
- Uploads nobody completes or aborts leave their marker and staged blocks behind. `disk.SweepAbandoned(ctx, time.Now().Add(-24*time.Hour))` removes the uploads started before the cutoff and returns how many; those uploads are then `CodeNotFound`. Azure has no call to delete an uncommitted block, but a commit discards every block not listed, so when the target blob does not exist and holds only that upload's blocks, the sweep commits an empty list (refused if the blob appeared meanwhile) tagged with the upload's ID, then deletes that empty blob. For the moment between the two, an empty blob is visible at the key. If the delete fails, the sweep returns the error and keeps the upload's marker, and the next sweep finds the tagged empty blob (no blocks, no bytes, the upload's tag) and deletes it; a user's own empty blob carries no tag and is never taken for one. The delete runs even if the sweep's context has just been cancelled, bounded by 30 seconds, so a cancelled sweep does not leave the blob behind. Where the blob already exists, or another upload or a running `Write` has blocks on it, the blocks are left to Azure's seven-day expiry, since a commit would rewrite the stored blob or destroy the other writer's blocks. A marker records only when its upload started, not its last part, so choose a cutoff beyond the longest upload you allow. Stored blobs and blobs under the prefix that are not markers are never touched. The library starts no goroutine: schedule the sweep yourself (the disk declares `SweepAbandoned`). `disk.FindAbandoned(ctx, cutoff)` is its dry run: the same marker listing, returning each upload's ID, start time and key (read from its marker; empty when the marker vanished or does not decode), and changing nothing.

`Write` itself also stages blocks and commits at the end (`UploadStream`), so a body that fails half way never produces a blob.

### Checksums

Three mechanisms check Azure blobs, each for a different question.

| Mechanism | Algorithm | Who checks | When to use it |
|---|---|---|---|
| `WriteOptions.SHA256` | SHA-256 of the whole body | The Disk, while streaming, on every backend; a mismatch is `CodeInvalidArgument` before the block list is committed | The client or an upstream step declared a digest: a direct upload's confirm, an import manifest, a copy from another backend. |
| `Config.ValidateCRC64` | CRC64 of each staged block | Azure, before accepting the block | Bytes could be corrupted between this process and Azure in a way TLS would not catch (a faulty proxy or NIC offload). Off by default: it costs a pass over every block. |
| `Driver.ObjectInfo` | Reads the blob's stored Content-MD5 | Your code | A blob uploaded by a tool that stores an MD5 (single-shot uploads, `azcopy --put-md5`) is compared against a checksum you hold without downloading it. |

Azure does not verify SHA-256 itself, so the driver does not declare `ChecksumSHA256`. The Disk still hashes every body and refuses a mismatch before anything is committed:

<!-- ds:block id=checksums-bu9zpz2k -->
**checksums** · [`examples/adapters-azure/main.go:219-233`](../../examples/adapters-azure/main.go#L219-L233)

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

Check, against Azurite: `checksums: matching digest stored, wrong digest invalid_argument, stored=false` ([examples/adapters-azure](../../examples/adapters-azure)).

Turn on per-block CRC64 in the driver's Config:

```go
drv, err := azure.New(azure.Config{Client: client, Container: "media", SharedKey: cred, ValidateCRC64: true})
```

- Azure keeps no whole-blob CRC64: the per-block values are checked on arrival and then dropped, so `ObjectInfo` cannot return one.
- This driver writes through staged blocks and a block-list commit (`UploadStream`), which sends no whole-blob MD5, so `ObjectInfo.MD5` is empty for blobs the driver wrote. Keep the Disk's `Entry.SHA256` (a records row stores it as `sha256`) when you need a digest to compare later.
- `ObjectInfo` is a driver method, past the Disk: pass `disk.DriverKey(key)` when the disk is scoped.
- `WriteOptions.SHA256` is refused on multipart uploads (`CodeUnsupported`); check each part's size and the completed object's digest yourself, or let the client send the digest to `upload.Confirm`.

### Tags, appends, holds, tiers and versions

Blob index tags are `Disk.Tags` and `Disk.SetTags` (ten per blob, checked by Azure). `Disk.Append` writes append blobs: each call is one atomic Append Block of at most `azure.MaxAppendBlock` (4 MiB), read whole first, so a failing body appends nothing and concurrent appends never interleave; `AppendOptions.MaxSize` is Azure's own max-size condition. A key written with `Write` is a block blob and refuses appends (`CodeConflict`). Compare-and-swap (`WriteOptions.IfMatch`) conditions the commit on the ETag.

```go
drv, err := azure.New(azure.Config{Client: client, Container: "records", AccessTier: blob.AccessTierCool, Versioned: true})
disk, err := filesystem.NewDisk("records", drv)
_, err = disk.Append(ctx, "audit/2026-10-07.log", strings.NewReader(line), filesystem.AppendOptions{MaxSize: 100 << 20})
err = disk.SetHold(ctx, "contracts/acme.pdf", filesystem.Hold{Legal: true})
err = disk.SetStorageClass(ctx, "scans/2019.tar", string(blob.AccessTierArchive)) // the access tier
err = disk.Restore(ctx, "scans/2019.tar", filesystem.RestoreOptions{Days: 1}) // rehydrate to Hot
_, err = disk.ReadAll(ctx, "scans/2019.tar")                                   // errors.Is(err, azure.ErrArchived) until done
info, err := drv.ObjectInfo(ctx, "scans/2019.tar")                            // ArchiveStatus, MD5, VersionID
```

`Restore` moves an archived blob to `Config.RehydrateTier` (Hot) for good, since Azure has no restore-for-N-days; `RestoreOptions.Days` is required by the Disk but not sent. Legal holds and immutability policies need a container with version-level immutability support; `Config.RetainFor` gives every blob the disk creates (a write, a completed multipart upload, a copy, a restored version) an immutability policy, set right after the blob is stored, in a second request: if the process crashes or the request fails between the two, the blob is stored but unprotected (a failed policy request returns an error saying the blob was stored, with the cause's code). A disk with `RetainFor` does not declare `Presign`, since a SAS upload cannot pin the policy, nor `Append`, since a blob under a policy refuses further appends; keep logs that must stay appendable on a disk without it. An application that must not leave such a window re-applies the policy itself, for example after an error or in a periodic check, with `disk.SetHold(ctx, key, filesystem.Hold{RetainUntil: until})`, which sets the policy when the blob's current expiry differs; a container-level immutability policy closes the window entirely. A protected blob refuses deletion with `CodePermission`. `Config.CustomerKey` (`azure.NewCustomerKey`, 32 bytes, HTTPS only) encrypts every blob with a customer-provided key that never prints; SAS URLs and server-side copies are switched off with it. `SweepAbandoned` on a disk with a customer-provided key is unverified against real Azure: the sweep reads properties of a leftover blob without presenting the key, which Azure may refuse for a blob encrypted with one, so check it against a real account before relying on it. Azurite runs the tag, append and compare-and-swap cases of the conformance suite; customer keys, holds, immutability, versions and rehydration are not implemented by Azurite, so their tests check the requests the driver sends against a scripted service, and their effect needs a real account.

## 8. Listing, deleting, copying

- `List` uses Azure's flat listing for recursive calls and its hierarchy listing with `/` for directory views. The cursor is Azure's marker. Keys ending in `/` and the multipart marker prefix are not listed.
- `Delete` of a missing blob succeeds.
- `Copy` starts a server-side copy inside the account and waits for it to finish, polling every `Config.CopyPollInterval` (50 ms by default); Azure copies are asynchronous. A copy that ends in `failed` or `aborted` is `CodeUnknown` with Azure's description; cancelling the context while waiting is `CodeUnavailable`.
- `Move` is Copy then Delete, not atomic. `DeletePrefix` and `DeleteKeys` are Disk fallbacks, one request per blob.

## 9. Errors

| Azure error | Code |
|---|---|
| `BlobNotFound`, `ContainerNotFound`, 404 | `CodeNotFound` |
| `BlobImmutableDueToPolicy`, and any other refusal whose code names an immutability policy or legal hold, whatever the operation | `CodePermission` |
| `BlobArchived`, `BlobBeingRehydrated` | `CodeConflict`, matching `azure.ErrArchived` |
| `InvalidBlobType` (appending to a block blob) | `CodeConflict` |
| `ConditionNotMet`, `BlobNotFound`, `InvalidBlockList` on a write with `IfMatch` | `CodeConflict` |
| `ConditionNotMet`, `BlobAlreadyExists`, 409, 412 | `CodeExists` |
| `InvalidBlockList`, `InvalidBlockID` | `CodeInvalidArgument` |
| 401, 403 | `CodePermission` |
| 429, 5xx | `CodeUnavailable`, carrying the response's `Retry-After` when it sent one, so `layer.Retry` waits at least that long and `fshttp` passes it on |
| anything else | `CodeUnknown` |

A connection failure (no response at all) is never mistaken for a service answer. A ranged read past the end of a blob is an empty read. The SDK's `*azcore.ResponseError` stays reachable with `errors.As`.

## 10. The native client, and testing locally

`filesystem.As[*azblob.Client](disk)` returns the account client for container management, access tiers, leases or anything else. The example uses it to create its container:

<!-- ds:block id=ensurecontainer-4darszyg -->
**ensureContainer** · [`examples/adapters-azure/main.go:117-127`](../../examples/adapters-azure/main.go#L117-L127)

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

`task test:azure` starts a throwaway Azurite from the repository's `compose.yml` on `127.0.0.1:10000`, runs the driver's tests and the conformance suite against it, and removes it afterwards, even when a test fails:

```sh
task test:azure
```

Azurite's development account is public and not a secret. To run the example against it:

```sh
task azure:up
cd examples
export AZURE_STORAGE_CONNECTION_STRING='DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;'
go run ./adapters-azure
```

```text
capabilities: [copy,presign,multipart,sweep-abandoned,find-abandoned,metadata,content-type,headers,presign-ip,conditional-write,append,tags,hold,storage-class,restore,compare-and-swap]
second conditional write: exists
stat: 4 bytes, order=1042, cache-control="private, max-age=0"
signed PUT requires X-Ms-Blob-Type: BlockBlob
signed GET: "scanned"
multipart: exports/big.csv is 14 bytes, text/csv
public write: unsupported
presigned part: unsupported
```

The task runs Azurite with `--loose --skipApiVersionCheck`, so a newer SDK than the emulator knows is accepted. Setting `FILESYSTEM_AZURE_TEST_CONNECTION` to a real account's connection string runs the same tests against Azure itself; each conformance case creates and deletes its own container.

## 11. CORS for browser uploads

A browser that uploads or downloads directly with a SAS URL calls the storage account from your site's origin, so the account must allow that origin (CORS). `azure.SetCORS` sets the blob service's rules, and `azure.DirectUploadCORS` is the recommended rule:

```go
client, _ := filesystem.As[*azblob.Client](disk)
err := azure.SetCORS(ctx, client, azure.DirectUploadCORS("https://app.example.com"))
```

- On Azure, CORS is a property of the account's blob service, not of a container: the rules apply to every container in the account, and Azure keeps at most five. `SetCORS` replaces them all and leaves the other service properties as they are.
- The recommended rule allows `GET`, `HEAD` and `PUT` from the origins you name (Azure has no POST upload), the request headers a SAS upload carries (`content-type` and `x-ms-*`, which covers `x-ms-blob-type`), exposes `etag`, and lets browsers cache the preflight for an hour. Name your origins: `*` lets any site use links your users hold.
- It is a setup step: run it from a deploy script with an account key or the Storage Account Contributor role. Checked against Azurite on 2026-10-06: the rule is stored, and a preflight for a SAS upload is allowed from the configured origin and not from another (`TestSetCORS`).

## 12. Scoped SAS and IP-restricted URLs

`DelegatePrefix` signs a SAS a client can use directly, limited to one directory of the container, with its expiry:

```go
s, err := azure.DelegatePrefix(azure.DelegateOptions{
	ContainerURL: "https://acme.blob.core.windows.net/media",
	SharedKey:    cred, // or UserDelegation: a user delegation credential
	Prefix:       "users/42",
	Write:        true,
	Expires:      time.Hour,
})
client, err := container.NewClientWithNoCredential(s.URL(), nil)
```

- With a prefix it is a directory SAS (`sr=d`), which Azure honours only on accounts with a hierarchical namespace (Data Lake Storage Gen2). On a flat-namespace account Azure refuses every request made with it, so it fails closed rather than granting the container; this follows Azure's SAS documentation and is not tested here, since Azurite has no hierarchical namespace. Use `WholeContainer` (or one container per tenant) on flat accounts. A container SAS (`sr=c`) covers every blob and must be asked for with `WholeContainer: true`, so an empty prefix never widens a delegation by accident. Without `Write` it reads and lists only.
- For `UserDelegation`, take the key from the disk's driver rather than from the SDK: `cred, keyExp, err := drv.UserDelegationCredential(ctx, time.Now().Add(time.Hour))` returns the driver's own cached key, bound to `Config.AccountName` when that is set, so a prefix SAS signed behind a custom domain or Front Door names the right account (the SDK's `GetUserDelegationCredential` would name the host's first label). Keep `Expires` within `keyExp`. A disk without `UserDelegation` is `CodeUnsupported`.
- The SAS is signed locally, no request made. `ScopedSAS` prints with the token redacted; `Token` and `URL()` give it where it is needed.
- Checked against Azurite: a container SAS writes and lists in its container and is refused in another (`TestContainerSASAgainstAzurite`). Azurite has no hierarchical namespace, so the directory limit is checked only for what is signed (`sr=d`, `sdd`), not enforced there.
- `PresignOptions.AllowedIP` (a CIDR) becomes the SAS `sip` range, so Azure refuses a signed URL from any other address; the driver declares `PresignIP`. Azure takes IPv4 ranges only: an IPv6 prefix is `CodeUnsupported`. Azurite ignores `sip` (checked 2026-10-07), so the tests check the signed range (`TestPresignAllowedIP`), not its enforcement.
- Lifecycle management and object replication policies are part of Azure's management plane (`armstorage`), not the blob API this adapter uses, so the adapter has no helpers for them; set them with the Azure CLI, Bicep or Terraform.

## Pitfalls

- **Expecting per-object visibility.** It does not exist in Azure; plan two containers.
- **Entra ID without the delegator role.** `TokenOpener` and `UserDelegation` disks declare `Presign`, but `SignedURL` is `CodePermission` until the identity may request a user delegation key.
- **Metadata names with hyphens** are rejected by Azure; use letters, digits and underscores.
- **Abandoned multipart uploads** leave a marker blob under `.filesystem-multipart/` until aborted; their blocks expire on their own after seven days.
- **Writing a blob larger than the block size allows.** At the default 8 MiB blocks one `Write` holds at most about 390.6 GiB; a larger body fails when Azure refuses the block list at the commit, after every block was uploaded. Set `BlockSize` for larger files.
- **Trusting a signed PUT's content type.** Azure does not enforce it; confirm uploads server-side.

## Next

- [Serving files](../concepts/serving-files.md) and [private downloads](../recipes/private-downloads.md).
- [Public and private files](../concepts/public-and-private.md).
- [Comparison of every backend](README.md).
