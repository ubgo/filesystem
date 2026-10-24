-- app_fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "app_fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMP NOT NULL, PRIMARY KEY ("owner", "name"));

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS "app_fs_files" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "disk" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "deleted_at" TIMESTAMP, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_files_scope_created" ON "app_fs_files" ("scope", "created_at", "id");
CREATE INDEX IF NOT EXISTS "app_fs_files_scope_sha256" ON "app_fs_files" ("scope", "sha256");
CREATE TABLE IF NOT EXISTS "app_fs_attachments" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, "position" INTEGER NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_attachments_owner" ON "app_fs_attachments" ("scope", "owner_type", "owner_id", "role", "file_id");
CREATE INDEX IF NOT EXISTS "app_fs_attachments_file" ON "app_fs_attachments" ("file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', '2143fbd06988478d98740079def0035ff136a543612c946a841b31c4bbc0025f', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0002_files_uploaded_by
ALTER TABLE "app_fs_files" ADD COLUMN "uploaded_by" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', '8b6bff2ac41107748943dfd018d886fb96cceaaa5a3758d6fca0e7facd929a88', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0003_files_status
ALTER TABLE "app_fs_files" ADD COLUMN "status" TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE "app_fs_files" ADD COLUMN "status_detail" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "app_fs_files_scope_status" ON "app_fs_files" ("scope", "status");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', 'd106945555e60c06e0505474d09317a1efe485b0a77d1cc84b539da93ba7c386', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0004_files_object
CREATE INDEX IF NOT EXISTS "app_fs_files_object" ON "app_fs_files" ("disk", "storage_key");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0004_files_object', '95ca2ca6b5c4950d6c52d6b87c5278c8f92a1c88d07435f4e60ed97994f82e3e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0005_files_source
ALTER TABLE "app_fs_files" ADD COLUMN "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_files" ADD COLUMN "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_files" ADD COLUMN "source_ip" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0005_files_source', 'e3be9e07c0b76f7060f7e8284fd59c21921d7cc8b6fa1ec6671f58ffa53e653e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0006_files_expiry
ALTER TABLE "app_fs_files" ADD COLUMN "expires_at" TIMESTAMP;
ALTER TABLE "app_fs_files" ADD COLUMN "view_once" INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS "app_fs_files_expires" ON "app_fs_files" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0006_files_expiry', 'e820ada8d8171f1c4a46f5fc0c8eef361df8b947dde8f65a7448577e5ca87586', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0007_files_checksums
ALTER TABLE "app_fs_files" ADD COLUMN "checksums" TEXT;
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0007_files_checksums', 'f8462cbc137209b7a3bbc489446456905ab47cb4f1e72d0df741d6b3ea7a133f', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0001_grants
CREATE TABLE IF NOT EXISTS "app_fs_acl_grants" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "subject" TEXT NOT NULL, "resource_kind" TEXT NOT NULL, "resource_id" TEXT NOT NULL, "level" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_acl_grants_resource" ON "app_fs_acl_grants" ("scope", "resource_kind", "resource_id", "subject");
CREATE INDEX IF NOT EXISTS "app_fs_acl_grants_subject" ON "app_fs_acl_grants" ("scope", "subject");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0001_grants', '86f62b2b7e2940992450449092282eb99b01c46e450e4908234ed972967010a5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0002_grants_expiry
ALTER TABLE "app_fs_acl_grants" ADD COLUMN "expires_at" TIMESTAMP;
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0002_grants_expiry', '1f6b2bae6ebe2c938d231edb2256ff73cf8b02e57f71deb5d862c473c6dbe21a', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- acl 0003_grants_effect
ALTER TABLE "app_fs_acl_grants" ADD COLUMN "effect" TEXT NOT NULL DEFAULT 'allow';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0003_grants_effect', '6d69477bc3f9a66473b4a3d391e74a5dcc6c579acddbd35cbb578410d5962bd3', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0001_audit
CREATE TABLE IF NOT EXISTS "app_fs_audit" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "at" TIMESTAMP NOT NULL, "actor" TEXT NOT NULL, "kind" TEXT NOT NULL, "file_id" TEXT NOT NULL, "attachment_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_audit_scope" ON "app_fs_audit" ("scope", "at", "id");
CREATE INDEX IF NOT EXISTS "app_fs_audit_file" ON "app_fs_audit" ("scope", "file_id", "at", "id");
CREATE INDEX IF NOT EXISTS "app_fs_audit_actor" ON "app_fs_audit" ("scope", "actor", "at", "id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0001_audit', '31cf8370508412823592148ad398481f3c780c1fc3bfba7852367d31432f73b1', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0002_audit_subject_detail
ALTER TABLE "app_fs_audit" ADD COLUMN "subject" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN "detail" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0002_audit_subject_detail', '91e056001fef46007f4469ded2d771e9d0513bcca9c690b5bb5bc69da08d4b83', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0003_audit_chain
ALTER TABLE "app_fs_audit" ADD COLUMN "seq" INTEGER NOT NULL DEFAULT 0;
ALTER TABLE "app_fs_audit" ADD COLUMN "prev_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN "hash" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "app_fs_audit_seq" ON "app_fs_audit" ("scope", "seq");
CREATE TABLE IF NOT EXISTS "app_fs_audit_chain" ("scope" TEXT NOT NULL, "seq" INTEGER NOT NULL, "head" TEXT NOT NULL, "anchor_seq" INTEGER NOT NULL, "anchor_hash" TEXT NOT NULL, "since" TIMESTAMP NOT NULL, "anchor_at" TIMESTAMP NOT NULL, "mac" TEXT NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0003_audit_chain', '64bba88309e894453df416d5cd878bc97ad53b696e921702430698ada4115756', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- audit 0004_audit_keys_source
ALTER TABLE "app_fs_audit" ADD COLUMN "key_id" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN "source_channel" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN "source_client" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit" ADD COLUMN "source_ip" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_audit_chain" ADD COLUMN "key_id" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0004_audit_keys_source', 'd65b2f7f4458455eaf4f9236820511e6534dbdeea7fae26d5324f12421f870c5', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0001_egress_daily
CREATE TABLE IF NOT EXISTS "app_fs_egress_daily" ("scope" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "requests" INTEGER NOT NULL, "reported_bytes" INTEGER NOT NULL, "reported_requests" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "day"));
CREATE INDEX IF NOT EXISTS "app_fs_egress_daily_day" ON "app_fs_egress_daily" ("day", "scope");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0001_egress_daily', '5c46e0381cbf6afa0276cbaeb76421ba800a3a61f84b04579a81b25e7514d675', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- egress 0002_egress_subjects
CREATE TABLE IF NOT EXISTS "app_fs_egress_subjects" ("scope" TEXT NOT NULL, "kind" TEXT NOT NULL, "subject" TEXT NOT NULL, "day" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "requests" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "kind", "subject", "day"));
CREATE INDEX IF NOT EXISTS "app_fs_egress_subjects_day" ON "app_fs_egress_subjects" ("day");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0002_egress_subjects', '7726e404095d874eed9106dd5029eb1dea8a7f70dc598b786f51dc590e4cab91', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0001_folders
CREATE TABLE IF NOT EXISTS "app_fs_folders" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "parent_id" TEXT NOT NULL, "name" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_folders_sibling" ON "app_fs_folders" ("scope", "parent_id", "name");
CREATE TABLE IF NOT EXISTS "app_fs_folder_files" ("file_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, PRIMARY KEY ("file_id"));
CREATE INDEX IF NOT EXISTS "app_fs_folder_files_folder" ON "app_fs_folder_files" ("scope", "folder_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0001_folders', '2a57b8933df24e9265299e851db4ff9c509a54abf48bf2c9de7de4913bce217b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0002_folder_locks
CREATE TABLE IF NOT EXISTS "app_fs_folder_locks" ("scope" TEXT NOT NULL, "version" INTEGER NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0002_folder_locks', '17178057c17aab5ff44cce9e59e415922bdb2fffb24a153b1fd632c8aa0796f0', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0003_folder_shortcuts
CREATE TABLE IF NOT EXISTS "app_fs_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "file_id" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "folder_id", "file_id"));
CREATE INDEX IF NOT EXISTS "app_fs_folder_shortcuts_file" ON "app_fs_folder_shortcuts" ("scope", "file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0003_folder_shortcuts', '1c942e94c46fb7c9737fa18d0a8d5846c5ec9fef5bb290370a542c491cf58e07', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- folders 0004_folder_folder_shortcuts
CREATE TABLE IF NOT EXISTS "app_fs_folder_folder_shortcuts" ("scope" TEXT NOT NULL, "folder_id" TEXT NOT NULL, "target_id" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "folder_id", "target_id"));
CREATE INDEX IF NOT EXISTS "app_fs_folder_folder_shortcuts_target" ON "app_fs_folder_folder_shortcuts" ("scope", "target_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0004_folder_folder_shortcuts', '0088c1b4613930375c1b56482c9f912c31383d48555ab2ebc32490f148f6114d', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0001_locks
CREATE TABLE IF NOT EXISTS "app_fs_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL, "locked_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "file_id"));
CREATE INDEX IF NOT EXISTS "app_fs_locks_expires" ON "app_fs_locks" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0001_locks', 'e4b12ca283f3e89684e9fb8db9252d067e35d99cd00bb2db6f9b3530fcf1e0c0', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0002_lock_tokens
ALTER TABLE "app_fs_locks" ADD COLUMN "token" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0002_lock_tokens', '4193637729defa315b670bca57268f7d7578eaeebe60f08cd9cc18cb58f06f54', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- locks 0003_shared_locks
CREATE TABLE IF NOT EXISTS "app_fs_shared_locks" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL, "locked_at" TIMESTAMP NOT NULL, "token" TEXT NOT NULL, PRIMARY KEY ("scope", "file_id", "owner"));
CREATE INDEX IF NOT EXISTS "app_fs_shared_locks_expires" ON "app_fs_shared_locks" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0003_shared_locks', 'e29e79b2199c61541a7643979225040f8d11f8511f6c64ed7759cc256e33ef99', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0001_usage
CREATE TABLE IF NOT EXISTS "app_fs_quota_usage" ("scope" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "files" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0001_usage', 'd11bec64b5df492056f58968e1774e7a6b9fadcd26e23c6452e0b55a997105d9', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0002_uploader_usage
CREATE TABLE IF NOT EXISTS "app_fs_quota_uploader_usage" ("scope" TEXT NOT NULL, "uploader" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "files" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "uploader"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0002_uploader_usage', 'a60420c9e07df16028a16503256844e98780e882a2d09f77df5a64735e957d82', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0003_reservations
ALTER TABLE "app_fs_quota_usage" ADD COLUMN "reserved" INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS "app_fs_quota_reservations" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "expires_at" TIMESTAMP NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_quota_reservations_scope" ON "app_fs_quota_reservations" ("scope");
CREATE INDEX IF NOT EXISTS "app_fs_quota_reservations_expires" ON "app_fs_quota_reservations" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0003_reservations', 'cabf172d3f103de24fa5d3c1e187333710aad3ad96a4bd3c112d2f1b6ce7a7b3', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- quota 0004_dimension_usage
CREATE TABLE IF NOT EXISTS "app_fs_quota_dimension_usage" ("dimension" TEXT NOT NULL, "bytes" INTEGER NOT NULL, "files" INTEGER NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("dimension"));
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0004_dimension_usage', '561c92e1020a15c72068f74e96f6de673fdc5e513cf9ff0668b5cf58b98a5500', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0001_shares
CREATE TABLE IF NOT EXISTS "app_fs_shares" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "token_hash" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL, "max_downloads" INTEGER NOT NULL, "downloads" INTEGER NOT NULL, "revoked_at" TIMESTAMP, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_shares_token" ON "app_fs_shares" ("token_hash");
CREATE INDEX IF NOT EXISTS "app_fs_shares_file" ON "app_fs_shares" ("file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0001_shares', '8a1eed6dc5981deea0183f830ba8705a6a25fb0275212ef0a218c757d5a49530', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- share 0002_shares_access
ALTER TABLE "app_fs_shares" ADD COLUMN "password_hash" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_shares" ADD COLUMN "target_kind" TEXT NOT NULL DEFAULT 'file';
ALTER TABLE "app_fs_shares" ADD COLUMN "target_ref" TEXT NOT NULL DEFAULT '';
ALTER TABLE "app_fs_shares" ADD COLUMN "mode" TEXT NOT NULL DEFAULT 'download';
CREATE TABLE IF NOT EXISTS "app_fs_share_events" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "share_id" TEXT NOT NULL, "at" TIMESTAMP NOT NULL, "viewer" TEXT NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_share_events_share" ON "app_fs_share_events" ("share_id", "at", "id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0002_shares_access', '627132b5d104d1ce3420367443afb61e467b52cc39611b6b0461947d2860dae1', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- tags 0001_file_tags
CREATE TABLE IF NOT EXISTS "app_fs_file_tags" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "tag" TEXT NOT NULL, PRIMARY KEY ("file_id", "tag"));
CREATE INDEX IF NOT EXISTS "app_fs_file_tags_tag" ON "app_fs_file_tags" ("scope", "tag", "file_id");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('tags', '0001_file_tags', '64425b2762a777d4110bc0abae2675ad8335295d340b1441af5ddb332bb8a697', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- uploadsessions 0001_upload_sessions
CREATE TABLE IF NOT EXISTS "app_fs_upload_sessions" ("id" TEXT NOT NULL, "secret_hash" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "prefix" TEXT NOT NULL, "max_size" INTEGER NOT NULL, "allowed" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "expires_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_upload_sessions_expires" ON "app_fs_upload_sessions" ("expires_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('uploadsessions', '0001_upload_sessions', '53a9a5f8ef1d5967ec96dde0da626cc3be5967b36ef99936108f8c33d9d320a2', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0001_file_versions
CREATE TABLE IF NOT EXISTS "app_fs_file_versions" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "number" INTEGER NOT NULL, "disk" TEXT NOT NULL, "object_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "replaced_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "app_fs_file_versions_number" ON "app_fs_file_versions" ("file_id", "number");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0001_file_versions', '107195cc4a469c5ac38474dc222649017c43b11993cf8d5864664fc8a86d2e3c', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- versions 0002_file_versions_scope
CREATE INDEX IF NOT EXISTS "app_fs_file_versions_scope" ON "app_fs_file_versions" ("scope");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0002_file_versions_scope', '5bc8a0faaf92e78cdbaa18dd0f4d6a80a696875a9400d45c3ce249e466673335', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0001_webhooks
CREATE TABLE IF NOT EXISTS "app_fs_webhook_endpoints" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "url" TEXT NOT NULL, "secret" TEXT NOT NULL, "kinds" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_webhook_endpoints_scope" ON "app_fs_webhook_endpoints" ("scope");
CREATE TABLE IF NOT EXISTS "app_fs_webhook_deliveries" ("id" TEXT NOT NULL, "endpoint_id" TEXT NOT NULL, "scope" TEXT NOT NULL, "event_id" TEXT NOT NULL, "kind" TEXT NOT NULL, "payload" TEXT NOT NULL, "state" TEXT NOT NULL, "attempts" INTEGER NOT NULL, "next_at" TIMESTAMP NOT NULL, "last_error" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "updated_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "app_fs_webhook_deliveries_due" ON "app_fs_webhook_deliveries" ("state", "next_at");
CREATE INDEX IF NOT EXISTS "app_fs_webhook_deliveries_endpoint" ON "app_fs_webhook_deliveries" ("endpoint_id", "created_at");
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0001_webhooks', 'd03ebf0cd147ade4a1c025ee644fa39a84dac510632e78f87bd6465764ab469e', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0002_webhooks_global
ALTER TABLE "app_fs_webhook_endpoints" ADD COLUMN "is_global" INTEGER NOT NULL DEFAULT 0;
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0002_webhooks_global', '24dd64144ff5587df41d63185383fefe6938e32c1865eae75e29120ebce8e040', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- webhooks 0003_webhooks_headers
ALTER TABLE "app_fs_webhook_deliveries" ADD COLUMN "headers" TEXT NOT NULL DEFAULT '';
INSERT INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0003_webhooks_headers', 'd73c0f5201674a6613de7ad245180c432e66ebd73d059e370fc3352b79d4c7b7', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

