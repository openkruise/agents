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
is a base64-encoded JSON payload used by secretKeyStorage, then emits a SQL file:
1) DDL for `teams` and `team_api_keys`
2) Upserts for all teams referenced by the API keys
3) Upserts for all valid API keys with key_hash = HMAC-SHA256(pepper, raw_key)

Usage:
    python hack/migrate_secret_keys_to_mysql.py \
        --namespace sandbox-system --pepper mysecret
    E2B_KEY_HASH_PEPPER=mysecret python hack/migrate_secret_keys_to_mysql.py \
        --namespace sandbox-system --output - | mysql e2b
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
class ParsedTeam:
    uid: str
    name: str


@dataclass(frozen=True)
class ParsedAPIKey:
    uid: str
    name: str
    raw_key: str
    created_at: dt.datetime | None
    created_by_uid: str | None
    team: ParsedTeam
    quota_json: str | None


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
        help=f"Output SQL path, or - for stdout (default: {DEFAULT_OUTPUT})",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Validate entries without rendering or writing SQL output",
    )
    parser.add_argument(
        "--quiet",
        action="store_true",
        help="Suppress success messages",
    )
    return parser.parse_args()


def require_uuid(value: str, field_name: str) -> str:
    try:
        return str(uuid.UUID(value))
    except (ValueError, TypeError) as err:
        raise ValueError(f"invalid {field_name}: {value!r}") from err


def require_non_empty_string(value: Any, field_name: str) -> str:
    if not isinstance(value, str) or value == "":
        raise ValueError(f"{field_name} is empty or is not a string")
    return value


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
    except ValueError as err:
        raise ValueError(f"invalid createdAt: {raw!r}") from err


def sql_escape(value: str) -> str:
    return value.replace("\\", "\\\\").replace("'", "''")


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


def parse_team(payload: Any, admin_team_uid: str) -> ParsedTeam:
    # secretKeyStorage assigns legacy entries without a team to the admin team
    # when it loads them. Preserve that compatibility behavior during migration.
    if payload is None:
        return ParsedTeam(uid=admin_team_uid, name=DEFAULT_ADMIN_TEAM_NAME)
    if not isinstance(payload, dict):
        raise ValueError("apiKey.team is not a JSON object")

    name = require_non_empty_string(payload.get("name"), "apiKey.team.name")
    uid = require_uuid(str(payload.get("id", "")), "apiKey.team.id")
    if name == DEFAULT_ADMIN_TEAM_NAME:
        uid = admin_team_uid
    return ParsedTeam(uid=uid, name=name)


def encode_quota(payload: dict[str, Any]) -> str | None:
    if "quota" not in payload or payload["quota"] is None:
        return None
    # Keep the raw JSON semantics. Both backends deliberately tolerate an
    # invalid stored quota on authentication paths and treat it as unlimited.
    return json.dumps(payload["quota"], ensure_ascii=False, separators=(",", ":"))


def collect_teams(
    admin_team_uid: str, rows: list[ParsedAPIKey]
) -> list[ParsedTeam]:
    admin_team = ParsedTeam(
        uid=admin_team_uid,
        name=DEFAULT_ADMIN_TEAM_NAME,
    )
    by_name = {admin_team.name: admin_team}
    by_uid = {admin_team.uid: admin_team}

    for row in rows:
        team = row.team
        if existing := by_name.get(team.name):
            if existing.uid != team.uid:
                raise ValueError(
                    f"team name {team.name!r} has conflicting UIDs "
                    f"{existing.uid!r} and {team.uid!r}"
                )
        elif existing := by_uid.get(team.uid):
            raise ValueError(
                f"team UID {team.uid!r} has conflicting names "
                f"{existing.name!r} and {team.name!r}"
            )
        else:
            by_name[team.name] = team
            by_uid[team.uid] = team

    return sorted(
        by_name.values(),
        key=lambda team: (team.name != DEFAULT_ADMIN_TEAM_NAME, team.name, team.uid),
    )


