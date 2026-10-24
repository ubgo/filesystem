# github.com/ubgo/filesystem/contrib/ent

ent schema mixins and edges for the records tables (`fs_files`, `fs_attachments`), so an application whose data layer ent generates reads files with its own client. The package is `fsent`. The guide is [docs/integrations/ent.md](../../docs/integrations/ent.md).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/ent
```

It imports ent, the core and records, and no SQL driver (`deps_test.go` enforces it).

## Read only

Writes go through `records.Store`, which runs plugin hooks, emits events and keeps stored objects consistent with their rows. Both mixins install a hook that refuses every ent mutation with `ErrReadOnly` (import your generated `ent/runtime` package so it is registered), and the tables must never be migrated by ent: `Store.Migrate` owns them.

## Use

```go
type File struct{ ent.Schema }

func (File) Mixin() []ent.Mixin { return []ent.Mixin{fsent.FileMixin{}} }
func (File) Edges() []ent.Edge  { return []ent.Edge{fsent.FileAttachments(Attachment.Type)} }

type Attachment struct{ ent.Schema }

func (Attachment) Mixin() []ent.Mixin { return []ent.Mixin{fsent.AttachmentMixin{}} }
func (Attachment) Edges() []ent.Edge  { return []ent.Edge{fsent.AttachmentFile(File.Type)} }
```

A Store built with `records.WithTablePrefix` or `records.WithTableNames` keeps its tables and indexes under other names; give both mixins the same mapping in `Names` and they follow it:

```go
var names records.TableNamer = func(name string) string { return "app_" + name } // what WithTablePrefix("app_") does

func (File) Mixin() []ent.Mixin { return []ent.Mixin{fsent.FileMixin{Names: names}} }
```

## API

| Symbol | What it is |
|---|---|
| `FileMixin` | `fs_files`: table annotation, every column with the Store's names (`id`, `scope`, `disk`, `storage_key`, `filename`, `content_type`, `size`, `sha256`, `visibility`, `metadata`, `created_at`, `deleted_at`, `uploaded_by`, `status`, `status_detail`, `source_channel`, `source_client`, `source_ip`, `expires_at`, `view_once`, `checksums`), the Store's indexes (never created by ent), and the read-only hook. `Names` maps the table and index names as the Store's `records.TableNamer` does (nil: the default names) |
| `AttachmentMixin` | `fs_attachments`, likewise (`id`, `scope`, `file_id`, `owner_type`, `owner_id`, `role`, `position`, `created_at`) |
| `FileAttachments(attachment)` | The File schema's edge to its attachments (`EdgeAttachments`) |
| `AttachmentFile(file)` | The Attachment schema's edge to its file, on `file_id` (`EdgeFile`) |
| `ErrReadOnly` | The `permission` error of every mutation |

## Tests

`TestFieldsMatchTables` compares the mixin fields and indexes with the columns and indexes `Store.Migrate` creates on the engine under test; `TestNamesFollowTheStore` does the same for mixins given `Names` against a Store built with `records.WithTablePrefix`. `TestGenerateBuildAndRead` copies `testdata/entcheck` (the schemas above and a reader program) to a temporary module, generates the ent client with `go tool ent generate` (the generator is pinned in `go.mod` with a `tool` directive, beside a `golang.org/x/tools` new enough for Go 1.26), builds and runs it against a SQLite database the Store wrote, and compares its output with the Store. It skips only when go cannot download the generator, and says so; with a warm module cache it takes a few seconds.
