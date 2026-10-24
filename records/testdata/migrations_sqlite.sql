-- fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMP NOT NULL, PRIMARY KEY ("owner", "name"));

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS "fs_files" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "disk" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "deleted_at" TIMESTAMP, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_files_scope_created" ON "fs_files" ("scope", "created_at", "id");
CREATE INDEX IF NOT EXISTS "fs_files_scope_sha256" ON "fs_files" ("scope", "sha256");
CREATE TABLE IF NOT EXISTS "fs_attachments" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, "position" INTEGER NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_attachments_owner" ON "fs_attachments" ("scope", "owner_type", "owner_id", "role", "file_id");
CREATE INDEX IF NOT EXISTS "fs_attachments_file" ON "fs_attachments" ("file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', '9b05c159cf93d091de55e546575858878c817cb01e18fbac4f39b186b34c5aa9', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0002_files_uploaded_by
ALTER TABLE "fs_files" ADD COLUMN "uploaded_by" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', 'd8b47bc5d946ab9ec47b516a984a9744ba7d0db5a5c9b10db0de300c1101037b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0003_files_status
ALTER TABLE "fs_files" ADD COLUMN "status" TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE "fs_files" ADD COLUMN "status_detail" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "fs_files_scope_status" ON "fs_files" ("scope", "status");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', '5f66e178b335a533d7fd81c9cf0e251aba8b3e61d130b507c65b8baa91205363', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0004_files_object
CREATE INDEX IF NOT EXISTS "fs_files_object" ON "fs_files" ("disk", "storage_key");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0004_files_object', '115d51f3178972205efa2fbbbf51ac592f6a8a7a9ee36fbe83131965e9e73478', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0005_files_source
ALTER TABLE "fs_files" ADD COLUMN "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_files" ADD COLUMN "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_files" ADD COLUMN "source_ip" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0005_files_source', '83449c358efe2e441888e6dced9e7822d9b8c7fa77dbfc4b65fad53f439e86d1', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0006_files_expiry
ALTER TABLE "fs_files" ADD COLUMN "expires_at" TIMESTAMP;
ALTER TABLE "fs_files" ADD COLUMN "view_once" INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS "fs_files_expires" ON "fs_files" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0006_files_expiry', '2ca1e69c103331bcb4bce788071032f9ad2c1743d2f20deddd0650a3cfa38906', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0007_files_checksums
ALTER TABLE "fs_files" ADD COLUMN "checksums" TEXT;
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0007_files_checksums', 'da1c771a25fc0067bbd1e1bab3e0a2c6f85e983fe55985ef1df294a79c70aa80', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0001_grants
CREATE TABLE IF NOT EXISTS "fs_acl_grants" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "subject" TEXT NOT NULL, "resource_kind" TEXT NOT NULL, "resource_id" TEXT NOT NULL, "level" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_acl_grants_resource" ON "fs_acl_grants" ("scope", "resource_kind", "resource_id", "subject");
CREATE INDEX IF NOT EXISTS "fs_acl_grants_subject" ON "fs_acl_grants" ("scope", "subject");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0001_grants', '45386d3a8f1bbbff943fd7142481172ff3d17c642bbd1952fd09b1bb64aa470c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0002_grants_expiry
ALTER TABLE "fs_acl_grants" ADD COLUMN "expires_at" TIMESTAMP;
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0002_grants_expiry', '240edf5f114dc4e0077f80f8c27e48dbda554a7082afff8759422e77068dad69', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0003_grants_effect
ALTER TABLE "fs_acl_grants" ADD COLUMN "effect" TEXT NOT NULL DEFAULT 'allow';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0003_grants_effect', 'f6729ff46195f24e6e725aa2a6e62c26466619cba5685e551109166a67624cfe', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0001_audit
CREATE TABLE IF NOT EXISTS "fs_audit" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "at" TIMESTAMP NOT NULL, "actor" TEXT NOT NULL, "kind" TEXT NOT NULL, "file_id" TEXT NOT NULL, "attachment_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_audit_scope" ON "fs_audit" ("scope", "at", "id");
CREATE INDEX IF NOT EXISTS "fs_audit_file" ON "fs_audit" ("scope", "file_id", "at", "id");
CREATE INDEX IF NOT EXISTS "fs_audit_actor" ON "fs_audit" ("scope", "actor", "at", "id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0001_audit', '859fc5230e5d21d34ad827adb4f3ad6557787d7da205570cf69712d543342eeb', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0002_audit_subject_detail
ALTER TABLE "fs_audit" ADD COLUMN "subject" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN "detail" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0002_audit_subject_detail', '6a2b2821b775bb3fae19a0aef2e86560ca121f3bb52051e113e441c87231bcd6', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0003_audit_chain
ALTER TABLE "fs_audit" ADD COLUMN "seq" INTEGER NOT NULL DEFAULT 0;
ALTER TABLE "fs_audit" ADD COLUMN "prev_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN "hash" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "fs_audit_seq" ON "fs_audit" ("scope", "seq");
CREATE TABLE IF NOT EXISTS "fs_audit_chain" ("scope" TEXT NOT NULL, "seq" INTEGER NOT NULL, "head" TEXT NOT NULL, "anchor_seq" INTEGER NOT NULL, "anchor_hash" TEXT NOT NULL, "since" TIMESTAMP NOT NULL, "anchor_at" TIMESTAMP NOT NULL, "mac" TEXT NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0003_audit_chain', 'bbce9bc7904196a63611bc6a6747fbcd7bfb439ce86cd1ca8aed8e6eca02859c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0004_audit_keys_source
ALTER TABLE "fs_audit" ADD COLUMN "key_id" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit" ADD COLUMN "source_ip" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_audit_chain" ADD COLUMN "key_id" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0004_audit_keys_source', '1036fed31c6a1f366f8a912f71424e7ba20ff7002c1ee7a55426e17a266760de', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0001_egress_daily
CREATE TABLE IF NOT EXISTS "fs_egress_daily" ("scope" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "requests" INTEGER NOT NULL, "reported_bytes" INTEGER NOT NULL, "reported_requests" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "day"));
CREATE INDEX IF NOT EXISTS "fs_egress_daily_day" ON "fs_egress_daily" ("day", "scope");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0001_egress_daily', '9b56f4789d1a1dff8dc5099afe6c0035859d90033f726e323900f69fb0e3ca29', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0002_egress_subjects
CREATE TABLE IF NOT EXISTS "fs_egress_subjects" ("scope" TEXT NOT NULL, "kind" TEXT NOT NULL, "subject" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "requests" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "kind", "subject", "day"));
CREATE INDEX IF NOT EXISTS "fs_egress_subjects_day" ON "fs_egress_subjects" ("day");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0002_egress_subjects', 'a60f63f72571f18c4325f3979025f3ecb542b1829c94ae39d2591e0a82e203a5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0001_folders
CREATE TABLE IF NOT EXISTS "fs_folders" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "parent_id" TEXT NOT NULL, "name" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_folders_sibling" ON "fs_folders" ("scope", "parent_id", "name");
CREATE TABLE IF NOT EXISTS "fs_folder_files" ("file_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, PRIMARY KEY ("file_id"));
CREATE INDEX IF NOT EXISTS "fs_folder_files_folder" ON "fs_folder_files" ("scope", "folder_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0001_folders', '95a5a65b77e3c5628a92e94bbe7b6f8ff706b158f0a051e0b5af41d1e7679997', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0002_folder_locks
CREATE TABLE IF NOT EXISTS "fs_folder_locks" ("scope" TEXT NOT NULL, "version" INTEGER NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0002_folder_locks', '4ec0dc59de2d99f301157e9dd03c1dd6474c0720a666d7b508ceca3735dd6f39', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0003_folder_shortcuts
CREATE TABLE IF NOT EXISTS "fs_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "file_id" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "folder_id", "file_id"));
CREATE INDEX IF NOT EXISTS "fs_folder_shortcuts_file" ON "fs_folder_shortcuts" ("scope", "file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0003_folder_shortcuts', '3fb5d570954d8ead69122e3ad74e264d4a8079b70c3a17018513f2201164aed5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0004_folder_folder_shortcuts
CREATE TABLE IF NOT EXISTS "fs_folder_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "target_id" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "folder_id", "target_id"));
CREATE INDEX IF NOT EXISTS "fs_folder_folder_shortcuts_target" ON "fs_folder_folder_shortcuts" ("scope", "target_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0004_folder_folder_shortcuts', '9793bb77e53af94fe49d435a1061c017392ff20757ed0c78ad959025956c6faa', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0001_locks
CREATE TABLE IF NOT EXISTS "fs_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL, "locked_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "file_id"));
CREATE INDEX IF NOT EXISTS "fs_locks_expires" ON "fs_locks" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0001_locks', '2b32c72b0fcfa2fdc528894335737b7e7e94f4092e2f4b17e099accdc9dce702', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0002_lock_tokens
ALTER TABLE "fs_locks" ADD COLUMN "token" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0002_lock_tokens', '339a778ca0a28ad72c6e7e60fe6731c592d430430f7c0d292c40faf0378a6268', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0003_shared_locks
CREATE TABLE IF NOT EXISTS "fs_shared_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL, "locked_at" TIMESTAMP NOT NULL, "token" TEXT NOT NULL, PRIMARY KEY ("scope", "file_id", "owner"));
CREATE INDEX IF NOT EXISTS "fs_shared_locks_expires" ON "fs_shared_locks" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0003_shared_locks', '8338a462dcb09e8fedfa9ea84f337298656dd92da89ba5a7a59571c367e0f936', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0001_usage
CREATE TABLE IF NOT EXISTS "fs_quota_usage" ("scope" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "files" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0001_usage', '7e824d854baed4d0d8b5547d62aa32f56262d2104dc96572198e08464b7aab3d', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0002_uploader_usage
CREATE TABLE IF NOT EXISTS "fs_quota_uploader_usage" ("scope" TEXT NOT NULL, "uploader" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "files" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "uploader"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0002_uploader_usage', '80b88d40038a96db1cfd7d5794fcdf22356d8149e81c559783e2a829d905b721', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0003_reservations
ALTER TABLE "fs_quota_usage" ADD COLUMN "reserved" INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS "fs_quota_reservations" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "expires_at" TIMESTAMP NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_quota_reservations_scope" ON "fs_quota_reservations" ("scope");
CREATE INDEX IF NOT EXISTS "fs_quota_reservations_expires" ON "fs_quota_reservations" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0003_reservations', '2d52d74e320566e28c1a3e9e46c0889bcc2d700f4130d6bed0e69bdd84843890', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0004_dimension_usage
CREATE TABLE IF NOT EXISTS "fs_quota_dimension_usage" ("dimension" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "files" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("dimension"));
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0004_dimension_usage', 'bb4e4c3eaba308118e9cf5b4d46e9af4682ee0b8cb25e247c8556b5a8d7c3560', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0001_shares
CREATE TABLE IF NOT EXISTS "fs_shares" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "token_hash" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL, "max_downloads" INTEGER NOT NULL, "downloads" INTEGER NOT NULL, "revoked_at" TIMESTAMP, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_shares_token" ON "fs_shares" ("token_hash");
CREATE INDEX IF NOT EXISTS "fs_shares_file" ON "fs_shares" ("file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0001_shares', '709f2caf0f0a3ff74916e7db7a599c7fc2885d3542023da98d20a028a3494bbd', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0002_shares_access
ALTER TABLE "fs_shares" ADD COLUMN "password_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_shares" ADD COLUMN "target_kind" TEXT NOT NULL DEFAULT 'file';
ALTER TABLE "fs_shares" ADD COLUMN "target_ref" TEXT NOT NULL DEFAULT '';
ALTER TABLE "fs_shares" ADD COLUMN "mode" TEXT NOT NULL DEFAULT 'download';
CREATE TABLE IF NOT EXISTS "fs_share_events" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "share_id" TEXT NOT NULL, "at" TIMESTAMP NOT NULL, "viewer" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_share_events_share" ON "fs_share_events" ("share_id", "at", "id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0002_shares_access', '1ff00c55d0cf69d4dea29156dcca03b1e94a20e7ac62ad2a079a9d66ad683bdf', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- tags 0001_file_tags
CREATE TABLE IF NOT EXISTS "fs_file_tags" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "tag" TEXT NOT NULL, PRIMARY KEY ("file_id", "tag"));
CREATE INDEX IF NOT EXISTS "fs_file_tags_tag" ON "fs_file_tags" ("scope", "tag", "file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('tags', '0001_file_tags', '12064508959a6a92c6f5cd0272ba89f87683d690a89dcdbf2f6546f8b3f4a296', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- uploadsessions 0001_upload_sessions
CREATE TABLE IF NOT EXISTS "fs_upload_sessions" ("id" TEXT NOT NULL, "secret_hash" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "prefix" TEXT NOT NULL, "max_size" INTEGER NOT NULL, "allowed" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "expires_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_upload_sessions_expires" ON "fs_upload_sessions" ("expires_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('uploadsessions', '0001_upload_sessions', '0c8add895d04cf75fb954ca79bfd2e26232445f02be68f1d18b9e419e92e76bc', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0001_file_versions
CREATE TABLE IF NOT EXISTS "fs_file_versions" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "number" INTEGER NOT NULL, "disk" TEXT NOT NULL, "object_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "replaced_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_file_versions_number" ON "fs_file_versions" ("file_id", "number");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0001_file_versions', '1278af27a1691da7ed501788c2ae9efe202f0cfc685e188cd0b18f56a54fd7ef', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0002_file_versions_scope
CREATE INDEX IF NOT EXISTS "fs_file_versions_scope" ON "fs_file_versions" ("scope");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0002_file_versions_scope', '25734465af16cc645c402780c9e9b6342ee5e726add1a89463348beea7931d8d', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0001_webhooks
CREATE TABLE IF NOT EXISTS "fs_webhook_endpoints" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "url" TEXT NOT NULL, "secret" TEXT NOT NULL, "kinds" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_webhook_endpoints_scope" ON "fs_webhook_endpoints" ("scope");
CREATE TABLE IF NOT EXISTS "fs_webhook_deliveries" ("id" TEXT NOT NULL, "endpoint_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "event_id" TEXT NOT NULL, "kind" TEXT NOT NULL, "payload" TEXT NOT NULL, "state" TEXT NOT NULL, "attempts" INTEGER NOT NULL, "next_at" TIMESTAMP NOT NULL, "last_error" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_webhook_deliveries_due" ON "fs_webhook_deliveries" ("state", "next_at");
CREATE INDEX IF NOT EXISTS "fs_webhook_deliveries_endpoint" ON "fs_webhook_deliveries" ("endpoint_id", "created_at");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0001_webhooks', '950a92ce69c2a893d15349ec3c3b9baaefc4e8b83f29d25e0a4e70af758a02ef', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0002_webhooks_global
ALTER TABLE "fs_webhook_endpoints" ADD COLUMN "is_global" INTEGER NOT NULL DEFAULT 0;
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0002_webhooks_global', '4a0cd56b3a75cbbcffe27630538fd005868b93e272fdb8d1169d5b3c112cf54c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0003_webhooks_headers
ALTER TABLE "fs_webhook_deliveries" ADD COLUMN "headers" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0003_webhooks_headers', 'deccc43824f758282b548973342af9fb5d5f7b26dd9e987e9dd86fef9a8584cb', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

