# Keeping earlier versions of a file

The versions plugin of the records layer: each time a file's content is replaced, the content it replaces is kept as a numbered version, up to a cap per file. Versions can be listed, restored (a restore can itself be undone) and deleted; they follow their file to another disk when it is made public, go with it when it is purged, and are not charged by the quota plugin. This is the version history of a document editor or a shared drive. It uses `github.com/ubgo/filesystem/records`, `records/versions` and `records/quota` on SQLite over two in-memory disks, one per visibility. The guide [Versions](../../docs/plugins/versions.md) explains each step.

## Run it

```sh
cd examples
go run ./records-versions
go test ./records-versions
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory. The cap is two versions per file, so the cap shows after three edits.

## What it does

```sh
go run ./records-versions
```

```text
uploaded on private, versions: 0
edit 1 -> 2B, history: v1:1B@private
edit 2 -> 3B, history: v2:2B@private v1:1B@private
edit 3 -> 4B, history: v3:3B@private v2:2B@private
v1 dropped past the cap, object still stored: false
quota counts 4 bytes (current content only)
restored v2 -> 2B, history: v4:4B@private v3:3B@private
deleted one, then published: history: v4:4B@public
purged: history "", newest version object still stored: false
free-co after 3 edits keeps 1 version(s)
pro-co after 3 edits keeps 3 version(s)
Trim 40 days later drops 3 pro-co version(s) past 30 days
restored copy stays in the tenant's prefix: true
```

1. A 1-byte file is uploaded to the private disk with no versions.
2. Three edits keep the replaced content as `v1`, `v2`, `v3`, newest first. The third edit passes the cap of two, so `v1` is dropped and its object deleted after the commit.
3. The quota plugin charges only the current 4 bytes, not the kept versions.
4. Restoring `v2` copies its object back as the content (2 bytes); the 4-byte content it replaces becomes `v4`, and the cap keeps the two newest.
5. Deleting the oldest version, then making the file public, moves its object and every kept version to the public disk.
6. Purging the file deletes its versions and their objects.
7. `plans.go` opens a second Store whose history follows each tenant's plan, with `versions.WithKeepFunc(planKeep)`: the free plan keeps one version, pro keeps ten for at most 30 days. After three edits each, the free tenant's file keeps 1 version and the pro tenant's 3.
8. Forty days later nobody has touched the pro file, so `Trim` applies its policy now and drops the 3 versions older than 30 days.
9. With `versions.WithRestoreKey(versions.RestoreBesideFile)`, restoring the free tenant's version puts the copy in a `versions/` directory beside the file, under `tenants/free-co/`, rather than at the disk's root. The test `TestKeepFuncFailsClosed` shows a replacement in a scope without a plan is refused and rolled back.

## The code

Two disks, one per visibility, and a Store that routes between them with versions (keeping two per file) and quota installed:

<!-- ds:block id=newdisks-8x2d8493 -->
**newDisks** · [`examples/records-versions/main.go:54-64`](main.go#L54-L64)

```go
func newDisks() (*filesystem.Manager, error) {
	pub, err := filesystem.NewDisk(publicDisk, memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return nil, err
	}
	priv, err := filesystem.NewDisk(privateDisk, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(priv, pub)
}
```
<!-- /ds:block hash=ccee59 -->

<!-- ds:block id=newstore-2jkdgkez -->
**newStore** · [`examples/records-versions/main.go:83-101`](main.go#L83-L101)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *versions.Plugin, *quota.Plugin, error) {
	v, err := versions.New(versions.WithKeep(keep))
	if err != nil {
		return nil, nil, nil, err
	}
	q, err := quota.New(unlimited)
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithVisibilityDisks(publicDisk, privateDisk), records.WithPlugins(v, q))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, err
	}
	return s, v, q, nil
}
```
<!-- /ds:block hash=e847b1 -->

An edit: the plugin keeps the replaced content inside `Replace`'s transaction and drops the oldest past the cap:

<!-- ds:block id=edit-3jmwen3q -->
**edit** · [`examples/records-versions/main.go:108-110`](main.go#L108-L110)

```go
func edit(ctx context.Context, s *records.Store, f records.File, body string) (records.File, error) {
	return s.Replace(ctx, tenant, f.ID, strings.NewReader(body), "", textPolicy)
}
```
<!-- /ds:block hash=0c9d31 -->

Listing, restoring and deleting versions:

<!-- ds:block id=history-nvyew4ud -->
**history** · [`examples/records-versions/main.go:114-124`](main.go#L114-L124)

```go
func history(ctx context.Context, s *records.Store, v *versions.Plugin, fileID string) (string, []versions.Version, error) {
	list, err := v.Versions(ctx, s, tenant, fileID)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(list))
	for _, ver := range list {
		parts = append(parts, fmt.Sprintf("v%d:%dB@%s", ver.Number, ver.Size, ver.Disk))
	}
	return strings.Join(parts, " "), list, nil
}
```
<!-- /ds:block hash=59d572 -->

<!-- ds:block id=restore-v7aa5ygn -->
**restore** · [`examples/records-versions/main.go:130-136`](main.go#L130-L136)

```go
func restore(ctx context.Context, s *records.Store, v *versions.Plugin, versionID string) (records.File, error) {
	ver, err := v.Version(ctx, s, tenant, versionID)
	if err != nil {
		return records.File{}, err
	}
	return v.Restore(ctx, s, tenant, ver.ID)
}
```
<!-- /ds:block hash=bd45f4 -->

<!-- ds:block id=forget-jw43mckv -->
**forget** · [`examples/records-versions/main.go:140-142`](main.go#L140-L142)

```go
func forget(ctx context.Context, s *records.Store, v *versions.Plugin, versionID string) error {
	return v.Delete(ctx, s, tenant, versionID)
}
```
<!-- /ds:block hash=3b8d14 -->

Publishing, with the versions moving along:

<!-- ds:block id=publish-3ffb8xnh -->
**publish** · [`examples/records-versions/main.go:148-150`](main.go#L148-L150)

```go
func publish(ctx context.Context, s *records.Store, f records.File) error {
	return s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic)
}
```
<!-- /ds:block hash=7c9d79 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: two disks, the plugins, edits, restore, delete, publish and purge |
| `plans.go` | History per plan with `WithKeepFunc`, `Trim`, and restored copies beside the file with `WithRestoreKey(RestoreBesideFile)`; code: [`newPlanStore`](ds:block?id=newplanstore-k8w4t6mb) |
| `main_test.go` | Runs the same steps, plus version objects moving on publish, a cap that must be positive, an unknown version, and a replacement refused in a scope without a plan |

## Next

- The guide: [Versions](../../docs/plugins/versions.md).
- Related examples: [records-replace](../records-replace) for replacing content, [records-quota](../records-quota) for the quota plugin, [recipe-team-drive](../recipe-team-drive).
