#!/usr/bin/env python3
#
# Copyright 2026.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Migrate secretKeyStorage API keys to MySQL bootstrap SQL.

This script reads Kubernetes secret `e2b-key-store` data entries where each value
is a base64-encoded JSON payload for `CreatedTeamAPIKey`, then emits a SQL file:
1) DDL for `teams` and `team_api_keys`
2) Seed upsert for the admin team row
3) Upserts for all valid API keys with key_hash = HMAC-SHA256(pepper, raw_key)
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import hashlib
import hmac
import json
import os
import subprocess
import sys
import uuid
from dataclasses import dataclass
from typing import Any

DEFAULT_SECRET_NAME = "e2b-key-store"
DEFAULT_ADMIN_TEAM_UID = "550e8400-e29b-41d4-a716-446655449999"
DEFAULT_OUTPUT = "e2b_key_migration.sql"
DEFAULT_ADMIN_TEAM_NAME = "admin"


@dataclass(frozen=True)
class ParsedAPIKey:
    uid: str
    name: str
    raw_key: str
    created_at: dt.datetime | None
    created_by_uid: str | None


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Export API keys from k8s secret to MySQL migration SQL."
    )
    parser.add_argument("--namespace", required=True, help="Kubernetes namespace")
    parser.add_argument(
        "--secret-name",
        default=DEFAULT_SECRET_NAME,
        help=f"Secret name (default: {DEFAULT_SECRET_NAME})",
    )
    parser.add_argument(
        "--pepper",
        default=os.environ.get("E2B_KEY_HASH_PEPPER", ""),
        help="HMAC pepper. Defaults to env E2B_KEY_HASH_PEPPER if set.",
    )
    parser.add_argument(
        "--admin-team-uid",
        default=DEFAULT_ADMIN_TEAM_UID,
        help=f"Admin team UID in `teams.uid` (default: {DEFAULT_ADMIN_TEAM_UID})",
    )
    parser.add_argument(
        "--output",
        default=DEFAULT_OUTPUT,
        help=f"Output SQL path (default: {DEFAULT_OUTPUT})",
    )
    return parser.parse_args()


def require_uuid(value: str, field_name: str) -> str:
    try:
        return str(uuid.UUID(value))
    except (ValueError, TypeError) as err:
        raise ValueError(f"invalid {field_name}: {value!r}") from err


def parse_rfc3339(raw: Any) -> dt.datetime | None:
    if not raw:
        return None
    if not isinstance(raw, str):
        raise ValueError(f"createdAt is not a string: {raw!r}")
    text = raw.strip()
    if not text:
        return None
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return dt.datetime.fromisoformat(text)
    except ValueError:
        return None


def sql_escape(value: str) -> str:
    return value.replace("'", "''")


def sql_literal(value: str | None) -> str:
    if value is None:
        return "NULL"
    return f"'{sql_escape(value)}'"


def format_datetime(value: dt.datetime | None) -> str:
    if value is None:
        return "NULL"
    if value.tzinfo is not None:
        value = value.astimezone(dt.timezone.utc).replace(tzinfo=None)
    return sql_literal(value.strftime("%Y-%m-%d %H:%M:%S.%f")[:-3])


def hash_key(raw_key: str, pepper: str) -> str:
    digest = hmac.new(
        key=pepper.encode("utf-8"),
        msg=raw_key.encode("utf-8"),
        digestmod=hashlib.sha256,
    )
    return digest.hexdigest()


def run_kubectl(namespace: str, secret_name: str) -> dict[str, Any]:
    cmd = [
        "kubectl",
        "-n",
        namespace,
        "get",
        "secret",
        secret_name,
        "-o",
        "json",
    ]
    proc = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        stderr = proc.stderr.strip() or "unknown kubectl error"
        raise RuntimeError(f"kubectl failed: {stderr}")
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as err:
        raise RuntimeError("failed to parse kubectl JSON output") from err


def decode_secret_entries(secret_json: dict[str, Any]) -> list[ParsedAPIKey]:
    data = secret_json.get("data", {})
    if not isinstance(data, dict):
        raise ValueError("secret .data is not an object")

    parsed: list[ParsedAPIKey] = []
    errors: list[str] = []
    for data_key, encoded_value in data.items():
        try:
            payload_bytes = base64.b64decode(encoded_value)
            payload = json.loads(payload_bytes.decode("utf-8"))
            if not isinstance(payload, dict):
                raise ValueError("payload is not a JSON object")

            uid = require_uuid(str(payload.get("id", "")), "apiKey.id")
            name = str(payload.get("name", "")).strip()
            if not name:
                raise ValueError("apiKey.name is empty")

            raw_key = str(payload.get("key", "")).strip()
            if not raw_key:
                raise ValueError("apiKey.key is empty")

            created_at = parse_rfc3339(payload.get("createdAt"))
            created_by_uid: str | None = None
            created_by = payload.get("createdBy")
            if isinstance(created_by, dict) and created_by.get("id"):
                created_by_uid = require_uuid(str(created_by.get("id")), "apiKey.createdBy.id")

            if uid != data_key:
                raise ValueError(
                    f"entry key {data_key!r} mismatches payload id {uid!r}"
                )

            parsed.append(
                ParsedAPIKey(
                    uid=uid,
                    name=name,
                    raw_key=raw_key,
                    created_at=created_at,
                    created_by_uid=created_by_uid,
                )
            )
        except Exception as err:  # noqa: BLE001
            errors.append(f"secret entry {data_key!r}: {err}")

    if errors:
        details = "\n".join(f"- {item}" for item in errors)
        raise ValueError(
            "secret contains malformed API key entries; aborting migration to avoid partial export:\n"
            f"{details}"
        )

    parsed.sort(key=lambda item: item.uid)
    return parsed


