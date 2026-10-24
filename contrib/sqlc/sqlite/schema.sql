-- The records tables on SQLite, as records.Store.Migrate leaves them.
-- For sqlc only: create the tables with Store.Migrate, never from this
-- file. schema_test.go fails when the Store's tables and this file differ.

CREATE TABLE fs_files (
    id TEXT NOT NULL PRIMARY KEY,
    scope TEXT NOT NULL,
    disk TEXT NOT NULL,
    storage_key TEXT NOT NULL,
    filename TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    visibility TEXT NOT NULL,
    metadata TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    deleted_at TIMESTAMP NULL,
    uploaded_by TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ready',
    status_detail TEXT NOT NULL DEFAULT '',
    source_channel TEXT NOT NULL DEFAULT '',
    source_client TEXT NOT NULL DEFAULT '',
    source_ip TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMP NULL,
    view_once INTEGER NOT NULL DEFAULT 0,
    checksums TEXT NULL
);

CREATE INDEX fs_files_scope_created ON fs_files (scope, created_at, id);

CREATE INDEX fs_files_scope_sha256 ON fs_files (scope, sha256);

CREATE INDEX fs_files_scope_status ON fs_files (scope, status);

CREATE INDEX fs_files_object ON fs_files (disk, storage_key);

CREATE INDEX fs_files_expires ON fs_files (expires_at);

CREATE TABLE fs_attachments (
    id TEXT NOT NULL PRIMARY KEY,
    scope TEXT NOT NULL,
    file_id TEXT NOT NULL,
    owner_type TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    role TEXT NOT NULL,
    position INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE UNIQUE INDEX fs_attachments_owner ON fs_attachments (scope, owner_type, owner_id, role, file_id);

CREATE INDEX fs_attachments_file ON fs_attachments (file_id);
