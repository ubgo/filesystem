# Permissions

You will have permissions on files and folders: give a user or a group read, write or admin access to one file, or to a folder and everything in it, and check them before serving or changing a file. "Share with named people", team folders and "editors of this folder" are all grants. Install the `acl` plugin when the uploader-owns-it rule is no longer enough.

Which tool for which kind of sharing:

| You want | Use |
|---|---|
| Only the uploader, or only the tenant, touches a file | No plugin: scope every read, and check `File.UploadedBy` in your `Authorize`. |
| Named users or groups of your app get access to files or folders | This plugin. |
| People without an account open a file or a folder through a link | [Share links](share.md). |
| A file belongs to a row (a post's images, a user's avatar) and follows that row's permissions | [Attachments](../records/attachments.md), checked by your rule on the owner row. |

## Before you start

- A Store, as in [Files](../records/files.md). For grants on folders, the [folders](folders.md) plugin, or any tree of your own that implements `acl.Hierarchy`.
- Who the subjects are is yours: `user:alice`, `group:editors`, `api-key:ci`. The plugin never expands a user into groups; `Can` takes every subject the caller speaks for.
- The example is [`examples/records-acl`](../../examples/records-acl): `go run ./records-acl` in `examples/` prints every step below; `go test ./records-acl` runs them.

## Step 1 — Install it

`acl.New(acl.WithHierarchy(fp))` reads folder membership from the folders plugin:

