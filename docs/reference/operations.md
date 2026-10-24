# Operations and events

Two closed sets name what happens in the library. Operation kinds (`filesystem.OpKind`) name every Disk operation; interceptors switch on them, errors carry them, logs and metrics label by them. Event kinds (`records.EventKind`) name every change the records Store makes; plugins hear them through `records.EventHooks`. This page lists both, read from `op.go`, the Disk's methods and the Store's `emit` calls.

## Operation kinds

`filesystem.OpKindValues` lists them all. Every Disk method that reaches a driver runs as exactly one operation, through the interceptor chain once; a portable fallback runs as the operations it is made of.

| Kind | Constant | Disk methods | `Op.Key` | Other fields | Mutating | Replayable |
|---|---|---|---|---|---|---|
| `stat` | `OpStat` | `Stat`, `Exists`, `Stat`/`Open` through `Disk.FS`, and each file `fshttp.ServeZip` sizes before it sends | the key | | no | yes |
| `read` | `OpRead` | `Read`, `ReadRange`, `ReadAll`, `TempFile` (after its `stat`), reading a file opened through `Disk.FS`, and each file `fshttp.ServeZip` streams | the key | `Streaming` | no | yes |
| `write` | `OpWrite` | `Write`, `WriteBytes` | the key | `Bytes` after success | yes | only when the body implements `io.Seeker` and can seek (not a pipe) |
| `delete` | `OpDelete` | `Delete` | the key | | yes | yes |
| `delete_prefix` | `OpDeletePrefix` | `DeletePrefix` | the directory | | yes | yes |
| `delete_keys` | `OpDeleteKeys` | `DeleteKeys` | empty | `Keys` | yes | yes |
| `list` | `OpList` | `ListPage`, each page of `List`, and `ReadDir` (or the directory probe) through `Disk.FS` | the directory | | no | yes |
| `copy` | `OpCopy` | `Copy`, `CopyAcross` on one disk | the source | `Target` | yes | yes |
| `move` | `OpMove` | `Move`, `MoveAcross` on one disk | the source | `Target` | yes | no |
| `set_visibility` | `OpSetVisibility` | `SetVisibility` | the key | | yes | yes |
| `presign` | `OpPresign` | `SignedURL` | the key | | only for `MethodPut` | yes |
| `public_url` | `OpPublicURL` | `PublicURL` through the driver | the key | | no | yes |
| `multipart_create` | `OpMultipartCreate` | `CreateMultipart` | the key | | yes | no |
| `multipart_upload_part` | `OpMultipartUpload` | `UploadPart` | the key | | yes | only when the body implements `io.Seeker` and can seek (not a pipe) |
| `multipart_presign_part` | `OpMultipartPresign` | `PresignPart` | the key | | yes | yes |
| `multipart_list_parts` | `OpMultipartListParts` | `ListParts` | the key | | no | yes |
| `multipart_complete` | `OpMultipartComplete` | `CompleteMultipart` | the key | | yes | no |
| `multipart_abort` | `OpMultipartAbort` | `AbortMultipart` | the key | | yes | yes |
| `presign_post` | `OpPresignPost` | `PresignPost` | the key or key prefix | | yes | yes |
| `sweep_abandoned` | `OpSweepAbandoned` | `SweepAbandoned` | empty | | yes | yes (the counts of every attempt add up) |
| `find_abandoned` | `OpFindAbandoned` | `FindAbandoned` | empty | | no | yes |
| `make_dir` | `OpMakeDir` | `MakeDir`, also where the driver has no `Dirs` and the call does nothing, so `ReadOnly` refuses it on every disk | the directory | | yes | yes |
| `dir_exists` | `OpDirExists` | `DirExists` on a driver with `Dirs` | the directory | | no | yes |

Columns:

- **Mutating** is `Op.Mutating`: the kind's `OpKind.Mutates()`, except that a `presign` is mutating only when it authorises a PUT. `filesystem.ReadOnly()` refuses exactly the mutating operations.
- **Replayable** is `Op.Replayable`: whether `layer.Retry` may run the call again. A write whose body is a one-shot stream cannot be replayed; a move and a multipart create or complete are never retried, because repeating them is unsafe.
- **Streaming** (`Op.Streaming`) marks a read, whose reader outlives the call: `layer.Timeout` leaves it alone and a `layer.Limit` slot is held only while the stream opens.
- **Attempt** (`Op.Attempt`) counts the calls started, from 1, for every kind.