def render_sql(admin_team_uid: str, pepper: str, rows: list[ParsedAPIKey]) -> str:
    generated_at = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    lines: list[str] = [
        "-- =============================================================",
        "-- Auto-generated from Go entities in pkg/servers/e2b/keys/mysql.go",
        f"-- Generated at: {generated_at}",
        "-- =============================================================",
        "",
        "-- DDL",
        "CREATE TABLE IF NOT EXISTS `teams` (",
        "  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,",
        "  `created_at` DATETIME(3) DEFAULT NULL,",
        "  `updated_at` DATETIME(3) DEFAULT NULL,",
        "  `deleted_at` DATETIME(3) DEFAULT NULL,",
        "  `uid` VARCHAR(36) NOT NULL,",
        "  `name` VARCHAR(255) NOT NULL,",
        "  PRIMARY KEY (`id`),",
        "  UNIQUE INDEX `idx_teams_uid` (`uid`),",
        "  INDEX `idx_teams_name` (`name`),",
        "  INDEX `idx_teams_deleted_at` (`deleted_at`)",
        ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;",
        "",
        "CREATE TABLE IF NOT EXISTS `team_api_keys` (",
        "  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,",
        "  `created_at` DATETIME(3) DEFAULT NULL,",
        "  `updated_at` DATETIME(3) DEFAULT NULL,",
        "  `deleted_at` DATETIME(3) DEFAULT NULL,",
        "  `uid` VARCHAR(36) NOT NULL,",
        "  `name` VARCHAR(255) NOT NULL,",
        "  `key_hash` CHAR(64) NOT NULL,",
        "  `team_id` BIGINT UNSIGNED NOT NULL,",
        "  `created_by_uid` VARCHAR(36) DEFAULT NULL,",
        "  PRIMARY KEY (`id`),",
        "  UNIQUE INDEX `idx_team_api_keys_uid` (`uid`),",
        "  UNIQUE INDEX `idx_team_api_keys_key_hash` (`key_hash`),",
        "  INDEX `idx_team_api_keys_team_id` (`team_id`),",
        "  INDEX `idx_team_api_keys_created_by_uid` (`created_by_uid`),",
        "  INDEX `idx_team_api_keys_deleted_at` (`deleted_at`)",
        ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;",
        "",
        "-- Seed Data",
        "INSERT INTO `teams` (`uid`, `name`, `created_at`, `updated_at`)",
        f"VALUES ({sql_literal(admin_team_uid)}, {sql_literal(DEFAULT_ADMIN_TEAM_NAME)}, NOW(3), NOW(3))",
        "ON DUPLICATE KEY UPDATE",
        "  `name` = VALUES(`name`),",
        "  `updated_at` = NOW(3);",
        "",
        "-- team_api_keys.team_id semantically references teams.id; explicit FK is not created here.",
        "-- key_hash must be HMAC-SHA256(pepper, raw_key) to match mysqlKeyStorage behavior.",
        "-- Admin API key hash cannot be hardcoded safely without runtime pepper; this migration computes all key_hash values using the provided pepper.",
        "",
    ]

    team_id_expr = (
        f"(SELECT `id` FROM `teams` WHERE `uid` = {sql_literal(admin_team_uid)} "
        "ORDER BY `id` ASC LIMIT 1)"
    )
    for row in rows:
        key_hash = hash_key(row.raw_key, pepper)
        lines.extend(
            [
                "INSERT INTO `team_api_keys` (",
                "  `uid`, `name`, `key_hash`, `team_id`, `created_by_uid`, `created_at`, `updated_at`, `deleted_at`",
                ") VALUES (",
                f"  {sql_literal(row.uid)}, {sql_literal(row.name)}, {sql_literal(key_hash)}, {team_id_expr}, "
                f"{sql_literal(row.created_by_uid)}, {format_datetime(row.created_at)}, NOW(3), NULL",
                ")",
                "ON DUPLICATE KEY UPDATE",
                "  `name` = VALUES(`name`),",
                "  `key_hash` = VALUES(`key_hash`),",
                "  `team_id` = VALUES(`team_id`),",
                "  `created_by_uid` = VALUES(`created_by_uid`),",
                "  `updated_at` = NOW(3),",
                "  `deleted_at` = NULL;",
                "",
            ]
        )

    return "\n".join(lines)


def main() -> int:
    args = parse_args()

    if not args.pepper:
        print(
            "ERROR: pepper is required. Pass --pepper or set E2B_KEY_HASH_PEPPER.",
            file=sys.stderr,
        )
        return 1

    try:
        admin_team_uid = require_uuid(args.admin_team_uid, "admin-team-uid")
    except ValueError as err:
        print(f"ERROR: {err}", file=sys.stderr)
        return 1

    try:
        secret_json = run_kubectl(args.namespace, args.secret_name)
        rows = decode_secret_entries(secret_json)
    except Exception as err:  # noqa: BLE001
        print(f"ERROR: {err}", file=sys.stderr)
        return 1

    sql = render_sql(admin_team_uid=admin_team_uid, pepper=args.pepper, rows=rows)
    with open(args.output, "w", encoding="utf-8") as fp:
        fp.write(sql)
        if not sql.endswith("\n"):
            fp.write("\n")

    print(f"Wrote SQL: {args.output}")
    print(f"Migrated keys: {len(rows)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
