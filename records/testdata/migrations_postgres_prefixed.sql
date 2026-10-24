-- app_fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "app_fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("owner", "name"));

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS "app_fs_files" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "disk" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" BIGINT NOT NULL, "sha256" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, "deleted_at" TIMESTAMPTZ, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_files_scope_created" ON "app_fs_files" ("scope", "created_at", "id");
CREATE INDEX IF NOT EXISTS "app_fs_files_scope_sha256" ON "app_fs_files" ("scope", "sha256");
CREATE TABLE IF NOT EXISTS "app_fs_attachments" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, "position" BIGINT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_attachments_owner" ON "app_fs_attachments" ("scope", "owner_type", "owner_id", "role", "file_id");
CREATE INDEX IF NOT EXISTS "app_fs_attachments_file" ON "app_fs_attachments" ("file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', 'a70146970e1dbfe9a62ba1dca9a6e67df33ea3b1b288dd59f0ae1a228fdc1a47', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0002_files_uploaded_by
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "uploaded_by" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', 'b517df1344a3e36f98ae463e33cde203d8bae565642a8f87e18781f289fbadf8', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0003_files_status
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "status" TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "status_detail" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "app_fs_files_scope_status" ON "app_fs_files" ("scope", "status");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', '038ea9bae47f66cc5b986aed41ac68bc9ed9b5d9275a02b33470e33bba5e14f9', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0004_files_object
CREATE INDEX IF NOT EXISTS "app_fs_files_object" ON "app_fs_files" ("disk", "storage_key");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0004_files_object', '95ca2ca6b5c4950d6c52d6b87c5278c8f92a1c88d07435f4e60ed97994f82e3e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0005_files_source
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "source_ip" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0005_files_source', '254912423bfb0d32a8446ccc7fe35be435d4ffb2f76fa9c6162234d89849ebe0', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0006_files_expiry
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "expires_at" TIMESTAMPTZ;
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "view_once" BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS "app_fs_files_expires" ON "app_fs_files" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0006_files_expiry', '7b11d53babb8d0afad0c463fa0a48d6a08b7a6a7f0ea077ae00219c59f60094e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0007_files_checksums
ALTER TABLE "app_fs_files" ADD COLUMN IF NOT EXISTS "checksums" TEXT;
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0007_files_checksums', 'e464ebaf7b8a276463c0c35be372545371883a646a465c9825a8297a3c298608', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0001_grants
CREATE TABLE IF NOT EXISTS "app_fs_acl_grants" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "subject" TEXT NOT NULL, "resource_kind" TEXT NOT NULL, "resource_id" TEXT NOT NULL, "level" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_acl_grants_resource" ON "app_fs_acl_grants" ("scope", "resource_kind", "resource_id", "subject");
CREATE INDEX IF NOT EXISTS "app_fs_acl_grants_subject" ON "app_fs_acl_grants" ("scope", "subject");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0001_grants', 'e0cdb8a528c34acd6c50245da6e5c181f571439f9f82b28587dee477e1f7de79', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0002_grants_expiry
ALTER TABLE "app_fs_acl_grants" ADD COLUMN IF NOT EXISTS "expires_at" TIMESTAMPTZ;
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0002_grants_expiry', 'bcdd10ed10aa2d32eaf4fc9d1e355b2819ddef2e1a90b4a774eef774c25f75c2', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0003_grants_effect
ALTER TABLE "app_fs_acl_grants" ADD COLUMN IF NOT EXISTS "effect" TEXT NOT NULL DEFAULT 'allow';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0003_grants_effect', 'ff96c0bed2cde50a3e692201151c74cba3599bf7a7daca0dad803b7bb1270f4f', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0001_audit
CREATE TABLE IF NOT EXISTS "app_fs_audit" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "at" TIMESTAMPTZ NOT NULL, "actor" TEXT NOT NULL, "kind" TEXT NOT NULL, "file_id" TEXT NOT NULL, "attachment_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_audit_scope" ON "app_fs_audit" ("scope", "at", "id");
CREATE INDEX IF NOT EXISTS "app_fs_audit_file" ON "app_fs_audit" ("scope", "file_id", "at", "id");
CREATE INDEX IF NOT EXISTS "app_fs_audit_actor" ON "app_fs_audit" ("scope", "actor", "at", "id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0001_audit', 'ad90033298823abda780a3dbd314283c1d51db859f38eeab265ce4385033fbe2', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0002_audit_subject_detail
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "subject" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "detail" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0002_audit_subject_detail', 'ba422dc23ec7e33ea644c32fd148bcd83f9a205dfdbc7984dc8c23709d9107a8', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0003_audit_chain
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "seq" BIGINT NOT NULL DEFAULT 0;
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "prev_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "hash" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "app_fs_audit_seq" ON "app_fs_audit" ("scope", "seq");
CREATE TABLE IF NOT EXISTS "app_fs_audit_chain" ("scope" TEXT NOT NULL, "seq" BIGINT NOT NULL, "head" TEXT NOT NULL, "anchor_seq" BIGINT NOT NULL, "anchor_hash" TEXT NOT NULL, "since" TIMESTAMPTZ NOT NULL, "anchor_at" TIMESTAMPTZ NOT NULL, "mac" TEXT NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0003_audit_chain', 'bb017cb1e4c04e4f3da2c7dcfac6ea52285d12ef2d2e07a23e2831d1d0cf9221', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0004_audit_keys_source
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "key_id" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN IF NOT EXISTS "source_ip" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit_chain" ADD COLUMN IF NOT EXISTS "key_id" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0004_audit_keys_source', 'd3baedb2ebf77869513df631b6f87a6fce62750a1c175fd0b74cf7b1ff6a8165', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0001_egress_daily
CREATE TABLE IF NOT EXISTS "app_fs_egress_daily" ("scope" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "requests" BIGINT NOT NULL, "reported_bytes" BIGINT NOT NULL, "reported_requests" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "day"));
CREATE INDEX IF NOT EXISTS "app_fs_egress_daily_day" ON "app_fs_egress_daily" ("day", "scope");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0001_egress_daily', 'e50538a0a807008473f8d4aae4438c39089600a8fc9ed2f6c2f77232318b73ae', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0002_egress_subjects
CREATE TABLE IF NOT EXISTS "app_fs_egress_subjects" ("scope" TEXT NOT NULL, "kind" TEXT NOT NULL, "subject" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "requests" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "kind", "subject", "day"));
CREATE INDEX IF NOT EXISTS "app_fs_egress_subjects_day" ON "app_fs_egress_subjects" ("day");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0002_egress_subjects', '4f21626550cae05b58a085b05547b541e638d68bd317467e8acec222078d5203', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0001_folders
CREATE TABLE IF NOT EXISTS "app_fs_folders" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "parent_id" TEXT NOT NULL, "name" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_folders_sibling" ON "app_fs_folders" ("scope", "parent_id", "name");
CREATE TABLE IF NOT EXISTS "app_fs_folder_files" ("file_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, PRIMARY KEY ("file_id"));
CREATE INDEX IF NOT EXISTS "app_fs_folder_files_folder" ON "app_fs_folder_files" ("scope", "folder_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0001_folders', 'f94a510a9fbcafe6dff74c89438c0d241cf6f620bbc468cf265f32a884348c20', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0002_folder_locks
CREATE TABLE IF NOT EXISTS "app_fs_folder_locks" ("scope" TEXT NOT NULL, "version" BIGINT NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0002_folder_locks', 'fbb03973b1f8c5fe077102f87698ea15ec1f99e354f80f90bad853f859ee3aa4', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0003_folder_shortcuts
CREATE TABLE IF NOT EXISTS "app_fs_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "file_id" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "folder_id", "file_id"));
CREATE INDEX IF NOT EXISTS "app_fs_folder_shortcuts_file" ON "app_fs_folder_shortcuts" ("scope", "file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0003_folder_shortcuts', '74a83b02f24d4d12cd14b0c60685cc45bcad41016f1e7caa063b4ee3594800e5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0004_folder_folder_shortcuts
CREATE TABLE IF NOT EXISTS "app_fs_folder_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "target_id" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "folder_id", "target_id"));
CREATE INDEX IF NOT EXISTS "app_fs_folder_folder_shortcuts_target" ON "app_fs_folder_folder_shortcuts" ("scope", "target_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0004_folder_folder_shortcuts', 'caf916eb48ea66c0ac96ef1ba2f4d809a122198e9b0b67b5e2e2de3da26a638a', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0001_locks
CREATE TABLE IF NOT EXISTS "app_fs_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "locked_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "file_id"));
CREATE INDEX IF NOT EXISTS "app_fs_locks_expires" ON "app_fs_locks" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0001_locks', 'ff3f59bc3ab7d0819f732fddd1add618b421ac5cafd54a4f4ecd1cc3d5ff1f26', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0002_lock_tokens
ALTER TABLE "app_fs_locks" ADD COLUMN IF NOT EXISTS "token" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0002_lock_tokens', '357bbc204584aac8aafb0043c47cecdb75ada133257fe4072433f7b64be69f11', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0003_shared_locks
CREATE TABLE IF NOT EXISTS "app_fs_shared_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "locked_at" TIMESTAMPTZ NOT NULL, "token" TEXT NOT NULL, PRIMARY KEY ("scope", "file_id", "owner"));
CREATE INDEX IF NOT EXISTS "app_fs_shared_locks_expires" ON "app_fs_shared_locks" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0003_shared_locks', 'c9a58442ecb80efb261af97d70653db0d3bf38a9919dce0cc050214260a29886', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0001_usage
CREATE TABLE IF NOT EXISTS "app_fs_quota_usage" ("scope" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "files" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0001_usage', '38750e64597d4db531a19ef70bc60311f0de02be0fd17d2035d5eaf329f2a430', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0002_uploader_usage
CREATE TABLE IF NOT EXISTS "app_fs_quota_uploader_usage" ("scope" TEXT NOT NULL, "uploader" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "files" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "uploader"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0002_uploader_usage', '42c59494a606d1af8088fe6b43c0d3f9f54e5ebc6e692c8e4eddc2772c15a847', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0003_reservations
ALTER TABLE "app_fs_quota_usage" ADD COLUMN IF NOT EXISTS "reserved" BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS "app_fs_quota_reservations" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_quota_reservations_scope" ON "app_fs_quota_reservations" ("scope");
CREATE INDEX IF NOT EXISTS "app_fs_quota_reservations_expires" ON "app_fs_quota_reservations" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0003_reservations', '2b631fd4e19492409060c5047db2e54128540b48faa4adb11b06665bf3c87c38', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0004_dimension_usage
CREATE TABLE IF NOT EXISTS "app_fs_quota_dimension_usage" ("dimension" TEXT NOT NULL, "bytes" BIGINT NOT NULL, "files" BIGINT NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("dimension"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0004_dimension_usage', '451c09f8fc4f3f7aa220d95d8c11f191a09d76281cebf71cf3d3fe0e3408f4a5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0001_shares
CREATE TABLE IF NOT EXISTS "app_fs_shares" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "token_hash" TEXT NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, "max_downloads" BIGINT NOT NULL, "downloads" BIGINT NOT NULL, "revoked_at" TIMESTAMPTZ, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_shares_token" ON "app_fs_shares" ("token_hash");
CREATE INDEX IF NOT EXISTS "app_fs_shares_file" ON "app_fs_shares" ("file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0001_shares', '33f5f06e706368148973d76d42141eeef15bb7a7bb1f4bd4ef37b569be5e703b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0002_shares_access
ALTER TABLE "app_fs_shares" ADD COLUMN IF NOT EXISTS "password_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_shares" ADD COLUMN IF NOT EXISTS "target_kind" TEXT NOT NULL DEFAULT 'file';
ALTER TABLE "app_fs_shares" ADD COLUMN IF NOT EXISTS "target_ref" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_shares" ADD COLUMN IF NOT EXISTS "mode" TEXT NOT NULL DEFAULT 'download';
CREATE TABLE IF NOT EXISTS "app_fs_share_events" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "share_id" TEXT NOT NULL, "at" TIMESTAMPTZ NOT NULL, "viewer" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_share_events_share" ON "app_fs_share_events" ("share_id", "at", "id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0002_shares_access', 'cf8e0534ee014c76a298184d4eea964cc7d39997d405b1bf5d5edb9513ac5448', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- tags 0001_file_tags
CREATE TABLE IF NOT EXISTS "app_fs_file_tags" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "tag" TEXT NOT NULL, PRIMARY KEY ("file_id", "tag"));
CREATE INDEX IF NOT EXISTS "app_fs_file_tags_tag" ON "app_fs_file_tags" ("scope", "tag", "file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('tags', '0001_file_tags', '64425b2762a777d4110bc0abae2675ad8335295d340b1441af5ddb332bb8a697', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- uploadsessions 0001_upload_sessions
CREATE TABLE IF NOT EXISTS "app_fs_upload_sessions" ("id" TEXT NOT NULL, "secret_hash" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "prefix" TEXT NOT NULL, "max_size" BIGINT NOT NULL, "allowed" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, "expires_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_upload_sessions_expires" ON "app_fs_upload_sessions" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('uploadsessions', '0001_upload_sessions', 'b7e838ba78365beba2bcea8e7f3f7aa21a8823447d12389bb30ed89e05f0e864', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0001_file_versions
CREATE TABLE IF NOT EXISTS "app_fs_file_versions" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "number" BIGINT NOT NULL, "disk" TEXT NOT NULL, "object_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" BIGINT NOT NULL, "sha256" TEXT NOT NULL, "replaced_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_file_versions_number" ON "app_fs_file_versions" ("file_id", "number");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0001_file_versions', 'a2d255bbc86ba1de1459aa8841fafb3c9f2568bbf977170a94b9b3cb5dd8f178', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0002_file_versions_scope
CREATE INDEX IF NOT EXISTS "app_fs_file_versions_scope" ON "app_fs_file_versions" ("scope");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0002_file_versions_scope', '5bc8a0faaf92e78cdbaa18dd0f4d6a80a696875a9400d45c3ce249e466673335', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0001_webhooks
CREATE TABLE IF NOT EXISTS "app_fs_webhook_endpoints" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "url" TEXT NOT NULL, "secret" TEXT NOT NULL, "kinds" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_webhook_endpoints_scope" ON "app_fs_webhook_endpoints" ("scope");
CREATE TABLE IF NOT EXISTS "app_fs_webhook_deliveries" ("id" TEXT NOT NULL, "endpoint_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "event_id" TEXT NOT NULL, "kind" TEXT NOT NULL, "payload" TEXT NOT NULL, "state" TEXT NOT NULL, "attempts" BIGINT NOT NULL, "next_at" TIMESTAMPTZ NOT NULL, "last_error" TEXT NOT NULL, "created_at" TIMESTAMPTZ NOT NULL, "updated_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_webhook_deliveries_due" ON "app_fs_webhook_deliveries" ("state", "next_at");
CREATE INDEX IF NOT EXISTS "app_fs_webhook_deliveries_endpoint" ON "app_fs_webhook_deliveries" ("endpoint_id", "created_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0001_webhooks', '2ea3adc823c3723e15111c42adeb0d74eeaa7b94b0d6272e797c1b521a5fc01d', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0002_webhooks_global
ALTER TABLE "app_fs_webhook_endpoints" ADD COLUMN IF NOT EXISTS "is_global" BIGINT NOT NULL DEFAULT 0;
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0002_webhooks_global', 'a7fae17850daacc93b6cbe76dfcbbdd074f455ff04d90c291ca51584a51eff04', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0003_webhooks_headers
ALTER TABLE "app_fs_webhook_deliveries" ADD COLUMN IF NOT EXISTS "headers" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0003_webhooks_headers', '49f108f88f9adb358651eb1ce9860a5f917603c0d22bfa5482e387c384f02c76', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