Not operations: `Disk.PublicURL` on a disk with a configured base (`WithPublicURL`, `public_url=`) builds the URL without a driver call and runs no operation. `Disk.Capabilities`, `Name`, `DefaultVisibility` and `Close` are not operations either. A call refused before the driver (an invalid key, an invalid option, a missing capability) never enters the chain.

## Fallbacks as operations

| Call | Capability missing | Operations run |
|---|---|---|
| `Copy` | `Copy` | `stat` (source), `read` (source), `write` (target) |
| `Move` | `Move` | the `Copy` above (or `copy`), then `delete` (source) |
| `DeletePrefix` | `DeletePrefix` | `list` per page, `delete` per key |
| `DeleteKeys` | `DeleteKeys` | `delete` per key |
| `CopyAcross`, `MoveAcross` between two disks | always | `stat` and `read` on the source disk, `write` on the target, and for a move `delete` on the source |
| `Exists` | never | `stat` |
| `DirExists` | `Dirs` | `list` of the directory, with a limit of one entry; `DirExists("")` runs nothing |
| `Check` | never | `write`, `read`, `delete` of a probe under `.filesystem-check/` |

An interceptor keyed on `Mutating`, `Key`, `Target` and `Keys` therefore sees every effect of a fallback; one keyed on a single kind does not.

## Event kinds

`records.EventKindValues` lists them all. Each event is emitted inside the transaction of the change it describes, after the change; an `EventHooks` plugin's error rolls the change back. Every event has `Kind`, `Scope` and `At` (the Store's clock, UTC, microseconds).

| Kind | Constant | Emitted by | Fields set |
|---|---|---|---|
| `file.created` | `EventFileCreated` | `CreateFile`, `Upload` | `FileID` |
| `file.trashed` | `EventFileTrashed` | `Trash`; `SetAttachment` with `PreviousTrash` | `FileID` |
| `file.restored` | `EventFileRestored` | `Restore` | `FileID` |
| `file.replaced` | `EventFileReplaced` | `Replace`, `ReplaceObject` | `FileID` |
| `file.visibility_changed` | `EventFileVisibilityChanged` | `SetVisibility` | `FileID` |
| `file.status_changed` | `EventFileStatusChanged` | `SetStatus`, `PassScan`, `FailScan` | `FileID` |
| `file.released` | `EventFileReleased` | `PassScan`, `Scan` (a quarantined file whose scan passed), right after its `file.status_changed` | `FileID` |
| `file.moved` | `EventFileMoved` | `MoveFile` | `FileID`; `Detail` with `records.DetailFrom` and `records.DetailTo` |
| `file.purged` | `EventFilePurged` | `Purge`, and through it `SweepTrash`, `PurgeOwner` and `SetAttachment` with `PreviousPurge` | `FileID`; also stands for the file's attachments, which go with it without events of their own |
| `attachment.created` | `EventAttachmentCreated` | `Attach`; `SetAttachment` | `FileID`, `AttachmentID`, `Owner`, `Role` |
| `attachment.deleted` | `EventAttachmentDeleted` | `Detach`; `SetAttachment` (the previous file); `PurgeOwner` (each attachment) | `FileID`, `AttachmentID`, `Owner`, `Role` |
| `attachments.reordered` | `EventAttachmentsOrdered` | `Reorder` | `Owner`, `Role` |

No event is emitted for a change that changes nothing: trashing a file already in the trash, restoring a live file, detaching an unknown attachment, or setting the attachment a single role already holds.

A compound call emits the events of its parts in order. `PurgeOwner` of a user with one avatar emits `attachment.deleted`, then `file.purged`; `SetAttachment` replacing an avatar with `PreviousPurge` emits `attachment.deleted` (old), `attachment.created` (new), then `file.purged` (old file, when nothing else uses it).

Plugins report their own changes through `Store.Emit`, with kinds named `<plugin>.<action>` and the folder or link in `Subject`: `folders.created`, `folders.renamed`, `folders.moved`, `folders.deleted`, `folders.file_put`, `folders.shortcut_added`, `folders.shortcut_removed`; `tags.added`, `tags.removed`, `tags.renamed`, `tags.deleted`; `share.created`, `share.downloaded`, `share.revoked`. Each plugin's page lists its `Detail` keys. The `audit` plugin logs every kind, the Store's and the plugins', with the actor from `records.WithActor`.

## Next

- [Interceptors](../concepts/interceptors.md) and [Write an interceptor](../extending/write-an-interceptor.md): using `Op`.
- [Events and hooks](../records/events-and-hooks.md) and [Write a records plugin](../extending/write-a-records-plugin.md): using events.
- [Error codes](errors.md): the codes an operation fails with.
