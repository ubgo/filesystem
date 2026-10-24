-- fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS `fs_migrations` (`owner` VARCHAR(64) NOT NULL, `name` VARCHAR(191) NOT NULL, `checksum` VARCHAR(64) NOT NULL, `applied_at` DATETIME(6) NOT NULL, PRIMARY KEY (`owner`, `name`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS `fs_files` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `disk` VARCHAR(64) NOT NULL, `storage_key` VARCHAR(1024) NOT NULL, `filename` VARCHAR(255) NOT NULL, `content_type` VARCHAR(255) NOT NULL, `size` BIGINT NOT NULL, `sha256` VARCHAR(64) NOT NULL, `visibility` VARCHAR(64) NOT NULL, `metadata` MEDIUMTEXT NOT NULL, `created_at` DATETIME(6) NOT NULL, `deleted_at` DATETIME(6), PRIMARY KEY (`id`), INDEX `fs_files_scope_created` (`scope`, `created_at`, `id`), INDEX `fs_files_scope_sha256` (`scope`, `sha256`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
CREATE TABLE IF NOT EXISTS `fs_attachments` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `owner_type` VARCHAR(64) NOT NULL, `owner_id` VARCHAR(191) NOT NULL, `role` VARCHAR(64) NOT NULL, `position` BIGINT NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `fs_attachments_owner` UNIQUE (`scope`, `owner_type`, `owner_id`, `role`, `file_id`), INDEX `fs_attachments_file` (`file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', '319308d73b25743ce06288f53f4931592d0de581dff8fb3e278637bb2753cce4', CURRENT_TIMESTAMP);

-- records 0002_files_uploaded_by
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'uploaded_by'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `uploaded_by` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', '9fde6e4b9e1977aadba94552d4a2434a79d8ce240f61642a35dc3aec725c8092', CURRENT_TIMESTAMP);

-- records 0003_files_status
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'status'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `status` VARCHAR(64) NOT NULL DEFAULT ''ready''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'status_detail'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `status_detail` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND index_name = 'fs_files_scope_status'), 'DO 0', 'CREATE INDEX `fs_files_scope_status` ON `fs_files` (`scope`, `status`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', 'f885bff6a90bd8d1730e43aa781781bb18302d4eb64b9f639b867cade3c4a43d', CURRENT_TIMESTAMP);

-- records 0004_files_object
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND index_name = 'fs_files_object'), 'DO 0', 'CREATE INDEX `fs_files_object` ON `fs_files` (`disk`, `storage_key`(255))');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0004_files_object', 'd6827b6c707fd8e503d011ea5d76dd5571cc15f0bb2a0d519afa49cbb393e61e', CURRENT_TIMESTAMP);

-- records 0005_files_source
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'source_channel'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `source_channel` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'source_client'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `source_client` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'source_ip'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `source_ip` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0005_files_source', 'dca4dd35a2467e8afcc6a39400afed97c8980c8992c24e8f0351a4b715194598', CURRENT_TIMESTAMP);

-- records 0006_files_expiry
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'expires_at'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `expires_at` DATETIME(6)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'view_once'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `view_once` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND index_name = 'fs_files_expires'), 'DO 0', 'CREATE INDEX `fs_files_expires` ON `fs_files` (`expires_at`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0006_files_expiry', '6ac64628d4fc4dae8514c933c47c6d6e5fdd015f4b6d80c3b51932033ca80768', CURRENT_TIMESTAMP);

-- records 0007_files_checksums
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_files' AND column_name = 'checksums'), 'DO 0', 'ALTER TABLE `fs_files` ADD COLUMN `checksums` MEDIUMTEXT');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0007_files_checksums', '9ccf2c97068f70960349d0e0605ed51b0d4332f169f8d2dda3d57ccf99d4a644', CURRENT_TIMESTAMP);

-- acl 0001_grants
CREATE TABLE IF NOT EXISTS `fs_acl_grants` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `subject` VARCHAR(191) NOT NULL, `resource_kind` VARCHAR(64) NOT NULL, `resource_id` VARCHAR(191) NOT NULL, `level` VARCHAR(64) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `fs_acl_grants_resource` UNIQUE (`scope`, `resource_kind`, `resource_id`, `subject`), INDEX `fs_acl_grants_subject` (`scope`, `subject`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0001_grants', '455448cfc40483b63420c047f555444bcda68096970b9abb99fdc97cfdb2ce8a', CURRENT_TIMESTAMP);

-- acl 0002_grants_expiry
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_acl_grants' AND column_name = 'expires_at'), 'DO 0', 'ALTER TABLE `fs_acl_grants` ADD COLUMN `expires_at` DATETIME(6)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0002_grants_expiry', '4a16cc0531ea86a736d5081490a52fa4dfda0081526307c47f87f2475f67f052', CURRENT_TIMESTAMP);

-- acl 0003_grants_effect
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_acl_grants' AND column_name = 'effect'), 'DO 0', 'ALTER TABLE `fs_acl_grants` ADD COLUMN `effect` VARCHAR(64) NOT NULL DEFAULT ''allow''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('acl', '0003_grants_effect', 'bb46e21d597133dae4f8f0fb03f3c07069ad7770370cdaf24c4e15372f585fc5', CURRENT_TIMESTAMP);

-- audit 0001_audit
CREATE TABLE IF NOT EXISTS `fs_audit` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `at` DATETIME(6) NOT NULL, `actor` VARCHAR(191) NOT NULL, `kind` VARCHAR(64) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `attachment_id` VARCHAR(191) NOT NULL, `owner_type` VARCHAR(64) NOT NULL, `owner_id` VARCHAR(191) NOT NULL, `role` VARCHAR(64) NOT NULL, PRIMARY KEY (`id`), INDEX `fs_audit_scope` (`scope`, `at`, `id`), INDEX `fs_audit_file` (`scope`, `file_id`, `at`, `id`), INDEX `fs_audit_actor` (`scope`, `actor`, `at`, `id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0001_audit', '7c7dbd3d116f7d70b5be1d0dc3afb4f69399cc471360954c8d55cfa73f1d1620', CURRENT_TIMESTAMP);

-- audit 0002_audit_subject_detail
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'subject'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `subject` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'detail'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `detail` MEDIUMTEXT');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
UPDATE fs_audit SET detail = '' WHERE detail IS NULL;
ALTER TABLE `fs_audit` MODIFY COLUMN `detail` MEDIUMTEXT NOT NULL;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0002_audit_subject_detail', '926b02640eb673fdae95e88a961d01ef9b49d166b952cf4959886cc7c5cd9f0e', CURRENT_TIMESTAMP);

-- audit 0003_audit_chain
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'seq'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `seq` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'prev_hash'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `prev_hash` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'hash'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `hash` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND index_name = 'fs_audit_seq'), 'DO 0', 'CREATE INDEX `fs_audit_seq` ON `fs_audit` (`scope`, `seq`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
CREATE TABLE IF NOT EXISTS `fs_audit_chain` (`scope` VARCHAR(191) NOT NULL, `seq` BIGINT NOT NULL, `head` VARCHAR(191) NOT NULL, `anchor_seq` BIGINT NOT NULL, `anchor_hash` VARCHAR(191) NOT NULL, `since` DATETIME(6) NOT NULL, `anchor_at` DATETIME(6) NOT NULL, `mac` VARCHAR(191) NOT NULL, PRIMARY KEY (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0003_audit_chain', 'd40655f2b13b9b6dc3f525a8c29da13c435768aa8ce0c8c1a5e89c9dfd7f8fef', CURRENT_TIMESTAMP);

-- audit 0004_audit_keys_source
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'key_id'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `key_id` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'source_channel'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `source_channel` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'source_client'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `source_client` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit' AND column_name = 'source_ip'), 'DO 0', 'ALTER TABLE `fs_audit` ADD COLUMN `source_ip` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_audit_chain' AND column_name = 'key_id'), 'DO 0', 'ALTER TABLE `fs_audit_chain` ADD COLUMN `key_id` VARCHAR(64) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('audit', '0004_audit_keys_source', '2ca9f175d34e720c29540d4af0e3d535a6f40db263de8af52d82232db5740968', CURRENT_TIMESTAMP);

-- egress 0001_egress_daily
CREATE TABLE IF NOT EXISTS `fs_egress_daily` (`scope` VARCHAR(191) NOT NULL, `day` VARCHAR(64) NOT NULL, `bytes` BIGINT NOT NULL, `requests` BIGINT NOT NULL, `reported_bytes` BIGINT NOT NULL, `reported_requests` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `day`), INDEX `fs_egress_daily_day` (`day`, `scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0001_egress_daily', '50e6fd73df2ca13d6b933673525615f48d09e8b0fd1455d6dcae1c6d3d9f1be7', CURRENT_TIMESTAMP);

-- egress 0002_egress_subjects
CREATE TABLE IF NOT EXISTS `fs_egress_subjects` (`scope` VARCHAR(191) NOT NULL, `kind` VARCHAR(64) NOT NULL, `subject` VARCHAR(191) NOT NULL, `day` VARCHAR(64) NOT NULL, `bytes` BIGINT NOT NULL, `requests` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `kind`, `subject`, `day`), INDEX `fs_egress_subjects_day` (`day`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('egress', '0002_egress_subjects', '218d9306c0a9fd9b32f8245f1f72b963b6f8ac49fb53922a8988c7251b3eb93d', CURRENT_TIMESTAMP);

-- folders 0001_folders
CREATE TABLE IF NOT EXISTS `fs_folders` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `parent_id` VARCHAR(191) NOT NULL, `name` VARCHAR(255) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `fs_folders_sibling` UNIQUE (`scope`, `parent_id`, `name`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
CREATE TABLE IF NOT EXISTS `fs_folder_files` (`file_id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `folder_id` VARCHAR(191) NOT NULL, PRIMARY KEY (`file_id`), INDEX `fs_folder_files_folder` (`scope`, `folder_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0001_folders', '9c7f39d60ca00589bded11c1fac2e3c238468b1d4a6ea4d4a63a5dd832f754e4', CURRENT_TIMESTAMP);

-- folders 0002_folder_locks
CREATE TABLE IF NOT EXISTS `fs_folder_locks` (`scope` VARCHAR(191) NOT NULL, `version` BIGINT NOT NULL, PRIMARY KEY (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0002_folder_locks', '65a80f829bb7e257ffa656b68163ad5b8e70fe59f174211a14563196e03d6d79', CURRENT_TIMESTAMP);

-- folders 0003_folder_shortcuts
CREATE TABLE IF NOT EXISTS `fs_folder_shortcuts` (`scope` VARCHAR(191) NOT NULL, `folder_id` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `folder_id`, `file_id`), INDEX `fs_folder_shortcuts_file` (`scope`, `file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0003_folder_shortcuts', 'c7e851aeed92cefffe334a5a0867cd43b782940f1ca3edb30c8e379e3dbebd33', CURRENT_TIMESTAMP);

-- folders 0004_folder_folder_shortcuts
CREATE TABLE IF NOT EXISTS `fs_folder_folder_shortcuts` (`scope` VARCHAR(191) NOT NULL, `folder_id` VARCHAR(191) NOT NULL, `target_id` VARCHAR(191) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `folder_id`, `target_id`), INDEX `fs_folder_folder_shortcuts_target` (`scope`, `target_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('folders', '0004_folder_folder_shortcuts', '85a01247c44daa9e0f636869d14d19aaa0690226ab80ac9ab2063a51c72b3f9b', CURRENT_TIMESTAMP);

-- locks 0001_locks
CREATE TABLE IF NOT EXISTS `fs_locks` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `owner` VARCHAR(191) NOT NULL, `expires_at` DATETIME(6) NOT NULL, `locked_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `file_id`), INDEX `fs_locks_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0001_locks', '52d9816f40bad07863e7b149af54a6afb12620e963b932885657873a8cb0e3c2', CURRENT_TIMESTAMP);

-- locks 0002_lock_tokens
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_locks' AND column_name = 'token'), 'DO 0', 'ALTER TABLE `fs_locks` ADD COLUMN `token` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0002_lock_tokens', '04c1836bf5d1c198fe10c1404c1792f9b64e43140c01f2f43e61288fc137ad41', CURRENT_TIMESTAMP);

-- locks 0003_shared_locks
CREATE TABLE IF NOT EXISTS `fs_shared_locks` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `owner` VARCHAR(191) NOT NULL, `expires_at` DATETIME(6) NOT NULL, `locked_at` DATETIME(6) NOT NULL, `token` VARCHAR(191) NOT NULL, PRIMARY KEY (`scope`, `file_id`, `owner`), INDEX `fs_shared_locks_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('locks', '0003_shared_locks', 'b4887f9146beec2cba2fbe421bf1439fa52841e4a1e475c411150d2d0ccce962', CURRENT_TIMESTAMP);

-- quota 0001_usage
CREATE TABLE IF NOT EXISTS `fs_quota_usage` (`scope` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `files` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0001_usage', '77c2efb97725f5b1f7da32d8d7068a0ce6963fa85778dabf18b55ffb74f16ef7', CURRENT_TIMESTAMP);

-- quota 0002_uploader_usage
CREATE TABLE IF NOT EXISTS `fs_quota_uploader_usage` (`scope` VARCHAR(191) NOT NULL, `uploader` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `files` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `uploader`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0002_uploader_usage', 'd700ece1b9e4e4ffa88939e9a43cd9e6bef5066a1590acbc728ac4ce8674c2a3', CURRENT_TIMESTAMP);

-- quota 0003_reservations
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_quota_usage' AND column_name = 'reserved'), 'DO 0', 'ALTER TABLE `fs_quota_usage` ADD COLUMN `reserved` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
CREATE TABLE IF NOT EXISTS `fs_quota_reservations` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `expires_at` DATETIME(6) NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `fs_quota_reservations_scope` (`scope`), INDEX `fs_quota_reservations_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0003_reservations', 'f49808ec9736be27839e1d449801e4c74de26b00693bf37d4043d2625fd51279', CURRENT_TIMESTAMP);

-- quota 0004_dimension_usage
CREATE TABLE IF NOT EXISTS `fs_quota_dimension_usage` (`dimension` VARCHAR(191) NOT NULL, `bytes` BIGINT NOT NULL, `files` BIGINT NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`dimension`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('quota', '0004_dimension_usage', '964b575c5ec81eabc6057b06648af04c603d8db0351d55fa2ef82a680b67fa22', CURRENT_TIMESTAMP);

-- share 0001_shares
CREATE TABLE IF NOT EXISTS `fs_shares` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `token_hash` VARCHAR(64) NOT NULL, `expires_at` DATETIME(6) NOT NULL, `max_downloads` BIGINT NOT NULL, `downloads` BIGINT NOT NULL, `revoked_at` DATETIME(6), `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `fs_shares_token` UNIQUE (`token_hash`), INDEX `fs_shares_file` (`file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0001_shares', '61ff1e7b12c2256c087812433ca308e316a3e6d44dc304fb85a7b79e5fbd81c3', CURRENT_TIMESTAMP);

-- share 0002_shares_access
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_shares' AND column_name = 'password_hash'), 'DO 0', 'ALTER TABLE `fs_shares` ADD COLUMN `password_hash` VARCHAR(255) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_shares' AND column_name = 'target_kind'), 'DO 0', 'ALTER TABLE `fs_shares` ADD COLUMN `target_kind` VARCHAR(64) NOT NULL DEFAULT ''file''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_shares' AND column_name = 'target_ref'), 'DO 0', 'ALTER TABLE `fs_shares` ADD COLUMN `target_ref` VARCHAR(191) NOT NULL DEFAULT ''''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_shares' AND column_name = 'mode'), 'DO 0', 'ALTER TABLE `fs_shares` ADD COLUMN `mode` VARCHAR(64) NOT NULL DEFAULT ''download''');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
CREATE TABLE IF NOT EXISTS `fs_share_events` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `share_id` VARCHAR(191) NOT NULL, `at` DATETIME(6) NOT NULL, `viewer` VARCHAR(191) NOT NULL, PRIMARY KEY (`id`), INDEX `fs_share_events_share` (`share_id`, `at`, `id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('share', '0002_shares_access', '1afb51037989b6133fefd31949a9e1019d82ad500af66fb51c40d35a864f9ebf', CURRENT_TIMESTAMP);

-- tags 0001_file_tags
CREATE TABLE IF NOT EXISTS `fs_file_tags` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `tag` VARCHAR(64) NOT NULL, PRIMARY KEY (`file_id`, `tag`), INDEX `fs_file_tags_tag` (`scope`, `tag`, `file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('tags', '0001_file_tags', '8b21575c9acd4270919368b31538173407881b554ce85b6fadd4dff87b222ae7', CURRENT_TIMESTAMP);

-- uploadsessions 0001_upload_sessions
CREATE TABLE IF NOT EXISTS `fs_upload_sessions` (`id` VARCHAR(191) NOT NULL, `secret_hash` VARCHAR(64) NOT NULL, `storage_key` VARCHAR(1024) NOT NULL, `prefix` VARCHAR(1024) NOT NULL, `max_size` BIGINT NOT NULL, `allowed` MEDIUMTEXT NOT NULL, `created_at` DATETIME(6) NOT NULL, `expires_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `fs_upload_sessions_expires` (`expires_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('uploadsessions', '0001_upload_sessions', 'adb607f517f23fe8dc356ee569372556d1000d25bb275585c8f64281a7bd2428', CURRENT_TIMESTAMP);

-- versions 0001_file_versions
CREATE TABLE IF NOT EXISTS `fs_file_versions` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `number` BIGINT NOT NULL, `disk` VARCHAR(64) NOT NULL, `object_key` VARCHAR(1024) NOT NULL, `filename` VARCHAR(255) NOT NULL, `content_type` VARCHAR(255) NOT NULL, `size` BIGINT NOT NULL, `sha256` VARCHAR(64) NOT NULL, `replaced_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `fs_file_versions_number` UNIQUE (`file_id`, `number`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0001_file_versions', '79d3a4f88476a98aebce1dae91f32a9d0639fb43b720487ce79b8a5cdd83dbc0', CURRENT_TIMESTAMP);

-- versions 0002_file_versions_scope
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'fs_file_versions' AND index_name = 'fs_file_versions_scope'), 'DO 0', 'CREATE INDEX `fs_file_versions_scope` ON `fs_file_versions` (`scope`)');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('versions', '0002_file_versions_scope', '6d4dd764bff9bb885ceb052af3aba0672344809a47a3abc4359cc002162dabd4', CURRENT_TIMESTAMP);

-- webhooks 0001_webhooks
CREATE TABLE IF NOT EXISTS `fs_webhook_endpoints` (`id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `url` VARCHAR(1024) NOT NULL, `secret` MEDIUMTEXT NOT NULL, `kinds` MEDIUMTEXT NOT NULL, `created_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `fs_webhook_endpoints_scope` (`scope`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
CREATE TABLE IF NOT EXISTS `fs_webhook_deliveries` (`id` VARCHAR(191) NOT NULL, `endpoint_id` VARCHAR(191) NOT NULL, `scope` VARCHAR(191) NOT NULL, `event_id` VARCHAR(191) NOT NULL, `kind` VARCHAR(64) NOT NULL, `payload` MEDIUMTEXT NOT NULL, `state` VARCHAR(64) NOT NULL, `attempts` BIGINT NOT NULL, `next_at` DATETIME(6) NOT NULL, `last_error` VARCHAR(255) NOT NULL, `created_at` DATETIME(6) NOT NULL, `updated_at` DATETIME(6) NOT NULL, PRIMARY KEY (`id`), INDEX `fs_webhook_deliveries_due` (`state`, `next_at`), INDEX `fs_webhook_deliveries_endpoint` (`endpoint_id`, `created_at`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0001_webhooks', 'e80199c483c27bb18e52cc0925e2d16dc43396b00fb14b8113840738e3abc198', CURRENT_TIMESTAMP);

-- webhooks 0002_webhooks_global
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_webhook_endpoints' AND column_name = 'is_global'), 'DO 0', 'ALTER TABLE `fs_webhook_endpoints` ADD COLUMN `is_global` BIGINT NOT NULL DEFAULT 0');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0002_webhooks_global', '36cf43360569b3c99c771be3cca8fb7e42a37c53b15f3a3890fe648150ed4ba3', CURRENT_TIMESTAMP);

-- webhooks 0003_webhooks_headers
SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'fs_webhook_deliveries' AND column_name = 'headers'), 'DO 0', 'ALTER TABLE `fs_webhook_deliveries` ADD COLUMN `headers` MEDIUMTEXT');
PREPARE fs_ddl FROM @fs_ddl;
EXECUTE fs_ddl;
DEALLOCATE PREPARE fs_ddl;
UPDATE fs_webhook_deliveries SET headers = '' WHERE headers IS NULL;
ALTER TABLE `fs_webhook_deliveries` MODIFY COLUMN `headers` MEDIUMTEXT NOT NULL;
INSERT IGNORE INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('webhooks', '0003_webhooks_headers', 'e18b6f47d125350ab4cf3fc3821590aca1434cd4ea70c4127feca020b790b121', CURRENT_TIMESTAMP);

