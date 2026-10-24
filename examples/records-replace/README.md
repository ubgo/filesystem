# Replacing content and changing visibility

How a recorded file changes without changing its ID: new content with `Replace` (upload) or `ReplaceObject` (bytes already stored), and a new visibility with `SetVisibility`. Links, attachments and anything else that points at the file ID keep working, as a document editor, a re-uploaded avatar or a "publish" button need. The program runs twice: once on one disk that changes access in place, and once on a public and a private disk with `WithVisibilityDisks`, where the object moves between them. It uses `github.com/ubgo/filesystem/records`, `upload` and `signedurl` on SQLite over in-memory disks. The guide [Replace and visibility](../../docs/records/replace-and-visibility.md) explains each step.

## Run it

```sh
cd examples
go run ./records-replace
go test ./records-replace
```

It needs no network or environment variables. The two SQLite databases live in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-replace
```

```text
Replace: same ID true, name "notes.txt", size 11, old object kept false
ReplaceObject: name "notes-v3.txt"; same key again: invalid_argument
SetVisibility in place: object is public, link https://cdn.example.com/media/docs/v3.txt <nil>
routed upload: disk private, link https://app.example.com/files…
published: disk public, link https://cdn.example.com/public/docs/ygmsi65lutnq74thjrknaii42m.txt, private copy left false
ifunchanged: alice saves: ok
ifunchanged: bob saves the stale version: conflict (true)
ifunchanged: bob reloads and saves: ok
ifunchanged: trash alice's stale view: conflict
```

1. A file attached to a document is given new content with `Replace`: same ID, same display name (an empty filename keeps it), new size, and the old object deleted after the commit.
2. `ReplaceObject` makes an object already on the disk (written by a worker or a presigned upload) the file's content. Passing the current key again is `invalid_argument`.
3. `SetVisibility` on a single disk changes the object's access in place, and the link becomes a public CDN URL.
4. With `WithVisibilityDisks`, an upload with no disk name goes to the private disk by the policy's visibility, and its link is signed.
5. Publishing moves the object to the public disk, the link becomes a CDN URL, and no private copy is left. The random part of the key changes on every run.
6. `unchanged.go`: two editors load the same version. Alice saves with `IfUnchanged`; Bob's save of the stale version is refused with `records.ErrChanged` (`conflict`) and changes nothing; after reloading he saves. Trashing the version Alice saw is refused too, since it was replaced since.

## The code

A memory disk that signs URLs and has public URLs:

<!-- ds:block id=newdisk-p28hp9gs -->
**newDisk** · [`examples/records-replace/main.go:75-81`](main.go#L75-L81)

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

The routed Store, public files on one disk and private ones on the other:

<!-- ds:block id=newroutedstore-n9kscxvq -->
**newRoutedStore** · [`examples/records-replace/main.go:101-111`](main.go#L101-L111)

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

New content by upload; the new object takes the file's visibility whatever the policy says:

<!-- ds:block id=replacebody-4j3zgybf -->
**replaceBody** · [`examples/records-replace/main.go:118-120`](main.go#L118-L120)

```go
func replaceBody(ctx context.Context, s *records.Store, id, text string) (records.File, error) {
	return s.Replace(ctx, tenant, id, strings.NewReader(text), "", textPolicy)
}
```
<!-- /ds:block hash=3c4dee -->

New content from an object already stored:

<!-- ds:block id=replacewithobject-w53xbxkm -->
**replaceWithObject** · [`examples/records-replace/main.go:126-135`](main.go#L126-L135)

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

Publishing, and the link that changes from signed to public:

<!-- ds:block id=publish-4e2p57p2 -->
**publish** · [`examples/records-replace/main.go:140-149`](main.go#L140-L149)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the in-place run and the routed run |
| `unchanged.go` | Saving and trashing only the version that was read (`IfUnchanged`) |
| `main_test.go` | Runs the same steps, plus a replacement that stays private under a public policy, replacing a trashed file, and setting the same visibility twice |

## Next

- The guide: [Replace and visibility](../../docs/records/replace-and-visibility.md).
- Related examples: [records-versions](../records-versions) to keep the replaced content, [records-variants](../records-variants) for image variants that follow a replacement, [recipe-avatars](../recipe-avatars).
