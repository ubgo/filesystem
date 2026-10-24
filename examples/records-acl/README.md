# Permissions on files and folders

The `acl` plugin of the records layer: grants that give a user or a group read, write or admin access to one file or to a folder and everything in it, checked by `Can` and, behind the HTTP routes, by `acl.Authorizer`. It runs on SQLite over an in-memory disk with the `folders` plugin as the hierarchy. The guide [Permissions](../../docs/plugins/acl.md) explains each step.

## Run it

```sh
cd examples
go run ./records-acl
go test ./records-acl
```

It needs no network or environment variables; the database lives in a temporary directory deleted on exit.

## What it does

```text
user:alice               read  nda.txt  false
user:alice+group:legal   read  nda.txt  true
user:alice+group:legal   write nda.txt  true
user:alice+group:legal   admin nda.txt  false
user:bob                 read  memo.txt true
user:bob                 write memo.txt false
user:bob                 read  nda.txt  false
shared with on Contracts: group:legal write; shared with bob: 1 grant(s)
after revoke, bob reads memo.txt: false
GET /files/{nda} as user:alice: 302
GET /files/{nda} as user:bob: 403
user:carol view    spec.txt true
user:carol comment spec.txt true
user:carol edit    spec.txt false
user:dave  dropbox spec.txt true
user:dave  view    spec.txt false
grant on a missing project: not_found
check for a level the model lacks: invalid_argument
GET /files/{spec} as user:carol before expiry: 302
after 73h0m0s, carol views spec.txt: false; pruned 1 expired grant(s)
GET /files/{spec} as user:carol after expiry: 403
deny: user:dana reads budget.txt: true
deny: user:contractor reads roadmap.txt: true
deny: user:contractor reads budget.txt: false
deny: a deny without WithDeny: invalid_argument
deny: stored denies still refuse without WithDeny: true
```

1. `nda.txt` sits in `Contracts/Signed`. The legal group has `Write` on `Contracts`, which reaches the subfolder. Bob has `Read` on `memo.txt` alone.
2. Alice alone has nothing; Alice speaking for her group can read and write the NDA (write includes read) but not purge it (admin).
3. Bob can read the memo, not write it, and cannot see the NDA.
4. `Grants` lists who has access to the folder, `GrantsOf` what Bob can reach; `Revoke` removes Bob's grant.
5. Over HTTP, `acl.Authorizer` lets Alice (with the group the app resolves for her) read the NDA, a redirect to its signed URL, and refuses Bob, who has no grant and is not the uploader.
6. A second Store runs the app's own permission model (`custom.go`): `acl.WithLevels` defines view, comment (includes view), edit (includes comment) and an upload-only dropbox level that includes nothing; `acl.WithLevelFor` maps a read to view; `acl.WithContainers` lets grants sit on the app's projects. Carol gets comment on project `prj_42` until three days from now (`Grant.ExpiresAt`), so she can view and comment on `spec.txt` but not edit it; Dave's dropbox grant lets him see nothing. A grant on a project the app does not know is `not_found`, and a check for `admin`, which this model does not define, is `invalid_argument`.
7. The plugin's clock then moves past the expiry: Carol's grant gives nothing, `Prune` deletes it, and the same HTTP read that answered `302` now answers `403`.
8. A third Store is built with `acl.WithDeny()` (`deny.go`): the team group writes in `Team`, and the contractor, a member of the team, gets a deny on `Team/Finance` for the length of an audit (`Effect: acl.Deny` with an `ExpiresAt`). A colleague reads the budget in Finance; the contractor reads the roadmap in `Team` but not the budget, even with a read grant of their own on the budget file, because a deny that applies wins over every allow. A plugin without `WithDeny` refuses to store a deny (`invalid_argument`) but still honours the stored one. The test `TestDenyBeatsAllow` shows the same check passes once the deny is past its `ExpiresAt` or revoked.

## Files

| File | What it holds |
|---|---|
| `main.go` | The Store with both plugins, the grants, the checks and the HTTP routes |
| `custom.go` | The app's own levels, projects as containers, an expiring grant and `Prune` |
| `deny.go` | Deny grants with `acl.WithDeny`: everyone on the team except the contractor; code: [`newDenyStore`](ds:block?id=newdenystore-r7k3w9mf), [`everyoneButContractor`](ds:block?id=everyonebutcontractor-p2x8v5nk) |
| `main_test.go` | Runs the steps, and pins that folder grants need a hierarchy, that an invalid level model refuses every check, and that the deny is what refuses the contractor |

## Next

- The guide: [Permissions](../../docs/plugins/acl.md); the plugin's [README](../../records/acl/README.md).
- Related examples: [records-folders](../records-folders) for the folder tree, [records-share](../records-share) for links for people without an account, [records-http](../records-http) for the routes.
