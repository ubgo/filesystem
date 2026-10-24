-- The records tables on MySQL, as records.Store.Migrate leaves them.
-- For sqlc only: create the tables with Store.Migrate, never from this
-- file. schema_test.go fails when the Store's tables and this file differ.

CREATE TABLE fs_files (
    id VARCHAR(191) NOT NULL PRIMARY KEY,
    scope VARCHAR(191) NOT NULL,
    disk VARCHAR(64) NOT NULL,
    storage_key VARCHAR(1024) NOT NULL,
    filename VARCHAR(255) NOT NULL,
    content_type VARCHAR(255) NOT NULL,
    size BIGINT NOT NULL,
    sha256 VARCHAR(64) NOT NULL,
    visibility VARCHAR(64) NOT NULL,
    metadata MEDIUMTEXT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    deleted_at DATETIME(6) NULL,
    uploaded_by VARCHAR(191) NOT NULL DEFAULT '',
    status VARCHAR(64) NOT NULL DEFAULT 'ready',
    status_detail VARCHAR(255) NOT NULL DEFAULT '',
    source_channel VARCHAR(64) NOT NULL DEFAULT '',
    source_client VARCHAR(255) NOT NULL DEFAULT '',
    source_ip VARCHAR(64) NOT NULL DEFAULT '',
    expires_at DATETIME(6) NULL,
    view_once BIGINT NOT NULL DEFAULT 0,
    checksums MEDIUMTEXT NULL,
    KEY fs_files_scope_created (scope, created_at, id),
    KEY fs_files_scope_sha256 (scope, sha256),
    KEY fs_files_scope_status (scope, status),
    KEY fs_files_object (disk, storage_key(255)),
    KEY fs_files_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE fs_attachments (
    id VARCHAR(191) NOT NULL PRIMARY KEY,
    scope VARCHAR(191) NOT NULL,
    file_id VARCHAR(191) NOT NULL,
    owner_type VARCHAR(64) NOT NULL,
    owner_id VARCHAR(191) NOT NULL,
    role VARCHAR(64) NOT NULL,
    position BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    UNIQUE KEY fs_attachments_owner (scope, owner_type, owner_id, role, file_id),
    KEY fs_attachments_file (file_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
