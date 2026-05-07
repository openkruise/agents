-- =============================================================
-- Auto-generated from Go entities in pkg/servers/e2b/keys/mysql.go
-- Generated at: 2026-05-07
--
-- Source constants:
--   AdminTeamID  = 550e8400-e29b-41d4-a716-446655449999  (models/api_key.go)
--   AdminKeyID   = 550e8400-e29b-41d4-a716-446655440000  (keys/secret.go)
--   AdminTeamName = "admin"                               (models/api_key.go)
-- =============================================================

-- ---------------------------------------------------------------
-- DDL
-- ---------------------------------------------------------------

CREATE TABLE IF NOT EXISTS `teams` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `created_at` DATETIME(3)     DEFAULT NULL,
  `updated_at` DATETIME(3)     DEFAULT NULL,
  `deleted_at` DATETIME(3)     DEFAULT NULL,
  -- display-only compatibility metadata; authorization uses `name`
  `uid`        VARCHAR(36)     NOT NULL,
  `name`       VARCHAR(255)    NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE INDEX `idx_teams_uid`  (`uid`),
  UNIQUE INDEX `idx_teams_name` (`name`),
  INDEX        `idx_teams_deleted_at` (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE IF NOT EXISTS `team_api_keys` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `created_at`      DATETIME(3)     DEFAULT NULL,
  `updated_at`      DATETIME(3)     DEFAULT NULL,
  `deleted_at`      DATETIME(3)     DEFAULT NULL,
  `uid`             VARCHAR(36)     NOT NULL,
  `name`            VARCHAR(255)    NOT NULL,
  -- HMAC-SHA256(pepper, raw_api_key) stored as 64-char hex; never plaintext
  `key_hash`        CHAR(64)        NOT NULL,
  -- References teams.id (no FK constraint; relationship enforced by application)
  `team_id`         BIGINT UNSIGNED NOT NULL,
  -- Creator metadata only; not used for ownership or authorization
  `created_by_uid`  VARCHAR(36)     DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE INDEX `idx_team_api_keys_uid`      (`uid`),
  UNIQUE INDEX `idx_team_api_keys_key_hash` (`key_hash`),
  INDEX        `idx_team_api_keys_team_id`          (`team_id`),
  INDEX        `idx_team_api_keys_created_by_uid`   (`created_by_uid`),
  INDEX        `idx_team_api_keys_deleted_at`        (`deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- ---------------------------------------------------------------
-- Seed Data
-- ---------------------------------------------------------------

-- Admin team row (AdminTeamID = 550e8400-e29b-41d4-a716-446655449999)
-- ensureAdminTeam() uses FirstOrCreate by name, then patches the uid if needed.
INSERT INTO `teams` (`uid`, `name`, `created_at`, `updated_at`)
VALUES ('550e8400-e29b-41d4-a716-446655449999', 'admin', NOW(3), NOW(3))
ON DUPLICATE KEY UPDATE
  `uid`        = VALUES(`uid`),
  `updated_at` = NOW(3);

-- Admin API key row (AdminKeyID = 550e8400-e29b-41d4-a716-446655440000)
--
-- IMPORTANT: `key_hash` cannot be pre-computed here because it is
-- HMAC-SHA256(pepper, admin_key) and `pepper` is a runtime secret read from
-- the sandbox-manager configuration. The application's ensureAdminKey() call
-- at startup inserts or updates this row with the correct hash automatically.
--
-- If you need to pre-seed this row for testing, compute the hash with:
--   echo -n "<admin_key>" | openssl dgst -sha256 -hmac "<pepper>"
-- and substitute it below in place of <RUNTIME_COMPUTED_HASH>.
--
-- INSERT INTO `team_api_keys` (`uid`, `name`, `key_hash`, `team_id`, `created_at`, `updated_at`)
-- SELECT '550e8400-e29b-41d4-a716-446655440000', 'admin', '<RUNTIME_COMPUTED_HASH>', id, NOW(3), NOW(3)
-- FROM `teams` WHERE `name` = 'admin' LIMIT 1
-- ON DUPLICATE KEY UPDATE
--   `name`       = VALUES(`name`),
--   `key_hash`   = VALUES(`key_hash`),
--   `team_id`    = VALUES(`team_id`),
--   `updated_at` = NOW(3);
