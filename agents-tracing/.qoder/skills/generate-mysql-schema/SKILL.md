---
name: generate-mysql-schema
description: Generate MySQL DDL (CREATE TABLE) and seed data SQL scripts by reading Go entity definitions in pkg/servers/e2b/keys/mysql.go. Use when the user asks for database schema SQL, table creation scripts, initialization SQL, or MySQL migration scripts for the key storage module.
---

# Generate MySQL Schema SQL

Generate MySQL DDL and seed data SQL scripts based on the latest Go entity definitions in the codebase.

## Workflow

### Step 1: Read Source Files

Read the following files to get the latest entity definitions and initialization logic:

1. `pkg/servers/e2b/keys/mysql.go` — Primary source:
   - `TeamEntity` struct and its `TableName()` → table name
   - `TeamAPIKeyEntity` struct and its `TableName()` → table name
   - GORM tags (`gorm:"..."`) → column types, indexes, constraints
   - `gorm.Model` embedded fields → `id`, `created_at`, `updated_at`, `deleted_at`
   - `ensureAdminTeam()` → admin team seed data
   - `ensureAdminKey()` → admin API key seed logic
   - `AdminTeamUID` / `AdminKeyID` constants → well-known UUIDs

2. `pkg/servers/e2b/keys/keys.go` — For `AdminKeyID` and other constants if not in mysql.go.

3. `pkg/servers/e2b/models/` — For referenced model types if needed to understand field semantics.

### Step 2: Map GORM Tags to MySQL DDL

Apply these mapping rules:

| GORM Tag | MySQL DDL |
|----------|-----------|
| `gorm.Model` (embedded) | `id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY`, `created_at DATETIME(3)`, `updated_at DATETIME(3)`, `deleted_at DATETIME(3) DEFAULT NULL` + index on `deleted_at` |
| `type:varchar(N)` | `VARCHAR(N)` |
| `type:char(N)` | `CHAR(N)` |
| `not null` | `NOT NULL` |
| `uniqueIndex` | `UNIQUE INDEX` |
| `index` | `INDEX` |
| `column:xxx` | column name = `xxx` |
| No `column` tag | column name = snake_case of Go field name |

### Step 3: Generate DDL SQL

Output `CREATE TABLE IF NOT EXISTS` statements for each entity. Include:
- All columns with correct types and constraints
- Primary key
- Unique indexes and regular indexes (use GORM's default naming: `idx_<table>_<column>`)
- `deleted_at` index from `gorm.Model`
- `ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci`

### Step 4: Generate Seed Data SQL

Based on `ensureAdminTeam()` and `ensureAdminKey()` logic, generate:
- `INSERT ... ON DUPLICATE KEY UPDATE` for the admin team row using `AdminTeamUID`
- A comment explaining that the admin API key hash is computed at runtime via HMAC-SHA256(pepper, raw_key), so the seed SQL includes a placeholder

### Step 5: Output

Produce a single `.sql` file with:

```sql
-- =============================================================
-- Auto-generated from Go entities in pkg/servers/e2b/keys/mysql.go
-- Generated at: <current timestamp>
-- =============================================================

-- DDL
CREATE TABLE IF NOT EXISTS ...;
CREATE TABLE IF NOT EXISTS ...;

-- Seed Data
INSERT INTO ... ON DUPLICATE KEY UPDATE ...;
```

## Important Notes

- Always re-read source files on each invocation; do NOT rely on cached/stale definitions.
- If new entity structs or seed logic are added to `mysql.go`, include them automatically.
- Preserve the foreign-key relationship between `team_api_keys.team_id` and `teams.id` semantically (GORM does not auto-create FK constraints, but add a comment noting the relationship).
- The admin API key hash cannot be pre-computed in SQL because it depends on the runtime pepper config. Add a clear comment about this.
