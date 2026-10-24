# Attachments

An attachment links a recorded file to a row of your application under a role: post `post_1` has three files as `gallery`, user `usr_1` has one file as `avatar`. This page covers attaching, listing in order, reordering, detaching, roles that hold a single file, and replacing the file of a role in one call. Read it when files belong to something: a profile, a product, a message, a document.

## Before you start

- A Store, as built in [Files](files.md).
- The example is [`examples/records-attachments`](../../examples/records-attachments): a post's gallery and a user's avatar on SQLite and a memory disk. `go run ./records-attachments` in `examples/` prints each step; `go test ./records-attachments` runs them, plus a concurrent `SetAttachment` test.
- The library never reads your tables. An owner is a type you register and an ID you pass; whether `post_1` exists, and whether the caller may touch it, is your application's check.

## Step 1 — Register owner types and single roles

<!-- ds:block id=newstore-wknhtt4d -->
**newStore** · [`examples/records-attachments/main.go:60-80`](../../examples/records-attachments/main.go#L60-L80)

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

- `records.WithOwnerTypes("user", "post")` lists the owner types. `Attach`, `Attachments`, `Reorder`, `SetAttachment` and `PurgeOwner` refuse any other type with `invalid_argument`, so `"usr"` instead of `"user"` fails loudly instead of attaching to nothing. Types match `^[a-z][a-z0-9_.]{0,63}$`.
- ``records.WithForeignOwnerTypes("User", "Admin::User", `App\Models\Post`)`` registers types spelled the way another web framework already stores them in its own polymorphic columns, when the two apps share a database and you want to JOIN without a mapping table. They match `^[A-Za-z][A-Za-z0-9_.:\\-]{0,63}$`: either case, `:`, a backslash and `-` are allowed; `/`, spaces, quotes and control characters are not, so a type stays one URL path segment. New applications keep `WithOwnerTypes`' lowercase names.
- `records.WithSingleRoles("avatar", "cover")` declares roles that hold at most one file per owner. Other roles hold any number.

Roles are your names and need no registration; they match `^[a-z][a-z0-9_.-]{0,63}$`. An owner ID is 1 to 191 bytes (the indexed width on MySQL).

## Step 2 — Attach files

The demo records each file (as in [Files](files.md)) and attaches it to the post's gallery:

<!-- ds:block id=store-8jvff75a -->
**store** · [`examples/records-attachments/main.go:84-94`](../../examples/records-attachments/main.go#L84-L94)

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

<!-- ds:block id=buildgallery-8a48cqb4 -->
**buildGallery** · [`examples/records-attachments/main.go:99-113`](../../examples/records-attachments/main.go#L99-L113)

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

`Attach(ctx, scope, fileID, owner, role)` adds the file at the last position (the highest position plus one) and returns the `records.Attachment` (ID `att_…`). It refuses:

| Case | Code |
|---|---|
| Owner type not registered, empty or too long owner ID, malformed role | `invalid_argument` |
| Unknown file, a file of another scope, or a file in the trash | `not_found` |
| The same file already attached to the same owner and role | `exists` |
| A single role that already holds a file | `exists` (the message says to use `SetAttachment`) |

A file may be attached to any number of owners and roles: the same logo can be a company's `logo` and a page's `hero`.

## Step 3 — List an owner's files in order

`Attachments(ctx, scope, owner, role)` returns `[]records.AttachedFile`, each the attachment and its file, ordered by position (then ID). Files in the trash are left out; their attachments stay, so restoring the file brings it back in its place.

<!-- ds:block id=gallerynames-webyqs5u -->
**galleryNames** · [`examples/records-attachments/main.go:118-128`](../../examples/records-attachments/main.go#L118-L128)

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

Check: `gallery: [1:beach.jpg 2:dunes.jpg 3:sunset.jpg]`.

`Attachment(ctx, scope, id)` reads one attachment by ID (`not_found` otherwise), which is what a detach endpoint needs to authorize against the attachment's owner before removing it.

## Step 4 — Reorder

`Reorder(ctx, scope, owner, role, ids)` sets the order: `ids` must list every attachment of that owner and role that `Attachments` returns (the live files) exactly once, in the new order. Positions become 1 to n. Attachments of trashed files, which `Attachments` and the HTTP list leave out, keep their relative order after the listed ones, so a restored file comes back at the end of the list. Anything else (a missing ID, an extra one, a duplicate) is `invalid_argument` and changes nothing, so a client that dragged items in a stale list cannot silently drop one that another user added meanwhile.

<!-- ds:block id=movelasttofront-fmtnaguu -->
**moveLastToFront** · [`examples/records-attachments/main.go:134-141`](../../examples/records-attachments/main.go#L134-L141)

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

Check: `reordered: [1:sunset.jpg 2:beach.jpg 3:dunes.jpg]`, and sending only one of the three IDs prints `partial reorder: invalid_argument`.

## Step 5 — Detach

`Detach(ctx, scope, attachmentID)` removes the link; the file stays recorded and stored (trash or purge it separately, see [Lifecycle](lifecycle.md)). Detaching an unknown attachment succeeds, so cleanup code can call it unconditionally. Positions are not renumbered: after detaching `dunes.jpg` (position 3) the demo prints `after detach: [1:sunset.jpg 2:beach.jpg]`, and detaching from the middle leaves a gap, which is harmless because only the order matters. The next `Attach` goes after the highest position; the next `Reorder` renumbers from 1.

## Step 6 — One file per role: SetAttachment

An avatar is replaced, not added to. `SetAttachment(ctx, scope, fileID, owner, role, previous)` makes `fileID` the only file of the owner under the role, in one transaction: it detaches every current attachment of that role (including ones whose file is in the trash), attaches the new file, and then disposes of the files it detached as `previous` says.

<!-- ds:block id=setavatar-9td4hpat -->
**setAvatar** · [`examples/records-attachments/main.go:146-148`](../../examples/records-attachments/main.go#L146-L148)

```go
func setAvatar(ctx context.Context, s *records.Store, user records.Owner, fileID string) (records.Attachment, error) {
	return s.SetAttachment(ctx, tenant, fileID, user, roleAvatar, records.PreviousTrash)
}
```
<!-- /ds:block hash=347850 -->

| `previous` | The detached files |
|---|---|
| `records.PreviousKeep` | Only detached; they stay live. |
| `records.PreviousTrash` | Moved to the trash, restorable until purged. |
| `records.PreviousPurge` | Deleted for good, objects after the commit. |

Any other value is `invalid_argument`. A detached file that another attachment still uses (another owner, another role) is only detached, whatever `previous` says:

<!-- ds:block id=sharedfiledemo-u3hcfqef -->
**sharedFileDemo** · [`examples/records-attachments/main.go:228-252`](../../examples/records-attachments/main.go#L228-L252)

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

Check: `avatar is the new file: true; previous trashed: true`, then `shared file kept: true, still the post's cover: true`. Setting the file that is already the role's only file does nothing and returns its attachment.

`SetAttachment` works on any role, not only single roles; on a many-file role it reduces the role to one file.

## Long lists: AttachmentsPage

`Attachments` returns every live file of an owner's role at once. For a role that can hold thousands (a gallery, a ticket's attachments), `AttachmentsPage(ctx, records.AttachmentQuery{Scope, Owner, Role, Limit, Cursor})` returns the same files in the same position order a page at a time (`Limit` 0 is 50, at most 1000), with `NextCursor` empty on the last page. A cursor is a position and an ID, so a `Reorder` between pages can skip or repeat items; start again after one. An owner's files can also be filtered and listed newest first with `AttachedTo` ([Files](files.md#an-owners-files-as-a-filter)).

<!-- ds:block id=gallerypages-q7d5vz -->
**galleryPages** · [`examples/records-query/main.go:118-138`](../../examples/records-query/main.go#L118-L138)

```go
func galleryPages(ctx context.Context, s *records.Store, post records.Owner) ([][]string, error) {
	var pages [][]string
	cursor := ""
	for {
		page, err := s.AttachmentsPage(ctx, records.AttachmentQuery{
			Scope: tenant, Owner: post, Role: roleGallery, Limit: pageSize, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, af := range page.Files {
			keys = append(keys, af.File.Key)
		}
		pages = append(pages, keys)
		if page.NextCursor == "" {
			return pages, nil
		}
		cursor = page.NextCursor
	}
}
```
<!-- /ds:block hash=d131f2 -->

## A role with at most N files

`records.WithRoleLimit(role, max)` caps how many files one owner may have under a role: a gallery of 20 photos, 5 attachments per message. `Attach` past the cap is `quota_exceeded` ("post "p1" holds at most 3 files as "gallery""); attaching the same file twice is still `exists`. Attachments of trashed files keep their place, since a restore brings them back; `Detach` frees one. `max` is from 2 to `records.MaxRoleLimit` (1000); one file per owner is `WithSingleRoles`, and a role cannot be both.

The database enforces the cap under concurrent requests, the way it enforces single roles: such a role's attachment IDs are derived from scope, owner, role and a slot number from 1 to `max`, so no more than `max` rows can exist. Two `Attach` calls racing for the last free slot make one fail with `unavailable`; retry it. `WithIDs` does not apply to limited roles.

<!-- ds:block id=attachphotos-y5k2hb -->
**attachPhotos** · [`examples/records-query/main.go:93-100`](../../examples/records-query/main.go#L93-L100)

```go
func attachPhotos(ctx context.Context, s *records.Store, post records.Owner, photos []records.File) error {
	for _, f := range photos {
		if _, err := s.Attach(ctx, tenant, f.ID, post, roleGallery); err != nil {
			return fmt.Errorf("photo %s: %w", f.Key, err)
		}
	}
	return nil
}
```
<!-- /ds:block hash=c1c9fa -->

## Concurrency: what single roles guarantee

A single role's attachment ID is not random: it is derived from scope, owner and role (`att_` plus 128 bits of a SHA-256). The primary key therefore admits one row per owner and role, which is how the database enforces the rule. No portable unique index could express "one row per owner and role, among the rows for this role only" (MySQL has no partial indexes), and locking zero rows does not stop a Postgres insert. Consequences:

- Two concurrent `SetAttachment` calls on a single role cannot both win. On Postgres and MySQL the loser fails with `exists` and can retry; on SQLite, which has one writer, they run one after the other and the last one wins. Either way the role ends with exactly one file (`TestConcurrentSetAttachment` in the example runs three at once).
- A second `Attach` to an occupied single role is `exists`, even from two racing requests.
- `WithIDs` does not apply to single-role attachments.
- Declaring an existing many-file role as single later does not merge its rows: owners that already hold several files keep them (the derived ID matches none of their random IDs), until a `SetAttachment` replaces them.

On a role that is not single, two concurrent `SetAttachment` calls with different files can both commit, leaving two files. Declare the role single if it must hold one.

## Events

`Attach` emits `attachment.created`, `Detach` `attachment.deleted` (both with `FileID`, `AttachmentID`, `Owner`, `Role`), `Reorder` `attachments.reordered` (with `Owner`, `Role`). `SetAttachment` emits what its steps do: a deletion per detached attachment, a creation, and `file.trashed` or `file.purged` per disposed file. Detaching an unknown attachment emits nothing. See [Events and hooks](events-and-hooks.md).

## What can go wrong

- `quota_exceeded` from `Attach`: the role has a `WithRoleLimit` and the owner holds that many files there already, trashed ones included. Detach one first.
- `unavailable` from `Attach` on a limited role: another request took the last free place at the same moment; retry.

- `invalid_argument: records: owner type "usr" is not registered (WithOwnerTypes)`: the type is misspelled or missing from `WithOwnerTypes`. The demo prints `unknown owner type: invalid_argument`.
- `exists` from `Attach` on an avatar: the role is single and occupied; call `SetAttachment` instead. The demo prints `second Attach to a single role: exists`.
- `not_found` from `Attach`: the file is trashed, belongs to another scope, or the ID is wrong.
- `invalid_argument` from `Reorder`: the client's list is stale or partial; reload and retry.
- A gallery missing an item after a delete: the file was trashed, not detached. `Attachments` hides trashed files; `Restore` brings it back.

## Next

- [Lifecycle](lifecycle.md): `PurgeOwner` deletes everything an owner leaves behind when your app deletes the owner.
- [HTTP endpoints](http-endpoints.md): upload-and-attach, list, reorder and detach over HTTP.
- [Avatars](../recipes/avatars.md) and [product images](../recipes/product-images.md) build on this page.
