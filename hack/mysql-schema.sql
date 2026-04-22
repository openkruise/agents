-- =============================================================
-- Auto-generated from Go entities in pkg/servers/e2b/keys/mysql.go
-- Generated at: 2026-04-22
-- =============================================================

-- ---------------------
-- DDL: teams
-- ---------------------
-- Source: TeamEntity struct, TableName() = "teams"
CREATE TABLE IF NOT EXISTS `teams` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `created_at` DATETIME(3)     NULL DEFAULT NULL,
  `updated_at` DATETIME(3)     NULL DEFAULT NULL,
  `deleted_at` DATETIME(3)     NULL DEFAULT NULL,
  `uid`        VARCHAR(36)     NOT NULL,
  `name`       VARCHAR(255)    NOT NULL,
  PRIMARY KEY (`id`),
  INDEX        `idx_teams_deleted_at` (`deleted_at`),
  UNIQUE INDEX `idx_teams_uid`        (`uid`),
  INDEX        `idx_teams_name`       (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ---------------------
-- DDL: team_api_keys
-- ---------------------
-- Source: TeamAPIKeyEntity struct, TableName() = "team_api_keys"
-- Note: team_id references teams.id semantically (GORM does not create FK constraints).
CREATE TABLE IF NOT EXISTS `team_api_keys` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `created_at`     DATETIME(3)     NULL DEFAULT NULL,
  `updated_at`     DATETIME(3)     NULL DEFAULT NULL,
  `deleted_at`     DATETIME(3)     NULL DEFAULT NULL,
  `uid`            VARCHAR(36)     NOT NULL,
  `name`           VARCHAR(255)    NOT NULL,
  `key_hash`       CHAR(64)        NOT NULL COMMENT 'HMAC-SHA256(pepper, raw_key) hex digest',
  `team_id`        BIGINT UNSIGNED NOT NULL COMMENT 'FK → teams.id (application-level, no DB constraint)',
  `created_by_uid` VARCHAR(36)     NULL DEFAULT NULL,
  PRIMARY KEY (`id`),
  INDEX        `idx_team_api_keys_deleted_at`     (`deleted_at`),
  UNIQUE INDEX `idx_team_api_keys_uid`            (`uid`),
  UNIQUE INDEX `idx_team_api_keys_key_hash`       (`key_hash`),
  INDEX        `idx_team_api_keys_team_id`        (`team_id`),
  INDEX        `idx_team_api_keys_created_by_uid` (`created_by_uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ---------------------
-- Seed Data
-- ---------------------

-- Admin team (well-known UUID: 550e8400-e29b-41d4-a716-446655449999)
-- Source: ensureAdminTeam() → FirstOrCreate with UID + Name
INSERT INTO `teams` (`uid`, `name`, `created_at`, `updated_at`)
VALUES ('550e8400-e29b-41d4-a716-446655449999', 'admin', NOW(3), NOW(3))
ON DUPLICATE KEY UPDATE `updated_at` = NOW(3);

-- Admin API key (well-known UUID: 550e8400-e29b-41d4-a716-446655440000)
-- Source: ensureAdminKey()
--
-- ⚠️  The key_hash value CANNOT be pre-computed in SQL because it depends on
--     the runtime pepper (env E2B_KEY_HASH_PEPPER). The application computes:
--       key_hash = hex(HMAC-SHA256(pepper, admin_raw_key))
--     and upserts this row on startup via ensureAdminKey().
--
-- The INSERT below uses a placeholder; replace <ADMIN_KEY_HASH> with the
-- actual 64-char hex digest if you need to seed the row manually.
--
-- INSERT INTO `team_api_keys` (`uid`, `name`, `key_hash`, `team_id`, `created_at`, `updated_at`)
-- VALUES (
--   '550e8400-e29b-41d4-a716-446655440000',
--   'admin',
--   '<ADMIN_KEY_HASH>',
--   (SELECT `id` FROM `teams` WHERE `uid` = '550e8400-e29b-41d4-a716-446655449999' LIMIT 1),
--   NOW(3),
--   NOW(3)
-- ) ON DUPLICATE KEY UPDATE
--   `key_hash`   = VALUES(`key_hash`),
--   `name`       = VALUES(`name`),
--   `team_id`    = VALUES(`team_id`),
--   `updated_at` = NOW(3),
--   `deleted_at` = NULL;
