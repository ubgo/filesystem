# github.com/ubgo/filesystem/records/acl

Permissions on the files of a records Store: grants of a level (`Read`, `Write`, `Admin`) to a subject on one file or on a folder, inherited by every file in the folder and its subfolders, and a `Can` check that resolves them. Sharing with named users and team folders are grants. The guide is [docs/plugins/acl.md](../../docs/plugins/acl.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

Part of the records module; no dependency of its own.

## Wire it

```go
fp := folders.New()
ap := acl.New(acl.WithHierarchy(fp)) // folders make grants on folders inherit
store, err := records.New(db, dialect, disks, records.WithPlugins(fp, ap))
```

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...)`, `WithHierarchy(h)`, `WithClock(now)` | A plugin; `WithHierarchy` enables grants on folders (the folders plugin, or any `Hierarchy` of yours); `WithClock` stamps `CreatedAt`, without it the plugin reads the Store's clock (`records.WithClock`), so one clock rules the Store and its plugins. Grant IDs come from `Store.NewID` (`records.WithIDs`) |
| `WithLevels(LevelDef{Level, Includes}...)`, `DefaultLevels()`, `(*Plugin).Levels()` | Your own levels instead of the read < write < admin ladder: `comment` that includes `view`, an `upload` level for drop-box folders that includes nothing; inclusion is transitive, every included level must be defined, names `[a-z][a-z0-9_]*`. Grants of a level no longer defined match nothing |
| `WithLevelFor(fn func(records.Action) (Level, bool))` | Replaces `LevelFor` as the map from an action to the level `Authorizer` asks for ("visibility needs Admin"); `ok` false leaves the action to `next`; a level the plugin does not define refuses |
| `WithLevelForTarget(fn LevelMapper)` | The same map with `ctx` and the `records.ActionTarget`: the loaded `File` and the requested `Change` ("making a file public needs Admin, private needs Write", "a file under legal hold needs Admin"); the later of the two options wins |
| `WithPrecedence(fn Precedence)`, `DenyOverrides`, `MostSpecific`, `Match{Grant, Depth, Applies}`, `ContainerDepth` | The rule that decides a check from its matching grants. `DenyOverrides` is the default (any applying deny wins). `MostSpecific` lets the nearest applying grant decide (NTFS-style broken inheritance: a file allow reopens a folder deny; at one distance a deny wins). A function of your own sees every match, nearest first; its error fails the check |
| `WithLevelsPerSubject()`, `(*Plugin).RevokeLevel(ctx, s, scope, kind, resource, subject, level)` | Several grants per subject on one resource, one per level (non-ladder levels side by side, an allow beside a deny of another level); `Grant` replaces only the same level. Adds migration `0004_grants_level_key`; keep it on once migrated |
| `WithContainers(c...)`, `Container{Kind, Of, Check}` | Grants on resources of your own model besides files and folders (an owner, a project): `Of` names the containers that hold a file, `Check` refuses a grant on one that does not exist; an `Of` error fails `Can` |
| `(*Plugin).Grant(ctx, s, scope, Grant{Subject, Kind, Resource, Level, ExpiresAt, Effect})` | Gives a subject a level on a file (`OnFile`), folder (`OnFolder`) or container kind, replacing the grant it had there; `ExpiresAt` (optional, after now) ends it ("access until Friday"); `Effect` is `Allow` (the default) or `Deny` (needs `WithDeny`); an unknown resource is `not_found` |
| `WithDeny()`, `Effect` (`Allow`, `Deny`, `EffectValues`) | Deny grants: a deny that applies to a check (on the file, a folder above it or a container holding it) beats every allow, explicit or inherited, on any of them; a deny of a level refuses it and every level that includes it (deny `write` leaves `read`); an expired deny refuses nothing. Without it a deny grant is `invalid_argument`; stored denies are honoured either way. Events of a deny carry `DetailEffect` |
| `(*Plugin).Prune(ctx, s, cutoff)` | Deletes grants that expired before `cutoff`; returns how many |
| `(*Plugin).Revoke(ctx, s, scope, kind, resource, subject)` | Removes the subject's grants there (every level with `WithLevelsPerSubject`); twice does nothing |
| `(*Plugin).Can(ctx, s, scope, subjects, fileID, level)` | Whether any of the subjects holds at least `level` on the file, directly or through its folder and the folders above, and none is denied it there (`WithDeny`) |
| `(*Plugin).CanFolder(ctx, s, scope, subjects, folderID, level)` | The same for a folder |
| `(*Plugin).Grants(ctx, s, scope, kind, resource)`, `GrantsOf(ctx, s, scope, subject)` | Who has access to a file or folder; what a subject has access to |
| `Authorizer(p, s, subjects, next)`, `LevelFor(action)` | A `recordshttp` `Authorize` that allows file actions a grant covers (by default read: `Read`; replace, trash, restore, visibility, text: `Write`; purge: `Admin`) and asks `next`, your own rule, for everything else. It takes `records.Action` and `records.ActionTarget` (which `recordshttp.Action` and `Target` alias), so the plugin does not import `recordshttp` |

Subjects are opaque (`user:alice`, `group:editors`); `Can` takes every subject the caller speaks for, so group membership stays in the application. Grants on files go with the file's purge, all grants with `Store.PurgeScope`; a grant on a deleted folder matches nothing. Granting and revoking emit `acl.granted` and `acl.revoked`.

## Table

`fs_acl_grants` (migrations `acl/0001_grants`, `acl/0002_grants_expiry`, `acl/0003_grants_effect`): `id`, `scope`, `subject`, `resource_kind`, `resource_id`, `level`, `created_at`, `expires_at` (nullable, from 0002), `effect` (`allow` or `deny`, default `allow`, from 0003); unique `(scope, resource_kind, resource_id, subject)`, index `(scope, subject)`. With `WithLevelsPerSubject`, migration `acl/0004_grants_level_key` replaces the unique key with `(scope, resource_kind, resource_id, subject, level)` (`fs_acl_grants_resource_level`, created before the old one is dropped).