def decode_secret_entries(
    secret_json: dict[str, Any], admin_team_uid: str
) -> list[ParsedAPIKey]:
    data = secret_json.get("data", {})
    if not isinstance(data, dict):
        raise ValueError("secret .data is not an object")

    parsed: list[ParsedAPIKey] = []
    errors: list[str] = []
    raw_key_owners: dict[str, str] = {}
    for data_key, encoded_value in data.items():
        try:
            if not isinstance(encoded_value, str):
                raise ValueError("secret data value is not a base64 string")
            payload_bytes = base64.b64decode(encoded_value, validate=True)
            payload = json.loads(payload_bytes.decode("utf-8"))
            if not isinstance(payload, dict):
                raise ValueError("payload is not a JSON object")

            uid = require_uuid(str(payload.get("id", "")), "apiKey.id")
            name = require_non_empty_string(payload.get("name"), "apiKey.name")
            raw_key = require_non_empty_string(payload.get("key"), "apiKey.key")

            created_at = parse_rfc3339(payload.get("createdAt"))
            created_by_uid: str | None = None
            created_by = payload.get("createdBy")
            if created_by is not None:
                if not isinstance(created_by, dict):
                    raise ValueError("apiKey.createdBy is not a JSON object")
                created_by_uid = require_uuid(
                    str(created_by.get("id", "")), "apiKey.createdBy.id"
                )

            team = parse_team(payload.get("team"), admin_team_uid)
            quota_json = encode_quota(payload)

            if uid != data_key:
                raise ValueError(
                    f"entry key {data_key!r} mismatches payload id {uid!r}"
                )
            if owner_uid := raw_key_owners.get(raw_key):
                raise ValueError(
                    f"raw key duplicates API key {owner_uid!r}; "
                    "MySQL requires key_hash to be unique"
                )
            raw_key_owners[raw_key] = uid

            parsed.append(
                ParsedAPIKey(
                    uid=uid,
                    name=name,
                    raw_key=raw_key,
                    created_at=created_at,
                    created_by_uid=created_by_uid,
                    team=team,
                    quota_json=quota_json,
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

    try:
        collect_teams(admin_team_uid, parsed)
    except ValueError as err:
        raise ValueError(f"secret contains inconsistent team metadata: {err}") from err

    parsed.sort(key=lambda item: item.uid)
    return parsed


def render_sql(admin_team_uid: str, pepper: str, rows: list[ParsedAPIKey]) -> str:
    generated_at = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    teams = collect_teams(admin_team_uid, rows)

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
        "  UNIQUE INDEX `idx_teams_name` (`name`),",
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
        "  `quota` JSON DEFAULT NULL,",
        "  PRIMARY KEY (`id`),",
        "  UNIQUE INDEX `idx_team_api_keys_uid` (`uid`),",
        "  UNIQUE INDEX `idx_team_api_keys_key_hash` (`key_hash`),",
        "  INDEX `idx_team_api_keys_team_id` (`team_id`),",
        "  INDEX `idx_team_api_keys_created_by_uid` (`created_by_uid`),",
        "  INDEX `idx_team_api_keys_deleted_at` (`deleted_at`)",
        ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;",
        "",
        "-- Migrated Data",
        "-- Team name is the storage and authorization identity;",
        "-- uid is compatibility metadata.",
        "-- Legacy API keys without team metadata are assigned to the admin team.",
        "",
    ]

    for team in teams:
        lines.extend(
            [
                "INSERT INTO `teams` (",
                "  `uid`, `name`, `created_at`, `updated_at`, `deleted_at`",
                ") VALUES (",
                f"  {sql_literal(team.uid)}, {sql_literal(team.name)}, "
                "NOW(3), NOW(3), NULL",
                ")",
                "ON DUPLICATE KEY UPDATE",
                "  `uid` = VALUES(`uid`),",
                "  `updated_at` = NOW(3),",
                "  `deleted_at` = NULL;",
                "",
            ]
        )

    lines.extend(
        [
            "-- team_api_keys.team_id semantically references teams.id;",
            "-- no explicit FK is created here.",
            "-- key_hash is HMAC-SHA256(pepper, raw_key), matching mysqlKeyStorage.",
            "-- The migration computes every hash with the provided runtime pepper.",
            "-- Quota JSON is preserved, including invalid-quota compatibility.",
            "",
        ]
    )

    for row in rows:
        key_hash = hash_key(row.raw_key, pepper)
        team_id_expr = (
            f"(SELECT `id` FROM `teams` WHERE `name` = {sql_literal(row.team.name)} "
            "ORDER BY `id` ASC LIMIT 1)"
        )
        lines.extend(
            [
                "INSERT INTO `team_api_keys` (",
                "  `uid`, `name`, `key_hash`, `team_id`, `created_by_uid`,",
                "  `quota`, `created_at`, `updated_at`, `deleted_at`",
                ") VALUES (",
                f"  {sql_literal(row.uid)}, {sql_literal(row.name)}, "
                f"{sql_literal(key_hash)}, {team_id_expr},",
                f"{sql_literal(row.created_by_uid)}, {sql_literal(row.quota_json)}, "
                f"{format_datetime(row.created_at)}, NOW(3), NULL",
                ")",
                "ON DUPLICATE KEY UPDATE",
                "  `name` = VALUES(`name`),",
                "  `key_hash` = VALUES(`key_hash`),",
                "  `team_id` = VALUES(`team_id`),",
                "  `created_by_uid` = VALUES(`created_by_uid`),",
                "  `quota` = VALUES(`quota`),",
                "  `updated_at` = NOW(3),",
                "  `deleted_at` = NULL;",
                "",
            ]
        )

    return "\n".join(lines)


def main() -> int:
    args = parse_args()

    if not args.dry_run and not args.pepper:
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
        rows = decode_secret_entries(secret_json, admin_team_uid)
    except Exception as err:  # noqa: BLE001
        print(f"ERROR: {err}", file=sys.stderr)
        return 1

    if args.dry_run:
        if not args.quiet:
            print(f"Validated keys: {len(rows)}")
        return 0

    sql = render_sql(admin_team_uid=admin_team_uid, pepper=args.pepper, rows=rows)
    if args.output == "-":
        sys.stdout.write(sql)
        if not sql.endswith("\n"):
            sys.stdout.write("\n")
        status_stream = sys.stderr
        output_name = "stdout"
    else:
        with open(args.output, "w", encoding="utf-8") as fp:
            fp.write(sql)
            if not sql.endswith("\n"):
                fp.write("\n")
        status_stream = sys.stdout
        output_name = args.output

    if not args.quiet:
        print(f"Wrote SQL: {output_name}", file=status_stream)
        print(f"Migrated keys: {len(rows)}", file=status_stream)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
