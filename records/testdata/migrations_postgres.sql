-- fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("owner", "name"));

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS "fs_files" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "disk" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" BIGINT NOT NULL, "sha256" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, "deleted_at" TIMESTAMPTZ, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_files_scope_created" ON "fs_files" ("scope", "created_at", "id");
CREATE INDEX IF NOT EXISTS "fs_files_scope_sha256" ON "fs_files" ("scope", "sha256");
CREATE TABLE IF NOT EXISTS "fs_attachments" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, "position" BIGINT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_attachments_owner" ON "fs_attachments" ("scope", "owner_type", "owner_id", "role", "file_id");
CREATE INDEX IF NOT EXISTS "fs_attachments_file" ON "fs_attachments" ("file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', '13f3b1d451253b537cce15bf660d610a534f75f5a789e4b6322b9bf1bcd2e75a', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0002_files_uploaded_by
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "uploaded_by" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', 'ed2ea53e63e617e8356a05453b6440e45732c5bfda3050984ddffd930dd7f91e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0003_files_status
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "status" TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "status_detail" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "fs_files_scope_status" ON "fs_files" ("scope", "status");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', '212fe725ecf89a740e50be943bf687b475bc52fb7828fbdc68347f9b8475bc11', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0004_files_object
CREATE INDEX IF NOT EXISTS "fs_files_object" ON "fs_files" ("disk", "storage_key");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0004_files_object', '115d51f3178972205efa2fbbbf51ac592f6a8a7a9ee36fbe83131965e9e73478', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0005_files_source
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "source_ip" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0005_files_source', 'abfaf6c38565371cf5ced1baddaa4057b52d614b79a20fc30d47e03483e16741', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0006_files_expiry
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "expires_at" TIMESTAMPTZ;
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "view_once" BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS "fs_files_expires" ON "fs_files" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0006_files_expiry', '1657ed17f10fbc41fd9b443c0b08b74550bc98496cffd3b602f3ddfcab217925', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0007_files_checksums
ALTER TABLE "fs_files" ADD COLUMN IF NOT EXISTS "checksums" TEXT;
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0007_files_checksums', 'ac0fc3d60dc6a0079d112861c90c71d49b819f425f65ba3b4eb266c4cfdd6272', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0001_grants
CREATE TABLE IF NOT EXISTS "fs_acl_grants" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "subject" TEXT NOT NULL, "resource_kind" TEXT NOT NULL, "resource_id" TEXT NOT NULL, "level" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_acl_grants_resource" ON "fs_acl_grants" ("scope", "resource_kind", "resource_id", "subject");
CREATE INDEX IF NOT EXISTS "fs_acl_grants_subject" ON "fs_acl_grants" ("scope", "subject");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0001_grants', '58baeb75c29aae370de0a208312145430884699802fbb37fbb5f027505e25f7c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0002_grants_expiry
ALTER TABLE "fs_acl_grants" ADD COLUMN IF NOT EXISTS "expires_at" TIMESTAMPTZ;
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0002_grants_expiry', 'f1cd52277613b2d3822608e92395e135f84bf3e366e57e6a4c3ebc1603865f65', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0003_grants_effect
ALTER TABLE "fs_acl_grants" ADD COLUMN IF NOT EXISTS "effect" TEXT NOT NULL DEFAULT 'allow';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0003_grants_effect', '4f6dd1956d86cee32f31df09aac206d36fd8897dd199b3310140d5e6c058c4e2', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0001_audit
CREATE TABLE IF NOT EXISTS "fs_audit" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "at" TIMESTAMPTZ NOT NULL, "actor" TEXT NOT NULL, "kind" TEXT NOT NULL, "file_id" TEXT NOT NULL, "attachment_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_audit_scope" ON "fs_audit" ("scope", "at", "id");
CREATE INDEX IF NOT EXISTS "fs_audit_file" ON "fs_audit" ("scope", "file_id", "at", "id");
CREATE INDEX IF NOT EXISTS "fs_audit_actor" ON "fs_audit" ("scope", "actor", "at", "id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0001_audit', '5878e505ed8ec587e3361f9d017c00c136dda546c25717a21094c419ab306ebe', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0002_audit_subject_detail
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "subject" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "detail" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0002_audit_subject_detail', '2a1331f611663f832a0095144ae95e151930aa822cc658e93d3c0d68ccf840f0', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0003_audit_chain
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "seq" BIGINT NOT NULL DEFAULT 0;
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "prev_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "hash" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "fs_audit_seq" ON "fs_audit" ("scope", "seq");
CREATE TABLE IF NOT EXISTS "fs_audit_chain" ("scope" TEXT NOT NULL, "seq" BIGINT NOT NULL, "head" TEXT NOT NULL, "anchor_seq" BIGINT NOT NULL, "anchor_hash" TEXT NOT NULL, "since" TIMESTAMPTZ NOT NULL, "anchor_at" TIMESTAMPTZ NOT NULL, "mac" TEXT NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0003_audit_chain', 'd2764cf109aad6f8790c5b41a1b5c4dad5cbc05a2c713ce4e4a2adb7cd381187', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0004_audit_keys_source
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "key_id" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN IF NOT EXISTS "source_ip" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit_chain" ADD COLUMN IF NOT EXISTS "key_id" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0004_audit_keys_source', '35dc8f4a65fbd0c67dc77274c06132ba2ecf96a2346c3d61def5a1e6db93352c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0001_egress_daily
CREATE TABLE IF NOT EXISTS "fs_egress_daily" ("scope" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "requests" BIGINT NOT NULL, "reported_bytes" BIGINT NOT NULL, "reported_requests" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "day"));
CREATE INDEX IF NOT EXISTS "fs_egress_daily_day" ON "fs_egress_daily" ("day", "scope");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0001_egress_daily', '75eb197081a052941e9ee21da4c551a9f97ecf79baea820e889df0122e2401dc', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0002_egress_subjects
CREATE TABLE IF NOT EXISTS "fs_egress_subjects" ("scope" TEXT NOT NULL, "kind" TEXT NOT NULL, "subject" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "requests" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "kind", "subject", "day"));
CREATE INDEX IF NOT EXISTS "fs_egress_subjects_day" ON "fs_egress_subjects" ("day");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0002_egress_subjects', 'fccbfdd6c9693b67ec97281f0cd6270ce8dc60ba99a57d31205ed27bfc6c1865', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0001_folders
CREATE TABLE IF NOT EXISTS "fs_folders" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "parent_id" TEXT NOT NULL, "name" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_folders_sibling" ON "fs_folders" ("scope", "parent_id", "name");
CREATE TABLE IF NOT EXISTS "fs_folder_files" ("file_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, PRIMARY KEY ("file_id"));
CREATE INDEX IF NOT EXISTS "fs_folder_files_folder" ON "fs_folder_files" ("scope", "folder_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0001_folders', '3fa6bf676a479523825925a3d82710929516f1dab733cb089e40774c0be45216', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0002_folder_locks
CREATE TABLE IF NOT EXISTS "fs_folder_locks" ("scope" TEXT NOT NULL, "version" BIGINT NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0002_folder_locks', '657aee17785cbba85be8c4007b5d78b8eef9a90e24b875014a3ad8a322dab03b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0003_folder_shortcuts
CREATE TABLE IF NOT EXISTS "fs_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "file_id" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "folder_id", "file_id"));
CREATE INDEX IF NOT EXISTS "fs_folder_shortcuts_file" ON "fs_folder_shortcuts" ("scope", "file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0003_folder_shortcuts', 'a322faaa3e4d4cc2125f21182eb62a67d939da1e4c55bb73e2a33904cda1af8b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0004_folder_folder_shortcuts
CREATE TABLE IF NOT EXISTS "fs_folder_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "target_id" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "folder_id", "target_id"));
CREATE INDEX IF NOT EXISTS "fs_folder_folder_shortcuts_target" ON "fs_folder_folder_shortcuts" ("scope", "target_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0004_folder_folder_shortcuts', '41ba3c0361530708c70558e7ad6622166356dae106c89503cc66c1b1d15bbfce', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0001_locks
CREATE TABLE IF NOT EXISTS "fs_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "locked_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "file_id"));
CREATE INDEX IF NOT EXISTS "fs_locks_expires" ON "fs_locks" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0001_locks', '22d59b6a50e362694189d93d6e5689adb9d94196b7d4096fbeb6422b24caf861', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0002_lock_tokens
ALTER TABLE "fs_locks" ADD COLUMN IF NOT EXISTS "token" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0002_lock_tokens', '3d8824b3c32c08cd844b97feac323257b4acd62225857867237fa8ac79c515a2', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0003_shared_locks
CREATE TABLE IF NOT EXISTS "fs_shared_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "locked_at" TIMESTAMPTZ NOT NULL, "token" TEXT NOT NULL, PRIMARY KEY ("scope", "file_id", "owner"));
CREATE INDEX IF NOT EXISTS "fs_shared_locks_expires" ON "fs_shared_locks" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0003_shared_locks', '48a369537ff940eaf8151aa6095cf53f91fc9d513f218cd9e0566d68110b042c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0001_usage
CREATE TABLE IF NOT EXISTS "fs_quota_usage" ("scope" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "files" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0001_usage', 'a800ed66c4df4215e5a9c24b17bf34d975824cc282b831915faecaf6051e628e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0002_uploader_usage
CREATE TABLE IF NOT EXISTS "fs_quota_uploader_usage" ("scope" TEXT NOT NULL, "uploader" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "files" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "uploader"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0002_uploader_usage', '38232a506ba5485b18dc56e6de5e482ce4dbcbe04feeaa248b72a1b33c112377', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0003_reservations
ALTER TABLE "fs_quota_usage" ADD COLUMN IF NOT EXISTS "reserved" BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS "fs_quota_reservations" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_quota_reservations_scope" ON "fs_quota_reservations" ("scope");
CREATE INDEX IF NOT EXISTS "fs_quota_reservations_expires" ON "fs_quota_reservations" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0003_reservations', 'c4ef94b6bf4f53972807cf2e24cf8d35bc5ba1260f00e0b775e90b902ac7dc85', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0004_dimension_usage
CREATE TABLE IF NOT EXISTS "fs_quota_dimension_usage" ("dimension" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "files" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("dimension"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0004_dimension_usage', '7df210c39a9b343807315aea799499023f27f79e923793d5055bc19224eb5ae7', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0001_shares
CREATE TABLE IF NOT EXISTS "fs_shares" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "token_hash" TEXT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "max_downloads" BIGINT NOT NULL, "downloads" BIGINT NOT NULL, "revoked_at" TIMESTAMPTZ, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_shares_token" ON "fs_shares" ("token_hash");
CREATE INDEX IF NOT EXISTS "fs_shares_file" ON "fs_shares" ("file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0001_shares', '13e32e1f6b7ba591f63d9a3bb802213d3b4d1e83ae69aa8838e5e67643804af5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0002_shares_access
ALTER TABLE "fs_shares" ADD COLUMN IF NOT EXISTS "password_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_shares" ADD COLUMN IF NOT EXISTS "target_kind" TEXT NOT NULL DEFAULT 'file';
ALTER TABLE "fs_shares" ADD COLUMN IF NOT EXISTS "target_ref" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_shares" ADD COLUMN IF NOT EXISTS "mode" TEXT NOT NULL DEFAULT 'download';
CREATE TABLE IF NOT EXISTS "fs_share_events" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "share_id" TEXT NOT NULL, "at" TIMESTAMPTZ NOT NULL, "viewer" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_share_events_share" ON "fs_share_events" ("share_id", "at", "id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0002_shares_access', 'd62645ad0e29d3144f4df8446ffdeca4a32e07bacbd90d804134722ecba91e03', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- tags 0001_file_tags
CREATE TABLE IF NOT EXISTS "fs_file_tags" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "tag" TEXT NOT NULL, PRIMARY KEY ("file_id", "tag"));
CREATE INDEX IF NOT EXISTS "fs_file_tags_tag" ON "fs_file_tags" ("scope", "tag", "file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('tags', '0001_file_tags', '12064508959a6a92c6f5cd0272ba89f87683d690a89dcdbf2f6546f8b3f4a296', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- uploadsessions 0001_upload_sessions
CREATE TABLE IF NOT EXISTS "fs_upload_sessions" ("id" TEXT NOT NULL, "secret_hash" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "prefix" TEXT NOT NULL, "max_size" BIGINT NOT NULL, "allowed" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_upload_sessions_expires" ON "fs_upload_sessions" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('uploadsessions', '0001_upload_sessions', '4e318343724067e311160c99eae8b1c423a6f27e5ec26b76710110c2dde9dbc6', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0001_file_versions
CREATE TABLE IF NOT EXISTS "fs_file_versions" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "number" BIGINT NOT NULL, "disk" TEXT NOT NULL, "object_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" BIGINT NOT NULL, "sha256" TEXT NOT NULL, "replaced_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_file_versions_number" ON "fs_file_versions" ("file_id", "number");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0001_file_versions', 'b6365f5abbdd32c0ef6008a1a55cd8171667b78a76c4dbd9f64f1c6d9d15c043', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0002_file_versions_scope
CREATE INDEX IF NOT EXISTS "fs_file_versions_scope" ON "fs_file_versions" ("scope");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0002_file_versions_scope', '25734465af16cc645c402780c9e9b6342ee5e726add1a89463348beea7931d8d', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0001_webhooks
CREATE TABLE IF NOT EXISTS "fs_webhook_endpoints" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "url" TEXT NOT NULL, "secret" TEXT NOT NULL, "kinds" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_webhook_endpoints_scope" ON "fs_webhook_endpoints" ("scope");
CREATE TABLE IF NOT EXISTS "fs_webhook_deliveries" ("id" TEXT NOT NULL, "endpoint_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "event_id" TEXT NOT NULL, "kind" TEXT NOT NULL, "payload" TEXT NOT NULL, "state" TEXT NOT NULL, "attempts" BIGINT NOT NULL, "next_at" TIMESTAMPTZ NOT NULL, "last_error" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_webhook_deliveries_due" ON "fs_webhook_deliveries" ("state", "next_at");
CREATE INDEX IF NOT EXISTS "fs_webhook_deliveries_endpoint" ON "fs_webhook_deliveries" ("endpoint_id", "created_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0001_webhooks', 'ee0bb28cf5425a731f9dd1b746c4d64dbd56e94ffb6f37a2d925dd83e7ec6f99', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0002_webhooks_global
ALTER TABLE "fs_webhook_endpoints" ADD COLUMN IF NOT EXISTS "is_global" BIGINT NOT NULL DEFAULT 0;
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0002_webhooks_global', '96bec847a4e7abd84d7445816188963c36bfc765fff96a85816444b12d3ed9ae', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0003_webhooks_headers
ALTER TABLE "fs_webhook_deliveries" ADD COLUMN IF NOT EXISTS "headers" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0003_webhooks_headers', '2130dabeacc57ce63e42f6b478a84c7fe5cbeae158c71028e97bc6f89967f63f', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

