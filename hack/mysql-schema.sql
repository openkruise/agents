-- =============================================================
-- Auto-generated from Go entities in pkg/servers/e2b/keys/mysql.go
-- Generated at: 2026-05-06
-- =============================================================

-- ---------------------
-- DDL: teams
-- ---------------------
-- Source: TeamEntity struct (pkg/servers/e2b/keys/mysql.go:66-71)
-- TableName() = "teams"
-- Note: uid is display-only compatibility metadata. Team identity and authorization use name.
--
-- Go struct mapping:
--   gorm.Model  -> id, created_at, updated_at, deleted_at
--   UID  string `gorm:"column:uid;type:varchar(36);uniqueIndex;not null"`
--   Name string `gorm:"type:varchar(255);uniqueIndex;not null"`
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
  UNIQUE INDEX `idx_teams_name`       (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ---------------------
-- DDL: team_api_keys
-- ---------------------
-- Source: TeamAPIKeyEntity struct (pkg/servers/e2b/keys/mysql.go:78-86)
-- TableName() = "team_api_keys"
-- Note: team_id references teams.id semantically (GORM does not create FK constraints).
--       KeyHash holds HMAC-SHA256(pepper, raw API key) as hex (64 chars), never plaintext.
--
-- Go struct mapping:
--   gorm.Model    -> id, created_at, updated_at, deleted_at
--   UID     string  `gorm:"column:uid;type:varchar(36);uniqueIndex;not null"`
--   Name    string  `gorm:"type:varchar(255);not null"`
--   KeyHash string  `gorm:"column:key_hash;type:char(64);uniqueIndex;not null"`
--   TeamID  uint    `gorm:"not null;index"`
--   CreatedByUID *string `gorm:"column:created_by_uid;type:varchar(36);index"`
CREATE TABLE IF NOT EXISTS `team_api_keys` (
  `id`             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `created_at`     DATETIME(3)     NULL DEFAULT NULL,
  `updated_at`     DATETIME(3)     NULL DEFAULT NULL,
  `deleted_at`     DATETIME(3)     NULL DEFAULT NULL,
  `uid`            VARCHAR(36)     NOT NULL,
  `name`           VARCHAR(255)    NOT NULL,
  `key_hash`       CHAR(64)        NOT NULL COMMENT 'HMAC-SHA256(pepper, raw_key) hex digest',
  `team_id`        BIGINT UNSIGNED NOT NULL COMMENT 'FK -> teams.id (application-level, no DB constraint)',
  `created_by_uid` VARCHAR(36)     NULL DEFAULT NULL COMMENT 'Creator metadata only, not for authorization',
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

-- Admin team (well-known display UUID: 550e8400-e29b-41d4-a716-446655449999)
-- Source: pkg/servers/e2b/models/api_key.go
--   AdminTeamID   = uuid.MustParse("550e8400-e29b-41d4-a716-446655449999")
--   AdminTeamName = "admin"
-- Logic: ensureAdminTeam() (mysql.go:154-176)
--   FirstOrCreate by Name = "admin" (including soft-deleted), then
--   ensures correct UID and restores if soft-deleted.
INSERT INTO `teams` (`uid`, `name`, `created_at`, `updated_at`)
VALUES ('550e8400-e29b-41d4-a716-446655449999', 'admin', NOW(3), NOW(3))
ON DUPLICATE KEY UPDATE `uid` = VALUES(`uid`), `updated_at` = NOW(3), `deleted_at` = NULL;

-- Admin API key (well-known UUID: 550e8400-e29b-41d4-a716-446655440000)
-- Source: pkg/servers/e2b/keys/secret.go:51
--   AdminKeyID = uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
-- Logic: ensureAdminKey() (mysql.go:178-215)
--   Upsert by UID: updates name, key_hash, team_id on existing row;
--   creates new row if not found; restores if soft-deleted.
--
-- The key_hash value CANNOT be pre-computed in SQL because it depends on
-- the runtime pepper (env E2B_KEY_HASH_PEPPER). The application computes:
--   key_hash = hex(HMAC-SHA256(pepper, admin_raw_key))
-- and upserts this row on startup via ensureAdminKey().
--
-- The INSERT below uses a placeholder; replace <ADMIN_KEY_HASH> with the
-- actual 64-char hex digest if you need to seed the row manually.
--
-- INSERT INTO `team_api_keys` (`uid`, `name`, `key_hash`, `team_id`, `created_at`, `updated_at`)
-- VALUES (
--   '550e8400-e29b-41d4-a716-446655440000',
--   'admin',
--   '<ADMIN_KEY_HASH>',
--   (SELECT `id` FROM `teams` WHERE `name` = 'admin' LIMIT 1),
--   NOW(3),
--   NOW(3)
-- ) ON DUPLICATE KEY UPDATE
--   `key_hash`   = VALUES(`key_hash`),
--   `name`       = VALUES(`name`),
--   `team_id`    = VALUES(`team_id`),
--   `updated_at` = NOW(3),
--   `deleted_at` = NULL;