<!-- ds:block id=newaclstore-w3n8k5qp -->
**newStore** · [`examples/records-acl/main.go:53-61`](../../examples/records-acl/main.go#L53-L61)

```go
func newStore(ctx context.Context, db *sql.DB) (*records.Store, *folders.Plugin, *acl.Plugin, error) {
	fp := folders.New()
	ap := acl.New(acl.WithHierarchy(fp))
	s, err := storeWith(ctx, db, fp, ap)
	if err != nil {
		return nil, nil, nil, err
	}
	return s, fp, ap, nil
}
```
<!-- /ds:block hash=440b32 -->

`Migrate` creates `fs_acl_grants`. Grant IDs come from the Store's generator (`records.WithIDs`, through `Store.NewID`), and `CreatedAt` from the Store's clock unless `acl.WithClock` sets one. Without `WithHierarchy` only grants on single files exist, and a folder grant is `invalid_argument` (the test `TestFolderGrantNeedsHierarchy` pins it). The disk signs URLs only because the HTTP route in step 4 redirects to one.

Check: `Migrate` returns `acl/0001_grants` among the applied migrations.

## Step 2 — Grant

A `Grant` names a subject, a resource (`acl.OnFile` with a file ID, or `acl.OnFolder` with a folder ID) and a level:

<!-- ds:block id=sharefolder-k9t2v6xm -->
**shareFolder** · [`examples/records-acl/main.go:91-97`](../../examples/records-acl/main.go#L91-L97)

```go
func shareFolder(ctx context.Context, s *records.Store, ap *acl.Plugin, folderID, fileID string) error {
	if _, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: legal, Kind: acl.OnFolder, Resource: folderID, Level: acl.Write}); err != nil {
		return err
	}
	_, err := ap.Grant(ctx, s, tenant, acl.Grant{Subject: bob, Kind: acl.OnFile, Resource: fileID, Level: acl.Read})
	return err
}
```
<!-- /ds:block hash=bb2e95 -->

- Levels include the ones below: `Admin` can do everything `Write` can, and `Write` everything `Read` can.
- A grant on a folder reaches every file in it and in its subfolders at the time of the check, so a file put there later is covered and a file moved out is not.
- Granting the same subject on the same resource again replaces the level.
- Each grant and revoke emits `acl.granted` or `acl.revoked` in the change's transaction, so the [audit plugin](audit.md) and [webhooks](webhooks.md) see who shared what.

## Step 3 — Check

`Can(ctx, store, scope, subjects, fileID, level)` is true when any grant to any of the subjects covers the file at that level or above, directly or through one of the file's folders. `CanFolder` asks the same for a folder.

<!-- ds:block id=can-c5m9w3rh -->
**can** · [`examples/records-acl/main.go:102-104`](../../examples/records-acl/main.go#L102-L104)

```go
func can(ctx context.Context, s *records.Store, ap *acl.Plugin, who []string, fileID string, level acl.Level) (bool, error) {
	return ap.Can(ctx, s, tenant, who, fileID, level)
}
```
<!-- /ds:block hash=d828e9 -->

Check: the example prints, for the NDA in `Contracts/Signed`:

```text
user:alice               read  nda.txt  false
user:alice+group:legal   read  nda.txt  true
user:alice+group:legal   write nda.txt  true
user:alice+group:legal   admin nda.txt  false
```

Alice alone has no grant; the group's write on `Contracts` reaches the subfolder and includes read, but not admin. Bob's file grant gives him `read memo.txt true`, `write memo.txt false` and nothing on the NDA.

`Grants(ctx, store, scope, kind, resource)` lists who has access to a file or folder (the "shared with" panel) and `GrantsOf(ctx, store, scope, subject)` what a subject can reach (the "shared with me" view). `Revoke(ctx, store, scope, kind, resource, subject)` removes a grant. The example prints `shared with on Contracts: group:legal write; shared with bob: 1 grant(s)`, then `after revoke, bob reads memo.txt: false`.

## Step 4 — Use it in the HTTP routes

`acl.Authorizer` builds a [recordshttp](../records/http-endpoints.md) `Authorize`. It allows a file action when a grant covers it: reading needs `Read`; replacing, trashing, restoring and changing visibility or text need `Write`; purging needs `Admin` (`acl.LevelFor` maps an action to its level). Everything else goes to your own rule: the owner routes, set-attachment, and file actions no grant allows. Here the rule lets the uploader do anything with their own files:

<!-- ds:block id=newaclhandler-q4h7n2zc -->
**newHandler** · [`examples/records-acl/main.go:136-144`](../../examples/records-acl/main.go#L136-L144)

```go
func newHandler(s *records.Store, ap *acl.Plugin) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:     s,
		Scope:     func(*http.Request) (string, error) { return tenant, nil },
		Authorize: acl.Authorizer(ap, s, subjectsOf, uploaderRule(s)),
		Disk:      diskName,
		Policy:    upload.Policy{MaxSize: 1 << 20, Allowed: []string{"text/plain"}, Key: upload.RandomKey("docs")},
	})
}
```
<!-- /ds:block hash=c7e320 -->

`subjectsOf(r)` returns who the request speaks for: the user and its groups, from your session. An empty list is anonymous and gets nothing from grants. Check: the example prints `GET /files/{nda} as user:alice: 302` (a redirect to the signed URL) and `GET /files/{nda} as user:bob: 403`.

## Your own permission model

The read < write < admin ladder is the default, not the contract. These options let the plugin follow the model your product already has, each leaving the rest of the plugin as it is:

- `acl.WithLevels(acl.LevelDef{Level, Includes}...)` replaces the ladder. Levels need not be linear: a grant gives every level it includes, transitively, and nothing else.
- `acl.WithLevelFor(fn)` replaces `acl.LevelFor`, the map from a [recordshttp](../records/http-endpoints.md) action to the level `Authorizer` asks for. Return `ok` false to leave an action to your `next` rule.
- `acl.WithLevelForTarget(fn)` is the same map with the request's context and the action's target (`records.ActionTarget`): the file as loaded and, on the visibility, text and set-attachment routes, the change the request asks for (`Target.Change`). "Making a file public needs admin, making it private needs write" and "a file under legal hold needs admin to trash" are written here (below).
- `acl.WithLevelsPerSubject()` lets a subject hold several grants on one resource, one per level: "comment" and "upload" on one file when your levels are not a ladder, or an allow of one level beside a deny of another; `Grant` then replaces only the subject's grant of the same level, and `RevokeLevel(ctx, s, scope, kind, resource, subject, level)` removes one level while `Revoke` still removes them all. It adds migration `acl/0004_grants_level_key`, which widens the unique key to include the level; run `Migrate` after turning it on, and keep it on once migrated.
- `acl.WithContainers(c...)` lets grants sit on resources of your own model besides files and folders: a post, a project, a client. `Of` names the containers that hold a file; `Check` refuses a grant on one that does not exist.
- `Grant.ExpiresAt` ends a grant at a time ("access until Friday"); `Prune` deletes expired grants.
- `acl.WithDeny()` adds deny grants (`Grant.Effect: acl.Deny`), for "everyone in the team folder except the contractor" (below).
- `acl.WithPrecedence(fn)` replaces the rule that decides between allows and denies (below): `acl.MostSpecific` for NTFS-style broken inheritance, or a function of your own.

A document tool with view, comment and edit, a drop-box level that may only upload, and grants on whole projects:

```go
const (
	view    acl.Level = "view"
	comment acl.Level = "comment"
	edit    acl.Level = "edit"
	dropbox acl.Level = "dropbox" // upload only: sees nothing
)

ap := acl.New(
	acl.WithLevels(
		acl.LevelDef{Level: view},
		acl.LevelDef{Level: comment, Includes: []acl.Level{view}},
		acl.LevelDef{Level: edit, Includes: []acl.Level{comment}},
		acl.LevelDef{Level: dropbox},
	),
	acl.WithLevelFor(func(a records.Action) (acl.Level, bool) {
		switch a {
		case records.ActionRead:
			return view, true
		case records.ActionSetText:
			return comment, true
		case records.ActionReplace, records.ActionTrash, records.ActionRestore, records.ActionSetVisibility, records.ActionPurge:
			return edit, true
		}
		return "", false // the owner routes go to your own rule
	}),
	acl.WithContainers(projects{}), // Kind() "project"; Of(file) reads your projects table
)

until := time.Now().Add(72 * time.Hour)
_, err := ap.Grant(ctx, store, scope, acl.Grant{Subject: "user:carol", Kind: "project", Resource: "prj_42", Level: comment, ExpiresAt: &until})
```

A map that reads the change: publishing needs admin, unpublishing write, a file under legal hold needs admin for anything that changes it.

```go
ap := acl.New(acl.WithLevelForTarget(func(ctx context.Context, a records.Action, t records.ActionTarget) (acl.Level, bool) {
	if t.File != nil && t.File.Metadata["hold"] == "legal" && a != records.ActionRead {
		return acl.Admin, true
	}
	if a == records.ActionSetVisibility && t.Change != nil && t.Change.Visibility == filesystem.VisibilityPublic {
		return acl.Admin, true
	}
	return acl.LevelFor(a)
}))
```

`Target.Change` is read from the request's capped body before `Authorize`, and is nil when the body cannot be read; that request is still answered `403` by a refusing rule and `400` only once a rule allows it.

Try it: [`examples/records-acl`](../../examples/records-acl) runs this section in `custom.go` ([`newCustomACL`](ds:block?id=newcustomacl-r7d2m9xk), [`grantUntil`](ds:block?id=grantuntil-b4q8w2nf)): `go run ./records-acl` from `examples/`.

### Deny grants

Grants only allow unless the plugin is built with `acl.WithDeny()`. With it, a grant whose `Effect` is `acl.Deny` takes a level away:

```go
ap := acl.New(acl.WithHierarchy(fp), acl.WithDeny())

// Everyone on the team writes in the team folder...
_, err := ap.Grant(ctx, store, scope, acl.Grant{Subject: "group:team", Kind: acl.OnFolder, Resource: team.ID, Level: acl.Write})
// ...except the contractor, who sees nothing under Finance, for the length of the audit.
until := time.Now().Add(30 * 24 * time.Hour)
_, err = ap.Grant(ctx, store, scope, acl.Grant{Subject: "user:contractor", Kind: acl.OnFolder, Resource: finance.ID, Level: acl.Read, Effect: acl.Deny, ExpiresAt: &until})
```

The default precedence is the safe one, `acl.DenyOverrides`: a deny that applies to a check wins over every allow, wherever each sits. "Applies" means on the file, on its folder or any folder above it, or on a container holding it, for one of the subjects the caller passes. So a folder deny beats a file allow inside it, a subfolder deny beats an allow inherited from above, and an allow can never reopen a deny; to let someone back in, revoke the deny or give it an `ExpiresAt`. A deny of a level refuses that level and every level that includes it: denying `write` refuses `write` and `admin` and leaves `read`. An expired deny refuses nothing.

A subject has one grant per resource (unless `WithLevelsPerSubject`, which keeps one per level), so a deny given where the subject had an allow replaces it, and the other way round. `Grants` lists both, with their `Effect`; `acl.granted` and `acl.revoked` events of a deny carry `DetailEffect: "deny"`. Without `WithDeny`, `Grant` refuses a deny (`invalid_argument`), but `Can` still honours denies already stored, so turning the option off never turns a refusal into access.

#### Another precedence

`acl.WithPrecedence(fn)` replaces deny-overrides. `acl.MostSpecific` is the model of NTFS and SharePoint's broken inheritance: the applying grant nearest the resource decides, so an explicit allow on a file reopens a deny on its folder ("contractor denied on /team, allowed on /team/brief.pdf"), a deny on a file closes an allow on its folder, grants on containers count as farther than every folder, and at one distance a deny still wins. It is never the default, since it lets a narrower grant undo a broader deny.

```go
ap := acl.New(acl.WithHierarchy(fp), acl.WithDeny(), acl.WithPrecedence(acl.MostSpecific))
```

A precedence of your own is a `func(ctx, level, matches []acl.Match) (bool, error)`. Each `acl.Match` is one unexpired grant of the asked subjects on the path, nearest first: the `Grant`, its `Depth` (0 for the file or the folder checked, n for the n-th folder above, `acl.ContainerDepth` for a container) and whether it `Applies` to the level asked; `Denies()` is true for a deny and for any effect the code does not know. "A user's own grant beats its group's" ranks by `Grant.Subject`. An error fails the check: `Can` returns it and `Authorizer` refuses the request without asking `next`.

Mistakes fail closed: an option with a level that includes an undefined one, a duplicate level, a container named `file` or a nil `WithLevelFor`, `WithLevelForTarget` or `WithPrecedence` makes `Migrate`, `Grant` and `Can` return `invalid_argument`, and an action mapped to a level the plugin does not define refuses the request instead of falling through to `next`. A permission engine of your own (OpenFGA, SpiceDB, Cedar) does not need this plugin at all: give `recordshttp.Config.Authorize` a function that asks it.

Try it: [`examples/records-acl`](../../examples/records-acl) runs this section in `deny.go` ([`everyoneButContractor`](ds:block?id=everyonebutcontractor-p2x8v5nk)): `go run ./records-acl` from `examples/`, then look for `deny: user:contractor reads budget.txt: false`.

## Try it

```sh
cd examples
go run ./records-acl
go test ./records-acl
```

## What can go wrong

- `invalid_argument` granting on a folder: the plugin has no `WithHierarchy`.
- A grant that seems ignored: `Can` was called without the group subject that holds it; pass every subject the caller is.
- `invalid_argument` on `Grant`: an unknown level (with `WithLevels`, the default `read`, `write` and `admin` are unknown too), a subject over `acl.MaxSubjectBytes` (191), an `ExpiresAt` that is not in the future, or a file, folder or container that does not exist in the scope. Grants never cross scopes.
- A grant that stopped working at a set time: it had an `ExpiresAt`; `Grants` still lists it until `Prune` deletes it.
- An allow that seems ignored with `WithDeny`: a deny for one of the subjects sits on the file, a folder above it or a container holding it. `GrantsOf` for each subject shows it. To let an explicit allow below win, use `WithPrecedence(acl.MostSpecific)`.
- A second level that replaces the first: the plugin has no `WithLevelsPerSubject`. With it, `unavailable` on a second level means migration `acl/0004_grants_level_key` has not run yet.
- Checking a listing one file at a time: `Can` is a query per file. For "files I can see" listings, filter by folder (the folders the subject can reach, from `GrantsOf`) rather than calling `Can` on every row.
- Grants on a deleted folder stay in the table but match nothing, since folder IDs are never reused; files' grants go with their purge, and every grant with `Store.PurgeScope`.
- `Authorize` is the only gate: a handler of your own that serves bytes outside `recordshttp` must call `Can` itself.

## Next

- [Share links](share.md) for people without an account.
- [Folders](folders.md) for the tree grants follow.
- The plugin's [README](../../records/acl/README.md) lists the API and the table.
