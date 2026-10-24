# Attaching files to your own rows

How recorded files hang off the application's own rows: a post's gallery (many files, ordered), a user's avatar (a single role holding one file), reordering, detaching, replacing the file of a role with `SetAttachment`, and one file shared by two owners. This is what a social post with photos, a profile picture or a product's images need. It uses `github.com/ubgo/filesystem/records` on SQLite over an in-memory disk. The guide [Attachments](../../docs/records/attachments.md) explains each step.

## Run it

```sh
cd examples
go run ./records-attachments
go test ./records-attachments
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-attachments
```

```text
gallery: [1:beach.jpg 2:dunes.jpg 3:sunset.jpg] <nil>
reordered: [1:sunset.jpg 2:beach.jpg 3:dunes.jpg] <nil>
partial reorder: invalid_argument
after detach: [1:sunset.jpg 2:beach.jpg] <nil>
unknown owner type: invalid_argument
second Attach to a single role: exists
avatar is the new file: true; previous trashed: true
shared file kept: true, still the post's cover: true
```

1. Three files are attached to a post's `gallery`; each `Attach` adds at the last position.
2. `Reorder` with every attachment ID once, in the new order, moves the last photo to the front.
3. A partial list (one ID of three) is refused with `invalid_argument` and changes nothing, so a client working from a stale list cannot drop an item.
4. `Detach` removes one attachment and keeps the file.
5. Attaching to an owner type the Store was not told about (`usr` instead of `user`) is `invalid_argument`, not an attachment to nothing.
6. `avatar` is a single role: a second `Attach` is `exists`. `SetAttachment` is the way to swap it, and the replaced file goes to the trash.
7. A file used as a user's avatar and a post's cover is replaced on the user with `PreviousPurge`: since the post still uses it, it is only detached from the user, never purged.

## The code

The Store knows the app's owner types and which roles hold one file:

<!-- ds:block id=newstore-wknhtt4d -->
**newStore** · [`examples/records-attachments/main.go:60-80`](main.go#L60-L80)

```go
func newStore(ctx context.Context, db *sql.DB) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser, ownerPost),
		records.WithSingleRoles(roleAvatar, roleCover),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=17ce67 -->

A small helper that writes an object and records it:

<!-- ds:block id=store-8jvff75a -->
**store** · [`examples/records-attachments/main.go:84-94`](main.go#L84-L94)

```go
func store(ctx context.Context, s *records.Store, name string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	key := "images/" + name
	if _, err := disk.Write(ctx, key, bytes.NewReader([]byte(name)), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: key, Filename: name})
}
```
<!-- /ds:block hash=0f1790 -->

Building the gallery, and reading it back in position order (`Attachments` returns each attachment with its file, leaving trashed files out):

<!-- ds:block id=buildgallery-8a48cqb4 -->
**buildGallery** · [`examples/records-attachments/main.go:99-113`](main.go#L99-L113)

```go
func buildGallery(ctx context.Context, s *records.Store, post records.Owner) ([]records.Attachment, error) {
	var out []records.Attachment
	for _, name := range []string{"beach.jpg", "dunes.jpg", "sunset.jpg"} {
		f, err := store(ctx, s, name)
		if err != nil {
			return nil, err
		}
		a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
```
<!-- /ds:block hash=7928a7 -->

<!-- ds:block id=gallerynames-webyqs5u -->
**galleryNames** · [`examples/records-attachments/main.go:118-128`](main.go#L118-L128)

```go
func galleryNames(ctx context.Context, s *records.Store, post records.Owner) ([]string, error) {
	list, err := s.Attachments(ctx, tenant, post, roleGallery)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, af := range list {
		names = append(names, fmt.Sprintf("%d:%s", af.Attachment.Position, af.File.Filename))
	}
	return names, nil
}
```
<!-- /ds:block hash=3ae923 -->

Reordering takes the full list of IDs exactly once:

<!-- ds:block id=movelasttofront-fmtnaguu -->
**moveLastToFront** · [`examples/records-attachments/main.go:134-141`](main.go#L134-L141)

```go
func moveLastToFront(ctx context.Context, s *records.Store, post records.Owner, atts []records.Attachment) error {
	last := len(atts) - 1
	ids := []string{atts[last].ID}
	for _, a := range atts[:last] {
		ids = append(ids, a.ID)
	}
	return s.Reorder(ctx, tenant, post, roleGallery, ids)
}
```
<!-- /ds:block hash=b130c7 -->

Swapping the one file of a single role, with the previous file trashed unless another owner still uses it:

<!-- ds:block id=setavatar-9td4hpat -->
**setAvatar** · [`examples/records-attachments/main.go:146-148`](main.go#L146-L148)

```go
func setAvatar(ctx context.Context, s *records.Store, user records.Owner, fileID string) (records.Attachment, error) {
	return s.SetAttachment(ctx, tenant, fileID, user, roleAvatar, records.PreviousTrash)
}
```
<!-- /ds:block hash=347850 -->

<!-- ds:block id=sharedfiledemo-u3hcfqef -->
**sharedFileDemo** · [`examples/records-attachments/main.go:228-252`](main.go#L228-L252)

```go
func sharedFileDemo(ctx context.Context, w io.Writer, s *records.Store) error {
	user := records.Owner{Type: ownerUser, ID: "usr_2"}
	post := records.Owner{Type: ownerPost, ID: "post_2"}
	shared, err := store(ctx, s, "team.png")
	if err != nil {
		return err
	}
	if _, err := s.Attach(ctx, tenant, shared.ID, post, roleCover); err != nil {
		return err
	}
	if _, err := s.SetAttachment(ctx, tenant, shared.ID, user, roleAvatar, records.PreviousPurge); err != nil {
		return err
	}
	next, err := store(ctx, s, "solo.png")
	if err != nil {
		return err
	}
	if _, err := s.SetAttachment(ctx, tenant, next.ID, user, roleAvatar, records.PreviousPurge); err != nil {
		return err
	}
	_, err = s.File(ctx, tenant, shared.ID)
	cover, _ := s.Attachments(ctx, tenant, post, roleCover)
	fmt.Fprintf(w, "shared file kept: %t, still the post's cover: %t\n", err == nil, len(cover) == 1)
	return nil
}
```
<!-- /ds:block hash=b2608d -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the Store with its owner types, the gallery, the avatar and the shared file |
| `main_test.go` | Runs the same steps, plus detaching an unknown ID, attaching twice, attaching a trashed file, concurrent `SetAttachment` calls and an unknown `Previous` mode |

## Next

- The guide: [Attachments](../../docs/records/attachments.md).
- Related examples: [records-files](../records-files) for the file rows themselves, [records-http](../records-http) for attachments over HTTP, [records-lifecycle](../records-lifecycle) for what happens to files their owners leave behind.
