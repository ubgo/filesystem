-- The paginated listings of the records tables on postgres, for sqlc, beside
-- the shared ../queries.sql. They live per engine because sqlc parses
-- LIMIT differently on each (MySQL takes only a bare ?).
-- Reads only, like ../queries.sql.

-- name: ListLiveFiles :many
-- The first page of a scope's live files, newest first: the order of
-- records.Store.Files.
SELECT id, scope, disk, storage_key, filename, content_type, size, sha256, visibility, metadata, created_at, deleted_at, uploaded_by, status, status_detail, source_channel, source_client, source_ip, expires_at, view_once, checksums
FROM fs_files
WHERE scope = sqlc.arg(scope) AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: ListLiveFilesAfter :many
-- The next page: the files after the last one of the previous page (its
-- created_at and id), keyset-paginated like records.Store.Files.
SELECT id, scope, disk, storage_key, filename, content_type, size, sha256, visibility, metadata, created_at, deleted_at, uploaded_by, status, status_detail, source_channel, source_client, source_ip, expires_at, view_once, checksums
FROM fs_files
WHERE scope = sqlc.arg(scope) AND deleted_at IS NULL
  AND (created_at < sqlc.arg(after_created_at) OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);
