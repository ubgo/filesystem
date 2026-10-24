# Replace and visibility

A recorded file can change without becoming a new file: a user uploads a corrected contract, a draft is published, a shared photo is made private again. Its ID stays the same, so its attachments, folder, tags, share links and audit history stay attached to it. This page covers new content (`Replace`, `ReplaceObject`) and new visibility (`SetVisibility`), on one disk that changes access in place and on separate public and private disks.

## Before you start

- A Store, as built in [Files](files.md).
- The example is [`examples/records-replace`](../../examples/records-replace): SQLite and memory disks that sign URLs and have a CDN prefix. `go run ./records-replace` in `examples/` prints both demos; `go test ./records-replace` runs them.
- [Public and private files](../concepts/public-and-private.md) explains visibility itself and why many buckets now refuse per-object ACLs.


## Step 1 — Replace a file's content with an upload

`Replace(ctx, scope, id, body, filename, policy)` stores `body` under the policy (size cap, detected type, a new key) and makes it the file's content.

<!-- ds:block id=replacebody-4j3zgybf -->
**replaceBody** · [`examples/records-replace/main.go:118-120`](../../examples/records-replace/main.go#L118-L120)

```go
func replaceBody(ctx context.Context, s *records.Store, id, text string) (records.File, error) {
	return s.Replace(ctx, tenant, id, strings.NewReader(text), "", textPolicy)
}
```
<!-- /ds:block hash=3c4dee -->

- The file keeps its ID, scope, disk, `CreatedAt` and `UploadedBy`. Key, content type, size, SHA-256 and (when given) the display name change.
- `filename` `""` keeps the display name; it is also the name the policy's extension checks see.
- The new object gets the file's visibility, whatever the policy's `Visibility` says, so a private file cannot become public by being replaced.
- The previous object is deleted after the commit, unless the [versions plugin](../plugins/versions.md) keeps it as a version.
- If the replacement fails, or an enclosing `Tx` rolls back, the new object is deleted again.

Check: `Replace: same ID true, name "notes.txt", size 11, old object kept false`.

## Step 2 — Replace with an object that is already stored

`ReplaceObject(ctx, scope, id, records.NewContent{Key, Filename, ContentType, Size, SHA256})` is the object-first form, for bytes that arrived some other way (a worker's output, a presigned upload). The object must exist on the file's own disk and differ from the current key; content type, size and digest default to what the disk reports. A digest you pass is 64 hex digits in either case and is stored lowercase (anything else is `invalid_argument`), and metadata a `BeforeReplace` hook writes is held to the same [text limits](files.md#alt-text-caption-and-title) as a new file's.

<!-- ds:block id=replacewithobject-w53xbxkm -->
**replaceWithObject** · [`examples/records-replace/main.go:126-135`](../../examples/records-replace/main.go#L126-L135)

```go
func replaceWithObject(ctx context.Context, s *records.Store, f records.File, key, text string) (records.File, error) {
	disk, err := s.Disks().Disk(f.Disk)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, bytes.NewReader([]byte(text)), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.ReplaceObject(ctx, tenant, f.ID, records.NewContent{Key: key, Filename: "notes-v3.txt"})
}
```
<!-- /ds:block hash=b91db8 -->

Check: `ReplaceObject: name "notes-v3.txt"; same key again: invalid_argument`.

`ReplaceObject` gives the new object the file's visibility where its disk can set one object's visibility (a private file never ends up over a public object, even on a disk whose default is public), and puts the object's own visibility back if the transaction rolls back. On a disk without that capability every object has the disk's visibility anyway. `Replace` writes the new object with the file's visibility from the start.

## Save only what you read

An editor that loads a document and saves it later can overwrite a save made in between. Pass `records.IfUnchanged(f.SHA256)` with the digest it read: `Replace` and `ReplaceObject` refuse with `records.ErrChanged` (`conflict`) and change nothing when the content has been replaced since. `Trash`, `Restore`, `SetVisibility` and `Purge` take the same option, so hiding or deleting the version a user looked at cannot act on a newer one. The check runs before the body is received, so a stale save costs no upload, and again with the file's row locked, so two concurrent saves cannot both pass it. The other calls check it again under the lock too, after their object change, which their rollback undoes when the check fails.

The example's `unchanged.go` has two editors load the same version and save in turn. Each save passes the digest its editor read:

<!-- ds:block id=save-v4n8c2wq -->
**save** · [`examples/records-replace/unchanged.go:20-22`](../../examples/records-replace/unchanged.go#L20-L22)

```go
func save(ctx context.Context, s *records.Store, loaded records.File, text string) (records.File, error) {
	return s.Replace(ctx, tenant, loaded.ID, strings.NewReader(text), "", textPolicy, records.IfUnchanged(loaded.SHA256))
}
```
<!-- /ds:block hash=a019bc -->

A delete takes the same guard, so a user who deletes "the version I am looking at" never deletes a newer one:

<!-- ds:block id=trashseen-k7t3m9hp -->
**trashSeen** · [`examples/records-replace/unchanged.go:28-30`](../../examples/records-replace/unchanged.go#L28-L30)

```go
func trashSeen(ctx context.Context, s *records.Store, seen records.File) error {
	return s.Trash(ctx, tenant, seen.ID, records.IfUnchanged(seen.SHA256))
}
```
<!-- /ds:block hash=3b1cce -->

Check: the example prints

```text
ifunchanged: alice saves: ok
ifunchanged: bob saves the stale version: conflict (true)
ifunchanged: bob reloads and saves: ok
ifunchanged: trash alice's stale view: conflict
```

Bob's stale save is `records.ErrChanged` (match it with `errors.Is`); he reloads with `File`, merges, and saves with the new digest. Over HTTP, [recordshttp](http-endpoints.md) maps an `If-Match` header to this option and answers `412` for `ErrChanged`.

When to use it rather than [check-out locks](../plugins/locks.md): almost always. It needs no table, no expiry and no administrator override, and the loser learns at save time. Use locks when an editor must reserve a file before a long edit.

Pitfalls: a file recorded without a digest has `SHA256` `""`, and `IfUnchanged("")` matches only such a file, so record digests (the default for `Upload`). A client that keeps the digest across a page reload must store it with the draft, or it saves against whatever is current.

## What a replacement runs

Inside one transaction, in this order:

1. The file is loaded; a trashed file is `not_found`.
2. Every `records.ReplaceHooks` plugin's `BeforeReplace(ctx, s, current, &next)`: the [quota plugin](../plugins/quota.md) charges the size difference (or refuses growth past the limit with `quota_exceeded`), the [dimensions plugin](../plugins/dimensions.md) measures the new image. A hook may change `next`'s metadata, not its ID, scope, disk or key. An error aborts the replacement.
3. The row is updated.
4. Every `records.Versioner` plugin's `KeepVersion(ctx, s, previous)`: returning true takes ownership of the previous object, so the Store does not delete it.
5. `file.replaced` is emitted to every `EventHooks` plugin; the [variants plugin](../plugins/variants.md) drops the variants made from the old content.
6. The previous object is scheduled for deletion after the commit (failures go to `WithObjectErrorHandler`, see [Lifecycle](lifecycle.md)).

## Step 3 — Change visibility on one disk

`SetVisibility(ctx, scope, id, v)` makes a file public or private. On a Store without `WithVisibilityDisks`, the file's own disk changes the object's access, which needs the disk's `SetVisibility` capability (memory, local, S3 with ACLs, GCS with fine-grained access); a disk without it answers `unsupported`. [Capabilities](../concepts/capabilities.md) lists which drivers have it.

<!-- ds:block id=newdisk-p28hp9gs -->
**newDisk** · [`examples/records-replace/main.go:75-81`](../../examples/records-replace/main.go#L75-L81)

```go
func newDisk(name string) (*filesystem.Disk, error) {
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(name, memory.New(), filesystem.WithURLSigner(signer), filesystem.WithPublicURL(cdnBase+"/"+name))
}
```
<!-- /ds:block hash=2907d6 -->

<!-- ds:block id=publish-4e2p57p2 -->
**publish** · [`examples/records-replace/main.go:140-149`](../../examples/records-replace/main.go#L140-L149)

```go
func publish(ctx context.Context, s *records.Store, id string) (string, error) {
	if err := s.SetVisibility(ctx, tenant, id, filesystem.VisibilityPublic); err != nil {
		return "", err
	}
	f, err := s.File(ctx, tenant, id)
	if err != nil {
		return "", err
	}
	return s.URL(ctx, f, linkTTL)
}
```
<!-- /ds:block hash=ed5ec5 -->

Check: `SetVisibility in place: object is public, link https://cdn.example.com/media/docs/v3.txt`: once public, `URL` returns the public URL instead of a signed one.

The object changes before the row, and a failed transaction puts it back, so the row never claims a visibility the object does not have. Setting the current visibility does nothing and emits no event; an unknown value is `invalid_argument`.

## Step 4 — Separate public and private disks

S3 disables object ACLs on new buckets by default, and many teams want public files on a bucket behind a CDN and private files on another bucket nobody can list. `records.WithVisibilityDisks(public, private)` sets that up:

<!-- ds:block id=newroutedstore-n9kscxvq -->
**newRoutedStore** · [`examples/records-replace/main.go:101-111`](../../examples/records-replace/main.go#L101-L111)

```go
func newRoutedStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	pub, err := newDisk(publicDisk)
	if err != nil {
		return nil, err
	}
	priv, err := newDisk(privateDisk)
	if err != nil {
		return nil, err
	}
	return newStore(ctx, db, []*filesystem.Disk{pub, priv}, records.WithVisibilityDisks(publicDisk, privateDisk))
}
```
<!-- /ds:block hash=595f6b -->

- `Upload` with an empty disk name stores the file on the disk of the policy's visibility (private when the policy names none).
- `SetVisibility` copies the object to the other disk under the same key (and sets its visibility there when that disk can), updates the row's disk and visibility, and deletes the old object after the commit. A rollback deletes the copy.
- Both disk names must be in the Store's manager, else `records.New` fails with `invalid_argument`; they must differ.
- `Trash` moves a public file's object to the private disk (the row keeps `public`) and `Restore` moves it back, so a trashed file never answers at its public URL; see [Lifecycle](lifecycle.md).

Check: `routed upload: disk private, link https://app.example.com/files…` (a signed URL), then after publishing `published: disk public, link https://cdn.example.com/public/docs/….txt, private copy left false`.

`Replace` writes the new content to the file's current disk, which is already the right one.

`SetVisibility` on a trashed file changes only its row: the object stays hidden until `Restore` applies the new visibility (moving it, or setting it in place). On a Store without `WithVisibilityDisks` it is still refused with `unsupported` when the disk could not apply it at `Restore`.

## How plugins follow

| Plugin | On `file.replaced` | On `file.visibility_changed` |
|---|---|---|
| [versions](../plugins/versions.md) | keeps the previous content as a version | moves or re-permissions every kept version, so an old version never stays public after the file was made private; it does the same on `file.trashed` (versions made private) and `file.restored` |
| [variants](../plugins/variants.md) | drops the variants; they are made again from the new content | drops them, so no public thumbnail of a private file survives |
| [dimensions](../plugins/dimensions.md) | measures the new content (through `BeforeReplace`) | nothing |
| [quota](../plugins/quota.md) | charges or credits the size difference (through `BeforeReplace`) | nothing |
| [audit](../plugins/audit.md) | logs the event with the actor | logs the event with the actor |

## Step 5 — Rename a disk

Every file row names its disk (`fs_files.disk`), and plugins that keep objects of their own store disk names too. When a disk changes name in your `Manager` (a store moved to S3 and registered as `media-s3`), `RenameDisk(ctx, from, to)` points every record at the new name, in every scope and in one transaction: `fs_files.disk`, trashed files included, then each installed plugin that implements `records.DiskRenamer`. It returns how many file rows changed.

With `WithVisibilityDisks`, the Store routes uploads and visibility changes by disk name, so renaming away from one of those disks while the Store still names it would leave it uploading to the old name: `RenameDisk` refuses that with `invalid_argument`. Build the Store with `WithVisibilityDisks` naming the new disk first (keep the old one registered in the `Manager`), then rename.

It changes rows only and copies no bytes: copy the objects first (the [move to S3 recipe](../recipes/move-to-s3.md)), so they are reachable on `to` under the same keys. `to` must be a disk of the Store's `Manager` (`not_found` otherwise); `from` need not be any more, so a rename after a deploy that already dropped the old name still works. Pause uploads to `from` and keep both names registered until it has committed. It emits no events, because it can touch every row. `from == to` or an empty name is `invalid_argument`, and a plugin's error rolls the whole rename back.

A plugin that stores disk names implements `records.DiskRenamer` (`RenameDisk(ctx, s, from, to) error`) and rewrites its own rows with `s.Conn(ctx)`, inside the same transaction. A plugin that stores disk names without implementing it is left pointing at the old name.

## Events

`Replace` and `ReplaceObject` emit `file.replaced`; `SetVisibility` emits `file.visibility_changed`. Both carry `FileID`. See [Events and hooks](events-and-hooks.md).

## What can go wrong

- `not_found` from `Replace`: the file is in the trash (restore it first) or in another scope. The uploaded object was deleted again.
- `invalid_argument` from `ReplaceObject`: the key is the file's current key.
- `not_found` from `ReplaceObject`: the new object is not on the file's disk.
- `unsupported` from `SetVisibility`: the disk cannot change per-object access (an S3 bucket with ACLs disabled, a GCS bucket with uniform access). Use `WithVisibilityDisks` with two buckets instead.
- `quota_exceeded` from `Replace`: the new content would pass the scope's byte limit.
- `not_found` from `RenameDisk`: `to` is not a disk of the Store's `Manager`; register it before renaming.

## Next

- [Versions](../plugins/versions.md): keep and restore previous contents.
- [Serving files](../concepts/serving-files.md): what `URL` returns for public and private files.
- [Public CDN recipe](../recipes/public-cdn.md): the two-bucket layout end to end.
