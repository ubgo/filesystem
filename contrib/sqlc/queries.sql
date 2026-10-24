-- Read queries over the records tables, for sqlc, on every engine (the
-- sqlc.yaml of each engine names this file and the engine's pages.sql,
-- which holds the paginated listings: their LIMIT is spelled per engine). Reads only: every write goes
-- through records.Store, which runs plugin hooks, emits events and keeps
-- objects and rows consistent; a row written here would skip all of that.
-- queries_test.go runs every query against tables the Store created and
-- rows it wrote.

-- name: GetFile :one
-- One file of a scope, live or trashed.
SELECT id, scope, disk, storage_key, filename, content_type, size, sha256, visibility, metadata, created_at, deleted_at, uploaded_by, status, status_detail, source_channel, source_client, source_ip, expires_at, view_once, checksums
FROM fs_files
WHERE scope = sqlc.arg(scope) AND id = sqlc.arg(id);

-- name: ListFilesBySHA256 :many
-- The live files of a scope with one content digest (lowercase hex),
-- newest first: several files may share it, and the first row is what
-- records.Store.FileBySHA256 returns.
SELECT id, scope, disk, storage_key, filename, content_type, size, sha256, visibility, metadata, created_at, deleted_at, uploaded_by, status, status_detail, source_channel, source_client, source_ip, expires_at, view_once, checksums
FROM fs_files
WHERE scope = sqlc.arg(scope) AND sha256 = sqlc.arg(sha256) AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC;

-- name: ScopeTotals :one
-- What a scope stores, trashed files included, summed from its files.
SELECT COUNT(*) AS files, COALESCE(SUM(size), 0) AS bytes
FROM fs_files
WHERE scope = sqlc.arg(scope);

-- name: ListOwnerAttachments :many
-- The live files attached to an owner under a role, in position order:
-- what records.Store.Attachments returns.
SELECT a.id AS attachment_id, a.position, a.created_at AS attached_at,
       f.id, f.disk, f.storage_key, f.filename, f.content_type, f.size, f.visibility, f.created_at
FROM fs_attachments a
JOIN fs_files f ON f.id = a.file_id AND f.scope = a.scope
WHERE a.scope = sqlc.arg(scope) AND a.owner_type = sqlc.arg(owner_type) AND a.owner_id = sqlc.arg(owner_id) AND a.role = sqlc.arg(role)
  AND f.deleted_at IS NULL
ORDER BY a.position, a.id;

-- name: ListFileAttachments :many
-- Where one file is attached: every owner and role.
SELECT id, owner_type, owner_id, role, position, created_at
FROM fs_attachments
WHERE scope = sqlc.arg(scope) AND file_id = sqlc.arg(file_id)
ORDER BY owner_type, owner_id, role;
