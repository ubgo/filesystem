-- app_fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS `app_fs_migrations` (`owner` VARCHAR(64) NOT NULL, `name` VARCHAR(191) NOT NULL, `checksum` VARCHAR(64) NOT NULL, `applied_at` DATETIME(6) NOT NULL, PRIMARY KEY (`owner`, `name`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS `app_fs_files` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `disk` VARCHAR(64) NOT NULL, `storage_key` VARCHAR(1024) NOT NULL, `filename` VARCHAR(255) NOT NULL, `content_type` VARCHAR(255) NOT NULL, `size` BIGINT NOT NULL, `sha256` VARCHAR(64) NOT NULL, `visibility` VARCHAR(64) NOT NULL, `metadata` MEDIUMTEXT NOT NULL, `created_at` DATETIME(6) NOT NULL, `deleted_at` DATETIME(6), PRIMARY KEY (`id`), INDEX `app_fs_files_scope_created` (`scope`, `created_at`, `id`), INDEX `app_fs_files_scope_sha256` (`scope`, `sha256`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
CREATE TABLE IF NOT EXISTS `app_fs_attachments` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `owner_type` VARCHAR(64) NOT NULL, `owner_id` VARCHAR(191) NOT NULL, `role` VARCHAR(64) NOT NULL, `position` BIGINT NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `app_fs_attachments_owner` UNIQUE (`scope`, `owner_type`, `owner_id`, `role`, `file_id`), INDEX `app_fs_attachments_file` (`file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', 'c0cd3b7d4d8effb9f58f87b99cd9bf46a58d267638b3788b72ebd64465c1214a', CURRENT_TIMESTAMP);

-- records 0002_files_uploaded_by
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'uploaded_by'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `uploaded_by` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', 'd289b9bbbceff6d34ff09d95aef7ba0be9c81ed9f2e39e563a546cc33f6d6ae0', CURRENT_TIMESTAMP);

-- records 0003_files_status
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'status'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `status` VARCHAR(64) NOT NULL DEFAULT ''ready''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'status_detail'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `status_detail` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND index_name = 'app_fs_files_scope_status'), 'DO 0', 'CREATE INDEX `app_fs_files_scope_status` ON `app_fs_files` (`scope`, `status`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', '4e703171ec540c336333401a560a6f94501754b79c7278b4249b2a79b3aa6be3', CURRENT_TIMESTAMP);

-- records 0004_files_object
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND index_name = 'app_fs_files_object'), 'DO 0', 'CREATE INDEX `app_fs_files_object` ON `app_fs_files` (`disk`, `storage_key`(255))');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0004_files_object', '3e081870abe474060738ac937a96a808e18480caf438c10d4fbf914df0418b8d', CURRENT_TIMESTAMP);

-- records 0005_files_source
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'source_channel'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `source_channel` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'source_client'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `source_client` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'source_ip'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `source_ip` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0005_files_source', 'fe8d7cf8a610bc9f38a9b20609d79f75e905376e60e39580681b82ed66b30e79', CURRENT_TIMESTAMP);

-- records 0006_files_expiry
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'expires_at'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `expires_at` DATETIME(6)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'view_once'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `view_once` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND index_name = 'app_fs_files_expires'), 'DO 0', 'CREATE INDEX `app_fs_files_expires` ON `app_fs_files` (`expires_at`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0006_files_expiry', 'de489b205893c629f09aa9e141f18aea222d86eef18fa55f335bc47466157291', CURRENT_TIMESTAMP);

-- records 0007_files_checksums
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_files' AND column_name = 'checksums'), 'DO 0', 'ALTER TABLE `app_fs_files` ADD COLUMN `checksums` MEDIUMTEXT');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0007_files_checksums', '78edd35dc026a8dfe6fe56e4641b6eae8745c32b9b91e2a81ff0dae5ea19adbc', CURRENT_TIMESTAMP);

-- acl 0001_grants
CREATE TABLE IF NOT EXISTS `app_fs_acl_grants` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `subject` VARCHAR(191) NOT NULL, `resource_kind` VARCHAR(64) NOT NULL, `resource_id` VARCHAR(191) NOT NULL, `level` VARCHAR(64) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `app_fs_acl_grants_resource` UNIQUE (`scope`, `resource_kind`, `resource_id`, `subject`), INDEX `app_fs_acl_grants_subject` (`scope`, `subject`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0001_grants', '93af8f5b57725fca5dd9e5cd4d59314842403dc68db0e02ea2d23d83e92d19be', CURRENT_TIMESTAMP);

-- acl 0002_grants_expiry
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_acl_grants' AND column_name = 'expires_at'), 'DO 0', 'ALTER TABLE `app_fs_acl_grants` ADD COLUMN `expires_at` DATETIME(6)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0002_grants_expiry', '74a072b7b0bf9d5b4ae870cd4eae997b9944feed703a1eff2fc93be1e92e7ed3', CURRENT_TIMESTAMP);

-- acl 0003_grants_effect
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_acl_grants' AND column_name = 'effect'), 'DO 0', 'ALTER TABLE `app_fs_acl_grants` ADD COLUMN `effect` VARCHAR(64) NOT NULL DEFAULT ''allow''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0003_grants_effect', 'd76832a9acd831436271c78d9ded9cee0041231c02452dc8d517fc53aa11bc85', CURRENT_TIMESTAMP);

-- audit 0001_audit
CREATE TABLE IF NOT EXISTS `app_fs_audit` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `at` DATETIME(6) NOT NULL, `actor` VARCHAR(191) NOT NULL, `kind` VARCHAR(64) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `attachment_id` VARCHAR(191) NOT NULL, `owner_type` VARCHAR(64) NOT NULL, `owner_id` VARCHAR(191) NOT NULL, `role` VARCHAR(64) NOT NULL, PRIMARY KEY (`id`), INDEX `app_fs_audit_scope` (`scope`, `at`, `id`), INDEX `app_fs_audit_file` (`scope`, `file_id`, `at`, `id`), INDEX `app_fs_audit_actor` (`scope`, `actor`, `at`, `id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0001_audit', '896aead2e7be910ee3da8d02d6e13aed8e376625b0484126c648aa1a02ccca92', CURRENT_TIMESTAMP);

-- audit 0002_audit_subject_detail
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'subject'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `subject` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'detail'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `detail` MEDIUMTEXT');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
UPDATE app_fs_audit SET detail = '' WHERE detail IS NULL;
ALTER TABLE `app_fs_audit` MODIFY COLUMN `detail` MEDIUMTEXT NOT NULL;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0002_audit_subject_detail', '0decd7f39e15cc2a8a3c13ba48afac7fea655112d7a1eb81a89b7018b976852c', CURRENT_TIMESTAMP);

-- audit 0003_audit_chain
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'seq'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `seq` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'prev_hash'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `prev_hash` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'hash'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `hash` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND index_name = 'app_fs_audit_seq'), 'DO 0', 'CREATE INDEX `app_fs_audit_seq` ON `app_fs_audit` (`scope`, `seq`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
CREATE TABLE IF NOT EXISTS `app_fs_audit_chain` (`scope` VARCHAR(191) NOT NULL, `seq` BIGINT NOT NULL, `head` VARCHAR(191) NOT NULL, `anchor_seq` BIGINT NOT NULL, `anchor_hash` VARCHAR(191) NOT NULL, `since` DATETIME(6) NOT NULL, `anchor_at` DATETIME(6) NOT NULL, `mac` VARCHAR(191) NOT NULL, PRIMARY KEY (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0003_audit_chain', '3d57dee7b0cdd4460bb3ea5cc74e08677e70bc7ebee3802f1a39a2973772686a', CURRENT_TIMESTAMP);

-- audit 0004_audit_keys_source
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'key_id'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `key_id` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'source_channel'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `source_channel` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'source_client'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `source_client` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit' AND column_name = 'source_ip'), 'DO 0', 'ALTER TABLE `app_fs_audit` ADD COLUMN `source_ip` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_audit_chain' AND column_name = 'key_id'), 'DO 0', 'ALTER TABLE `app_fs_audit_chain` ADD COLUMN `key_id` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0004_audit_keys_source', '8d92a8201d9cad540022ecf1a44e755db958c5143a31e492fc26944f982f8f68', CURRENT_TIMESTAMP);

-- egress 0001_egress_daily
CREATE TABLE IF NOT EXISTS `app_fs_egress_daily` (`scope` VARCHAR(191) NOT NULL, `day` VARCHAR(64) NOT NULL, `bytes` BIGINT NOT NULL, `requests` BIGINT NOT NULL, `reported_bytes` BIGINT NOT NULL, `reported_requests` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `day`), INDEX `app_fs_egress_daily_day` (`day`, `scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0001_egress_daily', 'dd3e545c5f9c42b84ade6a87087e5d8728d2e4beec944eff211e77137e82f882', CURRENT_TIMESTAMP);

-- egress 0002_egress_subjects
CREATE TABLE IF NOT EXISTS `app_fs_egress_subjects` (`scope` VARCHAR(191) NOT NULL, `kind` VARCHAR(64) NOT NULL, `subject` VARCHAR(191) NOT NULL, `day` VARCHAR(64) NOT NULL, `bytes` BIGINT NOT NULL, `requests` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `kind`, `subject`, `day`), INDEX `app_fs_egress_subjects_day` (`day`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0002_egress_subjects', '378daba5a74dc42673d4f487f7e33b8b873199f3270476e7be449cd761cb07cf', CURRENT_TIMESTAMP);

-- folders 0001_folders
CREATE TABLE IF NOT EXISTS `app_fs_folders` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `parent_id` VARCHAR(191) NOT NULL, `name` VARCHAR(255) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `app_fs_folders_sibling` UNIQUE (`scope`, `parent_id`, `name`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
CREATE TABLE IF NOT EXISTS `app_fs_folder_files` (`file_id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `folder_id` VARCHAR(191) NOT NULL, PRIMARY KEY (`file_id`), INDEX `app_fs_folder_files_folder` (`scope`, `folder_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0001_folders', 'e186a5de9d2b89dd487b4b4ce5f21d1d6a2d75a4d47d10698e642858080c43cc', CURRENT_TIMESTAMP);

-- folders 0002_folder_locks
CREATE TABLE IF NOT EXISTS `app_fs_folder_locks` (`scope` VARCHAR(191) NOT NULL, `version` BIGINT NOT NULL, PRIMARY KEY (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0002_folder_locks', '498150507dbb0bd554a9bc5f477e4c2401ea900f9e54353c9c58dd05e8c9c61f', CURRENT_TIMESTAMP);

-- folders 0003_folder_shortcuts
CREATE TABLE IF NOT EXISTS `app_fs_folder_shortcuts` (`scope` VARCHAR(191) NOT NULL, `folder_id` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `folder_id`, `file_id`), INDEX `app_fs_folder_shortcuts_file` (`scope`, `file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0003_folder_shortcuts', '6ac3dee0a855b1cfec0ee64be765e2c72daab1a1bd2c24e84d901efee1a2af9e', CURRENT_TIMESTAMP);

-- folders 0004_folder_folder_shortcuts
CREATE TABLE IF NOT EXISTS `app_fs_folder_folder_shortcuts` (`scope` VARCHAR(191) NOT NULL, `folder_id` VARCHAR(191) NOT NULL, `target_id` VARCHAR(191) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `folder_id`, `target_id`), INDEX `app_fs_folder_folder_shortcuts_target` (`scope`, `target_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0004_folder_folder_shortcuts', 'c12c1c4b992878cff7c03ee6b3626c49d9bbf5b9e2be77c06f4eccb249a8811e', CURRENT_TIMESTAMP);

-- locks 0001_locks
CREATE TABLE IF NOT EXISTS `app_fs_locks` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `owner` VARCHAR(191) NOT NULL, `expires_at` DATETIME(6) NOT NULL, `locked_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `file_id`), INDEX `app_fs_locks_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0001_locks', '5bcbb0a5664674590038761e7cc3cc599f8c4d71baa175ce59d2f91ebc0bcd08', CURRENT_TIMESTAMP);

-- locks 0002_lock_tokens
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_locks' AND column_name = 'token'), 'DO 0', 'ALTER TABLE `app_fs_locks` ADD COLUMN `token` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0002_lock_tokens', '21898aab988b439eb004bf6462b7da9e99d902e283151cf4c79bf4c92b4618ed', CURRENT_TIMESTAMP);

-- locks 0003_shared_locks
CREATE TABLE IF NOT EXISTS `app_fs_shared_locks` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `owner` VARCHAR(191) NOT NULL, `expires_at` DATETIME(6) NOT NULL, `locked_at` DATETIME(6) NOT NULL, `token` VARCHAR(191) NOT NULL, PRIMARY KEY (`scope`, `file_id`, `owner`), INDEX `app_fs_shared_locks_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0003_shared_locks', 'eb311a02ff2e2ef5bba8c8b0ecb4393b686891aeb3e3564abf2bc587c777a8c5', CURRENT_TIMESTAMP);

-- quota 0001_usage
CREATE TABLE IF NOT EXISTS `app_fs_quota_usage` (`scope` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `files` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0001_usage', '1794efc2bc342bccbe570b0de4371d95956999540891483cc8d97081a2acef5d', CURRENT_TIMESTAMP);

-- quota 0002_uploader_usage
CREATE TABLE IF NOT EXISTS `app_fs_quota_uploader_usage` (`scope` VARCHAR(191) NOT NULL, `uploader` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `files` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `uploader`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0002_uploader_usage', '4b3cc201e0b98c00aa7d0b3d07bb88aa06d8a27360d87e15bf8cf10ed72c8447', CURRENT_TIMESTAMP);

-- quota 0003_reservations
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_quota_usage' AND column_name = 'reserved'), 'DO 0', 'ALTER TABLE `app_fs_quota_usage` ADD COLUMN `reserved` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
CREATE TABLE IF NOT EXISTS `app_fs_quota_reservations` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `expires_at` DATETIME(6) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `app_fs_quota_reservations_scope` (`scope`), INDEX `app_fs_quota_reservations_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0003_reservations', 'e4c8541e44b8f5838caa92c05aa5b788ee892863e8ec9668a08f559eca5d0400', CURRENT_TIMESTAMP);

-- quota 0004_dimension_usage
CREATE TABLE IF NOT EXISTS `app_fs_quota_dimension_usage` (`dimension` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `files` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`dimension`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0004_dimension_usage', '401be0eb4adf04a9a3843591da0bc3b93764d9b9242efc57ca4dd852132dc1ce', CURRENT_TIMESTAMP);

-- share 0001_shares
CREATE TABLE IF NOT EXISTS `app_fs_shares` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `token_hash` VARCHAR(64) NOT NULL, `expires_at` DATETIME(6) NOT NULL, `max_downloads` BIGINT NOT NULL, `downloads` BIGINT NOT NULL, `revoked_at` DATETIME(6), `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `app_fs_shares_token` UNIQUE (`token_hash`), INDEX `app_fs_shares_file` (`file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0001_shares', '622119d56a846b6221ab2a70fd050b35d8d84395601ccb8ff41435bac55effc2', CURRENT_TIMESTAMP);

-- share 0002_shares_access
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_shares' AND column_name = 'password_hash'), 'DO 0', 'ALTER TABLE `app_fs_shares` ADD COLUMN `password_hash` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_shares' AND column_name = 'target_kind'), 'DO 0', 'ALTER TABLE `app_fs_shares` ADD COLUMN `target_kind` VARCHAR(64) NOT NULL DEFAULT ''file''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_shares' AND column_name = 'target_ref'), 'DO 0', 'ALTER TABLE `app_fs_shares` ADD COLUMN `target_ref` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_shares' AND column_name = 'mode'), 'DO 0', 'ALTER TABLE `app_fs_shares` ADD COLUMN `mode` VARCHAR(64) NOT NULL DEFAULT ''download''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
CREATE TABLE IF NOT EXISTS `app_fs_share_events` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `share_id` VARCHAR(191) NOT NULL, `at` DATETIME(6) NOT NULL, `viewer` VARCHAR(191) NOT NULL, PRIMARY KEY (`id`), INDEX `app_fs_share_events_share` (`share_id`, `at`, `id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0002_shares_access', '36f356eee465bb0630d1f54052744ad2ddfc505479c26db582ea8b47e8b85aad', CURRENT_TIMESTAMP);

-- tags 0001_file_tags
CREATE TABLE IF NOT EXISTS `app_fs_file_tags` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `tag` VARCHAR(64) NOT NULL, PRIMARY KEY (`file_id`, `tag`), INDEX `app_fs_file_tags_tag` (`scope`, `tag`, `file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('tags', '0001_file_tags', '10b7f44c59544fe0c6c7efb954dce0837683371fe2d79ab0190671b6a14a99f6', CURRENT_TIMESTAMP);

-- uploadsessions 0001_upload_sessions
CREATE TABLE IF NOT EXISTS `app_fs_upload_sessions` (`id` VARCHAR(191) NOT NULL, `secret_hash` VARCHAR(64) NOT NULL, `storage_key` VARCHAR(1024) NOT NULL, `prefix` VARCHAR(1024) NOT NULL, `max_size` BIGINT NOT NULL, `allowed` MEDIUMTEXT NOT NULL, `created_at` DATETIME(6) NOT NULL, `expires_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `app_fs_upload_sessions_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('uploadsessions', '0001_upload_sessions', 'b6f601822c3f74ca82ecd5f9972a7d6f08def90543d577688aae20f54bb66377', CURRENT_TIMESTAMP);

-- versions 0001_file_versions
CREATE TABLE IF NOT EXISTS `app_fs_file_versions` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `number` BIGINT NOT NULL, `disk` VARCHAR(64) NOT NULL, `object_key` VARCHAR(1024) NOT NULL, `filename` VARCHAR(255) NOT NULL, `content_type` VARCHAR(255) NOT NULL, `size` BIGINT NOT NULL, `sha256` VARCHAR(64) NOT NULL, `replaced_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `app_fs_file_versions_number` UNIQUE (`file_id`, `number`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0001_file_versions', 'a9e439f39d4b4114c34f75d2eedc9b88f86cd33d429129d2b8ac1fb498452923', CURRENT_TIMESTAMP);

-- versions 0002_file_versions_scope
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'app_fs_file_versions' AND index_name = 'app_fs_file_versions_scope'), 'DO 0', 'CREATE INDEX `app_fs_file_versions_scope` ON `app_fs_file_versions` (`scope`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0002_file_versions_scope', 'dd64fc3a14c979323172ca6df86ae461ed83af1ff120b364dd09180f927e7a8f', CURRENT_TIMESTAMP);

-- webhooks 0001_webhooks
CREATE TABLE IF NOT EXISTS `app_fs_webhook_endpoints` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `url` VARCHAR(1024) NOT NULL, `secret` MEDIUMTEXT NOT NULL, `kinds` MEDIUMTEXT NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `app_fs_webhook_endpoints_scope` (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
CREATE TABLE IF NOT EXISTS `app_fs_webhook_deliveries` (`id` VARCHAR(191) NOT NULL, `endpoint_id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `event_id` VARCHAR(191) NOT NULL, `kind` VARCHAR(64) NOT NULL, `payload` MEDIUMTEXT NOT NULL, `state` VARCHAR(64) NOT NULL, `attempts` BIGINT NOT NULL, `next_at` DATETIME(6) NOT NULL, `last_error` VARCHAR(255) NOT NULL, `created_at` DATETIME(6) NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `app_fs_webhook_deliveries_due` (`state`, `next_at`), INDEX `app_fs_webhook_deliveries_endpoint` (`endpoint_id`, `created_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0001_webhooks', 'abf2472049c32a6b7870a442e21fbe07b457cdcab5ea7039627901b17ab07d90', CURRENT_TIMESTAMP);

-- webhooks 0002_webhooks_global
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_webhook_endpoints' AND column_name = 'is_global'), 'DO 0', 'ALTER TABLE `app_fs_webhook_endpoints` ADD COLUMN `is_global` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0002_webhooks_global', '489e16c3dcbcf82ebfcbbeee1fc2953269a1d093ffe83f19afa1cce132e113b4', CURRENT_TIMESTAMP);

-- webhooks 0003_webhooks_headers
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_fs_webhook_deliveries' AND column_name = 'headers'), 'DO 0', 'ALTER TABLE `app_fs_webhook_deliveries` ADD COLUMN `headers` MEDIUMTEXT');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
UPDATE app_fs_webhook_deliveries SET headers = '' WHERE headers IS NULL;
ALTER TABLE `app_fs_webhook_deliveries` MODIFY COLUMN `headers` MEDIUMTEXT NOT NULL;
INSERT IGNORE INTO app_fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0003_webhooks_headers', '66b57d77dfd7d6f46a33cc31b948ab8629629e1f98478f3b3e9d6512a4031c60', CURRENT_TIMESTAMP);

