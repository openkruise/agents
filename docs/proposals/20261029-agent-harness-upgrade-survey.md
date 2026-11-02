---
title: Agent Harness Upgrade Survey
authors:
  - "@TBD"
creation-date: 2026-10-29
last-updated: 2026-10-29
status: provisional
see-also:
  - "/docs/proposals/20261023-sandboxupdateops-installer-update.md"
---

# Agent Harness Upgrade Survey

## Table of Contents

- [Summary](#summary)
- [Scope and Method](#scope-and-method)
- [How Each Harness Upgrades Itself](#how-each-harness-upgrades-itself)
- [Install-location overrides](#install-location-overrides)
  - [Relocatability classes](#relocatability-classes)
- [Air-gapped and mirrored environments](#air-gapped-and-mirrored-environments)
- [Version pinning](#version-pinning)
- [Example installer image: Hermes Agent](#example-installer-image-hermes-agent)
- [Conclusions Carried Into the InstallerUpdate Proposal](#conclusions-carried-into-the-installerupdate-proposal)
- [Implementation History](#implementation-history)
- [References](#references)

## Summary

This is the research backing for
[`SandboxUpdateOps` InstallerUpdate Strategy](./20261023-sandboxupdateops-installer-update.md).
That proposal adds an update strategy which injects an ephemeral container into a
running sandbox so new harness content can be written into the target container's
filesystem without replacing the Pod or changing its image. Whether that is
possible at all, and what an installer image has to do to make it work, is
determined entirely by how the harnesses themselves install and upgrade — so the
survey is the load-bearing part of the design.

It is kept in a separate document because the design proposal should be readable
on its own. Only the conclusions are reproduced there; the per-harness evidence,
the empirical verification transcripts and the worked installer image live here.

Headline findings:

1. **Upgrading is a filesystem mutation followed by a process restart**, never an
   image rebuild. Every harness surveyed replaces files under an install prefix
   and then expects the long-running process to be restarted.
2. **The install location can be overridden** in every harness surveyed except
   Claude Code's native binary, and that exception is a single static file.
3. **Air-gapped support is weak but version pinning is universal.** No harness can
   install with zero network access; all of them can install an exact version.
4. **Container-managed installs are often refused outright**, so a harness's own
   `update` command cannot be depended on inside a sandbox.
5. **Relocatability, not file copying, is the real constraint.** A toolchain that
   bakes absolute paths produces an install that reports success and does not run.

## Scope and Method

Five mainstream agent harnesses: **OpenClaw**, **Claude Code**, **Codex CLI**,
**Gemini CLI** and **Hermes Agent**. Each was examined for four things: the
command that performs an upgrade, whether the install location can be redirected,
whether an internal site mirror can be used, and whether an exact version can be
pinned.

Findings are recorded as verified only where they were checked rather than read:
the npm prefix redirect, the shape of the resulting tree, the registry override
and `--offline` behaviour were all confirmed by running npm offline against a
scratch package. Everything else is cited to vendor documentation, source, or a
tracked issue, and the citation is given so a reader can re-check it. Where a
harness's own issue tracker records a gap, the issue number and its state are
given rather than a paraphrase.

## How Each Harness Upgrades Itself

| Harness | Upgrade command | Mechanism | Restart control |
|---|---|---|---|
| **OpenClaw** | `openclaw update` | Installs into a *temporary* npm prefix, verifies the packaged `dist` inventory, then swaps it into the real global prefix; retries once with `--omit=optional`. | `--no-restart` separates the file swap from the process restart. `--yes` for non-interactive, `update status --json` for machine-readable state. |
| **Claude Code** | `claude update`; `claude install [version]` | Native installer writes a static binary to `~/.local/bin`; the npm path writes into the global prefix. | No separation flag; `autoUpdatesChannel` selects the channel. |
| **Codex CLI** | `codex upgrade` | Delegates to `npm install -g @openai/codex@…` or `brew upgrade codex`. | None — the command is a thin wrapper. |
| **Gemini CLI** | `gemini update` | Delegates to `npm install -g @google/gemini-cli@latest`. | None. |
| **Hermes Agent** | `hermes update` | Git-based: syncs with upstream and checks out the channel target, then re-runs the `uv`-managed environment install. `--channel` / `--set-channel` (`stable` = the exact latest `vX.Y.Z` commit, `main` = branch tip), `--branch`, `--plan`, `--check`, `--backup`, `--keep-stash`, `--yes`. | `--no-gateway-restart`. Without it, **every gateway belonging to that install is stopped immediately before the checkout is first changed**. |

Two behaviours are decisive for the design and are worth stating separately.

**Container-managed installs are refused.** OpenClaw records an install owner in
`openclaw-install-owner.json` and *skips* the update when it detects no owner,
which is the container case. Hermes Agent bakes
`/etc/hermes/image-provenance.json` (mode `0444`, outside both `$HERMES_HOME` and
the checkout) into its published images; when the marker is present,
`hermes update`, `hermes update --check` and the dashboard's update button all
refuse cleanly with exit code 2, and the refusal fails closed on a damaged marker.
Two independent harnesses converging on the same refusal is the strongest evidence
for the design's premise: self-update cannot be depended on inside a sandbox, so
the operator must be able to supply the install step.

**`update` entry points cannot be aimed at a foreign rootfs.** They resolve their
own install location from the running process. Their `install` entry points and the
package managers underneath can be redirected, which is what the design relies on.

## Install-location overrides

| Harness | Install-location override | Notes |
|---|---|---|
| **OpenClaw** | **First-class.** `install-cli.sh --prefix <path>` (default `~/.openclaw`); env `OPENCLAW_PREFIX`, `OPENCLAW_HOME`. `openclaw update` honours `NPM_CONFIG_PREFIX`, documented as `NPM_CONFIG_PREFIX="$(npm prefix -g)" openclaw update`. | The local-prefix installer is a *fresh install* path, so it sidesteps the `update` owner check that skips container-managed installs. Non-interactive: `--no-prompt`, `--no-onboard`, `--json`, `OPENCLAW_NO_PROMPT=1`. |
| **Hermes Agent** | **First-class, and the cleanest case.** Install-location flags `--dir`, `-InstallDir`, `-HermesHome`; env `HERMES_HOME`. Defaults: `~/.hermes` (Linux/macOS), `%LOCALAPPDATA%\hermes` (Windows), `/opt/hermes` (container), `$PREFIX/lib/hermes` (Termux). | Distributed by installer script, git, or APT — **not** npm or pip. The Python environment is managed by `uv`, which changes the relocatability analysis; see below. The whole install is a **self-contained directory**, so redirecting it does not interleave with the target's shared `/usr/local` the way an npm global prefix does. |
| **Claude Code** | **Partial.** `claude install [version]` — the argument is a *version*, not a path; the native installer targets `~/.local/bin` with no location override. The npm path honours `NPM_CONFIG_PREFIX`. | The config directory is overridable by env var, the install directory is not. For the native binary, write the downloaded artefact straight into the target's `~/.local/bin`. |
| **Codex CLI** | **Indirect.** `codex upgrade` delegates to `npm install -g @openai/codex@…` or `brew upgrade codex`; `NPM_CONFIG_PREFIX` covers the npm path. | Homebrew has no relocatable prefix, so a brew-managed target is not reachable this way. |
| **Gemini CLI** | **Indirect.** `gemini update` runs `npm install -g @google/gemini-cli@latest`; `NPM_CONFIG_PREFIX` applies. | Same Homebrew caveat. |

The npm mechanism was verified empirically rather than assumed. `NPM_CONFIG_PREFIX`,
`PREFIX` and `--prefix` all redirect a global install, and the resulting layout is
`<prefix>/lib/node_modules/<pkg>` with `<prefix>/bin/<name>`:

```
$ NPM_CONFIG_PREFIX=/tmp/a npm root -g
/tmp/a/lib/node_modules
$ NPM_CONFIG_PREFIX=$PREFIX_DIR npm install -g ./faketool-1.2.3.tgz
<prefix>/bin/faketool -> ../lib/node_modules/faketool/cli.js
<prefix>/lib/node_modules/faketool/{cli.js,package.json}
```

Two properties of that output are what make the redirect safe:

- The `bin` entry is a **relative** symlink (`../lib/node_modules/…`), so the tree
  is relocatable and resolves correctly when read from inside the target as
  `/usr/local/bin/…` even though it was written through `/proc/1/root/usr/local`.
- The shebang is `#!/usr/bin/env node`, resolved at **runtime** from PATH. So npm
  executes in the installer's mount namespace while the artefact it produces
  executes against the *target's* interpreter. Verified by running the installed
  `bin` on PATH.


### Relocatability classes

Those two properties are not universal, and assuming they are produces a second
silent-failure mode distinct from the `execve` trap. A toolchain that bakes
**absolute paths** into its output at install time will write paths beginning
`/proc/1/root/…` — a prefix that does not exist inside the target container. The
install reports success, the files are all present, and every entry point is
broken.

Hermes Agent is the case that exposes this, because it manages its Python
environment with `uv`. Console scripts in `.venv/bin` get absolute shebangs and
`pyvenv.cfg` records an absolute home path (astral-sh/uv#13350). `uv` provides
`uv venv --relocatable` (env `UV_VENV_RELOCATABLE`), which rewrites both to
relative paths, but it is **not** the stable default — relocatable-by-default
ships only as a uv *preview* feature (astral-sh/uv#17770). It must be requested
explicitly.

A cheap post-write assertion catches all three classes at once and belongs in the
installer contract:

```sh
if grep -rq "$SANDBOX_INSTALLER_TARGET_ROOT" "$target" 2>/dev/null; then
  echo "install baked an absolute /proc/1/root path" >&2; exit 126
fi
```

Exit code 126 is reserved for this, mirroring the 125 reserved for failed
namespace targeting.


The three classes and what each needs are recorded in the design proposal, because
the installer contract depends on them:
[Relocatability is the real constraint](./20261023-sandboxupdateops-installer-update.md#relocatability-is-the-real-constraint).

## Air-gapped and mirrored environments

Much of the target deployment is a cluster whose sandboxes have no outbound
internet access. Two separate questions follow: can the harness's installer be
pointed at an internal **site mirror**, and can a **specific version** be pinned?
No harness in the survey answers "yes, fully offline" to both.

| Harness | Site mirror | Version pinning | True air-gap (no network at all) |
|---|---|---|---|
| **OpenClaw** | **Yes, explicit.** `OPENCLAW_INSTALL_NPM_REGISTRY` "replaces `https://registry.npmjs.org`"; `OPENCLAW_INSTALL_BUN_PIN_URL` and `OPENCLAW_INSTALL_BUN_RELEASE_BASE_URL` redirect the Bun fork runtime. The npm/pnpm/bun path also honours `NPM_CONFIG_REGISTRY`. | **Yes.** `--version <version\|dist-tag\|spec>` (default `latest`), `OPENCLAW_VERSION`, `--tag`; git installs accept a 40-character commit SHA. Sealed releases verify a published Bun pin hash and fail on mismatch. | **No.** The installer scripts "do not support fully offline installation"; pins, archives and metadata require network. Source installs require network unless a local pnpm lockfile is used. |
| **Claude Code** | **No mirror for the native installer** — `curl -fsSL https://claude.ai/install.sh \| bash` is a fixed URL. The npm path inherits `NPM_CONFIG_REGISTRY`. | **Yes.** `claude install [version]`; channel via `autoUpdatesChannel`; npm `@<version>`. | **No.** anthropics/claude-code#39847 requested an offline installer and was closed unimplemented; it also notes that authentication is required even in offline settings. |
| **Codex CLI** | No harness-level mirror; inherits npm's. The Homebrew path can use `HOMEBREW_BOTTLE_DOMAIN` / `HOMEBREW_API_DOMAIN`. | **Yes.** `npm install -g @openai/codex@<version>`. | No documented support and no tracked request. |
| **Gemini CLI** | No harness-level mirror; inherits npm's. | **Yes.** `npm install -g @google/gemini-cli@<version>`. | No documented support and no tracked request. |
| **Hermes Agent** | Not at harness level. Its `uv`-managed Python environment inherits `UV_INDEX_URL`, `UV_EXTRA_INDEX_URL`, `UV_NO_INDEX`, `UV_FIND_LINKS`, `UV_OFFLINE`, `UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR` and `UV_PYTHON_DOWNLOADS`. | Via `uv.lock` or `--exact`, or a git SHA for source installs. | **No.** NousResearch/hermes-agent#17696 is **open**: "Hermes Agent cannot be deployed in offline or air-gapped environments … installation cannot be completed at all." The FAQ's affirmative "can I use it offline" answer concerns *runtime* model endpoints (`hermes model` → Custom endpoint), not installation. |

For the npm-distributed harnesses a fully local install is achievable and should
be the default recommendation: bake `npm pack` tarballs into the installer image
and install with `--offline`. Both mechanisms were verified empirically —
`NPM_CONFIG_REGISTRY` overrides the registry, and `--offline` puts npm in
`only-if-cached` mode so it fails with `ENOTCACHED` rather than reaching the
network:

```
$ NPM_CONFIG_REGISTRY=https://nexus.internal/repository/npm/ npm config get registry
https://nexus.internal/repository/npm/

$ npm install --offline --dry-run <uncached-pkg>
npm error code ENOTCACHED
npm error request to https://registry.npmjs.org/<pkg> failed:
npm error cache mode is 'only-if-cached' but no cached response is available.
```

`--offline` requires a populated npm cache (`npm config get cache`, itself
overridable); a tarball install requires none, which is why the tarball form is
preferred. For `uv`-managed harnesses the equivalent is `UV_NO_INDEX` with
`UV_FIND_LINKS` pointing at a local wheelhouse, plus `UV_PYTHON_INSTALL_DIR`
pre-populated if the interpreter must also come from the image.


## Version pinning

Every harness surveyed can install an exact version: OpenClaw
`--version <version|dist-tag|spec>` / `OPENCLAW_VERSION` / `--tag`, and a
40-character commit SHA for git installs; Claude Code `claude install [version]`
or npm `@<version>`; Codex and Gemini through npm `@<version>`; Hermes through
`uv.lock`, `--exact`, or a git SHA for source installs.

An installer image that resolves `latest` at upgrade time defeats the purpose of
carrying a payload in an image: the installed version would vary with the calendar
rather than with the image tag, and a `postUpgrade` version assertion would have
nothing stable to assert against. Pinning is therefore treated as mandatory in the
design, with the installer image tag as the record of what was pinned.

## Example installer image: Hermes Agent

Hermes Agent is the hardest case in the survey and therefore the most instructive
example: it is not distributed through npm or pip, its Python environment is managed
by `uv` (so it falls in the *relocatable only on request* class), its container
installs refuse `hermes update`, and its air-gap support is an open issue. It is
also the cleanest install-location story — `HERMES_HOME` plus `--dir`, and a
self-contained directory rather than a shared system prefix.

This is what an operator's installer image looks like for it. The image is the
payload carrier: everything needed is baked in, so the install requires no egress
from the sandbox.

```dockerfile
# syntax=docker/dockerfile:1
# Base must match the target's libc family. Hermes Agent's venv and any native
# wheels are built against glibc; a musl base would produce unusable binaries.
FROM python:3.12-slim-bookworm

ARG HERMES_VERSION=1.9.2

COPY --from=ghcr.io/astral-sh/uv:0.9.11 /uv /uvx /usr/local/bin/

# A pinned source tree, a local wheelhouse and a pre-downloaded interpreter — all
# baked in, so nothing is fetched at upgrade time.
COPY payload/hermes-src/       /payload/hermes-src/
COPY payload/wheelhouse/       /payload/wheelhouse/
COPY payload/python/           /payload/python/

ENV HERMES_VERSION=${HERMES_VERSION}
COPY install.sh /install.sh
ENTRYPOINT ["/install.sh"]
```

The vendor's own `install-hermes.sh` is deliberately *not* used here: it fetches
over the network, which is exactly what an air-gapped cluster cannot do (see
[Air-gapped and mirrored environments](#air-gapped-and-mirrored-environments)). The
script below reproduces what it does — unpack a pinned tree, build the `uv`
environment, place it under `HERMES_HOME` — from a payload the image already
carries. Where a mirror *is* reachable, baking the vendor script in and calling it
with `--dir "$SANDBOX_INSTALLER_TARGET_ROOT/opt/hermes"` is the shorter form.

```sh
#!/bin/sh
set -eu

version="${HERMES_VERSION:?HERMES_VERSION must be pinned in the image}"
root="${SANDBOX_INSTALLER_TARGET_ROOT:?}"

# Reserved exit 125: prove namespace targeting took effect before writing anything.
# Without it an unsupporting runtime yields a silent no-op that reports success.
[ /proc/1/root -ef / ] && { echo "namespace targeting unavailable" >&2; exit 125; }

# Build entirely in the installer's own filesystem. Nothing here executes a binary
# from under $root -- that is the execve trap, and it would silently install into
# the ephemeral container instead of the sandbox.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/hermes"
cp -a /payload/hermes-src/. "$work/hermes/"

# Offline uv: no index, local wheelhouse only, interpreter from the image.
export UV_NO_INDEX=1
export UV_FIND_LINKS=/payload/wheelhouse
export UV_OFFLINE=1
export UV_CACHE_DIR="$work/.uv-cache"
export UV_PYTHON_INSTALL_DIR=/payload/python
export UV_PYTHON_DOWNLOADS=never

# Relocatable is NOT uv's stable default. Without it, console scripts in .venv/bin
# get absolute shebangs and pyvenv.cfg records an absolute home -- both prefixed
# /proc/1/root, which does not exist inside the target.
export UV_VENV_RELOCATABLE=1
uv venv --relocatable "$work/hermes/.venv"
uv pip install --python "$work/hermes/.venv/bin/python" \
  -r "$work/hermes/requirements.txt"

# Stage into a versioned directory beside the live one. Writing over a running
# binary fails with ETXTBSY, and a half-copied tree leaves the harness unusable.
mkdir -p "$root/opt"
release="$root/opt/hermes-$version"
rm -rf "$release"
mkdir -p "$release"
cp -a "$work/hermes/." "$release/"
printf '%s\n' "$version" > "$release/INSTALLER_VERSION"

# Switch atomically: rename(2) over the symlink. The running process keeps its old
# inode until postUpgrade restarts it.
ln -s "hermes-$version" "$root/opt/.hermes-current.new"
mv -f "$root/opt/.hermes-current.new" "$root/opt/hermes-current"

# Reserved exit 126: assert no absolute /proc/1/root path was baked into the result.
if grep -rq "$root" "$release" 2>/dev/null; then
  echo "install baked an absolute $root path" >&2
  exit 126
fi
```

The matching `postUpgrade` hook runs inside the sandbox through envd, where
`/opt/hermes-current` is a real path and the harness's own interpreter and service
manager are available — no capability needed:

```yaml
lifecycle:
  postUpgrade:
    exec:
      command:
        - /bin/sh
        - -c
        - |
          set -eu
          export HERMES_HOME=/opt/hermes-current
          [ "$(cat "$HERMES_HOME/INSTALLER_VERSION")" = "1.9.2" ]
          "$HERMES_HOME/.venv/bin/hermes" --version | grep -F 1.9.2
          "$HERMES_HOME/.venv/bin/hermes" gateway restart
    timeoutSeconds: 120
```

What this example demonstrates, and what generalises to any harness:

- **Self-contained directory over shared prefix.** `/opt/hermes-<version>` plus a
  `hermes-current` symlink cannot clobber an unrelated dependency of the target, is
  trivially removable, and makes the relocatability class obvious. `HERMES_HOME` is
  how the harness finds itself, so the target needs no path baked into it.
- **Payload in the image, version pinned at build time.** `HERMES_VERSION` is an
  image build arg, so the image tag *is* the record of what was installed, and the
  `postUpgrade` assertion has something stable to assert against.
- **Both reserved exit codes earned their place.** 125 catches the runtime that
  does not implement targeting; 126 catches the `uv` default that would have
  shipped a broken install reported as success.
- **Nothing is executed from under the target rootfs.** `uv`, `cp`, `ln` and `mv`
  all come from the installer image.
- **Restart and verification are `postUpgrade`'s job**, not the installer's.
- For an npm-distributed harness the same shape applies with `NPM_CONFIG_PREFIX`
  and a `npm pack` tarball in place of `uv` and the wheelhouse — see
  [Run your own tooling, never the target's](./20261023-sandboxupdateops-installer-update.md#run-your-own-tooling-never-the-targets).


## Conclusions Carried Into the InstallerUpdate Proposal

| Finding | Where it lands in the design |
|---|---|
| Upgrade is file mutation then restart | The two-step split: the installer container replaces files, `postUpgrade` restarts and verifies. |
| Install location is overridable | "Run your own tooling, never the target's" — the installer executes only its own tooling with the prefix redirected through `/proc/1/root`. |
| Container-managed installs are refused | `installer.image` and `installer.command` are operator-supplied; the controller never assumes a harness `update` command works. |
| Air-gap support is weak, image distribution is not | The payload is baked into the installer image rather than fetched at upgrade time. |
| Version pinning is universal | Pinning is mandatory; the image tag is the record, and `postUpgrade` asserts against it. |
| Relocatability varies by toolchain | Reserved exit code `126` for a baked absolute `/proc/1/root` path, and a preference for a self-contained directory located by a `*_HOME` variable. |
| Hermes is the hardest case | The worked example above, and the E2E control in the design proposal's test plan. |

## Implementation History

- [ ] 10/29/2026: Split out of
      `docs/proposals/20261023-sandboxupdateops-installer-update.md`, carrying the
      survey detail that its Background section had accumulated: the per-harness
      install-location and air-gap tables, the npm prefix and `--offline`
      verification transcripts, the relocatability evidence, and the worked Hermes
      Agent installer image. Conclusions stayed behind in the design proposal.
- [ ] Re-verify the Hermes Agent example image against the harness's current
      bootstrap path before treating it as a working reference. Its published
      documentation requires Python 3.14 and supports `python -m pm.cli install`
      from a source checkout, while the example below builds a `uv` environment on
      a 3.12 base; its package manager also strips ambient `UV_*` variables other
      than a forwarded index and transport set. The example is retained as written
      here because it demonstrates the contract's shape, not because it has been
      run.
- [ ] Verify the prefix-redirect pattern end-to-end against a real OpenClaw and a
      real Hermes Agent sandbox, including whether lifecycle scripts and native
      builds survive the libc-family match requirement.

## References

- [OpenClaw `update` CLI](https://docs.openclaw.ai/cli/update) — channels, `--yes`, `--no-restart`, `update status --json`, `NPM_CONFIG_PREFIX` override, and container-install behaviour.
- [OpenClaw installer internals](https://docs.openclaw.ai/install/installer) — `install-cli.sh --prefix`, `OPENCLAW_PREFIX`, `OPENCLAW_HOME`, the non-interactive flags, and the mirror variables `OPENCLAW_INSTALL_NPM_REGISTRY`, `OPENCLAW_INSTALL_BUN_PIN_URL`, `OPENCLAW_INSTALL_BUN_RELEASE_BASE_URL`.
- [OpenClaw other update methods](https://docs.openclaw.ai/install/updating/update-methods) — `openclaw update` installs into a temporary npm prefix, verifies the packaged `dist` inventory, then swaps it into the real global prefix; retries once with `--omit=optional`.
- [anthropics/claude-code#39847: Complete Offline Installation](https://github.com/anthropics/claude-code/issues/39847) — requested, closed unimplemented.
- [NousResearch/hermes-agent#17696: Support offline/air-gapped installation](https://github.com/NousResearch/hermes-agent/issues/17696) — **open**; "installation cannot be completed at all" without outbound network.
- [uv settings reference](https://docs.astral.sh/uv/reference/settings/) — `UV_INDEX_URL`, `UV_NO_INDEX`, `UV_FIND_LINKS`, `UV_OFFLINE`, `UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR`, `UV_PYTHON_DOWNLOADS`.
- [npm config](https://docs.npmjs.com/cli/v9/using-npm/config) and [npm-pack](https://docs.npmjs.com/cli/v12/commands/npm-pack/) — `prefix` and global-mode install location; `registry`/`NPM_CONFIG_REGISTRY`, `offline`, `prefer-offline`, `cache`, and tarball creation. The prefix redirect, the registry override and `--offline` (`only-if-cached` → `ENOTCACHED`) were all verified empirically.
- [OpenClaw install](https://docs.openclaw.ai/install) — the local-prefix installer as an alternative install method.
- [OpenClaw updating](https://docs.openclaw.ai/install/updating) — `openclaw-install-owner.json`, owner values, and when updates are skipped.
- [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference) — `claude install [version]` takes a version, not a path.
- [Claude Code advanced setup](https://code.claude.com/docs/en/setup) — `claude update`, `autoUpdatesChannel`.
- [openai/codex#9274: Add codex upgrade command for self-updates](https://github.com/openai/codex/issues/9274).
- [google-gemini/gemini-cli#18618: Add 'gemini update' command](https://github.com/google-gemini/gemini-cli/issues/18618) and the [Gemini CLI reference](https://geminicli.com/docs/cli/cli-reference/).
- [NousResearch/hermes-agent](https://github.com/nousresearch/hermes-agent) — "The agent that grows with you"; Python, `uv`-managed environments.
- [Hermes Agent — Installation](https://hermes-agent.nousresearch.com/docs/getting-started/installation) — `~/.hermes`, `/opt/hermes`, `$PREFIX/lib/hermes`, and the `--dir` / `-InstallDir` / `-HermesHome` location flags.
- [Hermes Agent — CLI commands](https://hermes-agent.nousresearch.com/docs/reference/cli-commands) — `hermes update`, `--check`, `--backup` (`HERMES_HOME` snapshot).
- [astral-sh/uv#13350: Shebang lines in scripts use absolute paths](https://github.com/astral-sh/uv/issues/13350) — the non-relocatable venv failure mode.
- [astral-sh/uv#17770: Use relocatable virtual environments by default](https://github.com/astral-sh/uv/pull/17770) — preview-only, so `--relocatable` / `UV_VENV_RELOCATABLE` must be set explicitly.
- [uv CLI reference](https://docs.astral.sh/uv/reference/cli/) — `uv venv --relocatable`.
