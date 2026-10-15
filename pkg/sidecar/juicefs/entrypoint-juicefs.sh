#!/bin/bash
set -e

# Defense in depth: mount-proxy exports every Secret entry as an environment
# variable of the same name, so a Secret key could smuggle a code-execution
# env var (BASH_ENV, LD_PRELOAD, ...) into this shell. The provider rejects
# these keys, but clear them here too so a future relaxation cannot silently
# reintroduce the attack. PATH is reset to the image default because an
# injected PATH would shadow command lookup.
unset BASH_ENV ENV BASHOPTS SHELLOPTS PROMPT_COMMAND PS4 IFS BASH_XTRACEFD \
      LD_PRELOAD LD_LIBRARY_PATH LD_AUDIT LD_BIND_NOW LD_DEBUG \
      GLIBC_TUNABLES CDPATH HISTFILE 2>/dev/null || true
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# JuiceFS entrypoint for OpenKruise customfuse csi-sidecar
#
# Environment variables injected by mount-proxy-server from CSI request:
#   $source       — JuiceFS META-URL (e.g. redis://redis-cluster:6379/0)
#   $mountpoint   — target mount path inside the sandbox
#   $token        — JuiceFS token (from Secret, if present)
#   $access_key   — object storage access key (from Secret)
#   $secret_key   — object storage secret key (from Secret)
#   $bucket       — object storage bucket name
#   $url          — object storage URL
#   $storageType  — object storage type ("s3" default, "oss", "minio", ...)
#   $path         — sub-path under the volume root
#   $capacity     — volume capacity quota (e.g. "100Gi")
#   $readOnly     — "true" or "false"
#   $otherOpts    — extra options from PV.Spec.VolumeAttributes

# Log masking
# META-URLs may embed credentials (redis://user:pass@host), never echo them.
# The second pattern covers userinfo without a scheme (user:pass@host),
# which the provider's charset check lets through.
mask_source() {
    printf '%s' "$1" | sed -E -e 's|(://)[^ ]*@|\1***@|' -e 's|([^ /:@]+:)[^ /@]*@|\1***@|'
}

# Shell injection prevention
validate_opts() {
    for opt in "$@"; do
        if [[ "$opt" =~ [\;\|\&\`\$\(\)$'\n'$'\r'\\] ]]; then
            echo "ERROR: invalid character in option: $opt" >&2
            exit 1
        fi
    done
}

# Defense in depth: the provider and node server validate source, but this
# entrypoint must stay safe even if a request bypasses them. The character
# set mirrors the provider's safeForwardPattern exactly.
if [ -z "$source" ]; then
    echo "ERROR: source is required" >&2
    exit 1
fi
if [[ ! "$source" =~ ^[][A-Za-z0-9_:/@.%-]+$ ]]; then
    echo "ERROR: source contains invalid characters" >&2
    exit 1
fi

# url is validated unconditionally (not only inside the format branch):
# the field must be well-formed even when the token-only path does not
# use it, and future code paths can rely on it being clean.
if [ -n "$url" ] && [[ ! "$url" =~ ^[][A-Za-z0-9_:/@.%-]+$ ]]; then
    echo "ERROR: url contains invalid characters" >&2
    exit 1
fi
if [ -n "$otherOpts" ]; then
    # Split on space, tab and comma — the same separator set the
    # provider's ValidateMountOptions uses — so a debug option embedded
    # after any separator ("cache-size=1024,debug" or
    # "allow_other<TAB>debug") cannot dodge the checks below. Empty
    # fields from consecutive separators ("a,,b") are dropped so they
    # cannot reach -o.
    IFS=$' ,\t' read -ra _RAW_OPTS <<< "$otherOpts"
    _OPTS=()
    for _o in "${_RAW_OPTS[@]}"; do
        [ -n "$_o" ] && _OPTS+=("$_o")
    done
    validate_opts "${_OPTS[@]}"
    # Debug options make the FUSE client print full request details —
    # including Authorization signatures and credential material — to
    # stderr, which mount-proxy forwards to logs. A volume never
    # legitimately needs them, so deny them outright.
    # background is denied for a different reason: it daemonizes the
    # client, so this shell would reap an exited parent, exit 0, and the
    # TERM trap below would no longer exist — the flush-on-unmount
    # guarantee would silently vanish.
    for _opt in "${_OPTS[@]}"; do
        case "${_opt%%=*}" in
            curldbg|dbg|dbglevel|debug|verbose)
                echo "ERROR: debug option $_opt is not allowed (would leak credentials into logs)" >&2
                exit 1 ;;
            background)
                echo "ERROR: background option is not allowed (defeats TERM unmount persistence)" >&2
                exit 1 ;;
        esac
    done
    # Rebuild the sanitized option string so the -o line carries exactly
    # what was validated, with empty fields gone.
    otherOpts=$(IFS=,; printf '%s' "${_OPTS[*]}")
fi

# 1. Auth
# Token mode: authenticate with JuiceFS token
# Static AK/SK mode: authenticate via --access-key/--secret-key in juicefs format
FORMAT_ARGS=()

if [ -n "$token" ]; then
    export JFS_TOKEN="$token"
fi

if [ -n "$access_key" ] && [ -n "$secret_key" ]; then
    FORMAT_ARGS+=(--access-key="$access_key" --secret-key="$secret_key")
elif [ -n "$access_key" ] || [ -n "$secret_key" ]; then
    # Half a credential pair silently skips format and fails at mount
    # time with no clue; fail loudly instead.
    echo "ERROR: access_key and secret_key must be provided together" >&2
    exit 1
fi

# 2. Format (first-time only, safe to re-run)
# juicefs requires the object storage parameters when formatting a new
# volume; they arrive from PV volumeAttributes as $bucket/$url/$storageType.
# $name is the JuiceFS file system name; like the official CSI driver, it
# arrives via the Secret (any non-reserved Secret key is exported as an
# env var of the same name). It customizes the format-time volume name:
# one META-URL database holds exactly one volume (a second format on the
# same database fails), and mount infers the name from the database, so
# the name is not passed to the mount command.
_JFS_NAME="${name:-myjfs}"
if [[ ! "$_JFS_NAME" =~ ^[A-Za-z0-9_-]+$ ]]; then
    echo "ERROR: name contains invalid characters" >&2
    exit 1
fi
if [ ${#FORMAT_ARGS[@]} -gt 0 ]; then
    if [ -z "$bucket" ]; then
        echo "ERROR: bucket is required for object storage format" >&2
        exit 1
    fi
    # Same character set the provider enforces for forwarded fields; the
    # values go to juicefs format as argv, but a malformed bucket must
    # fail here with a readable error instead of confusing the client.
    if [[ ! "$bucket" =~ ^[][A-Za-z0-9_:/@.%-]+$ ]]; then
        echo "ERROR: bucket contains invalid characters" >&2
        exit 1
    fi
    # MinIO is S3-compatible; JuiceFS does not accept "minio" as a
    # --storage value.
    _storageType="${storageType:-s3}"
    [ "$_storageType" = "minio" ] && _storageType="s3"
    if [[ ! "$_storageType" =~ ^[A-Za-z0-9_-]+$ ]]; then
        echo "ERROR: storageType contains invalid characters" >&2
        exit 1
    fi
    # juicefs format has no --endpoint option: a self-hosted S3-compatible
    # endpoint is passed as part of --bucket in URL form
    # (http://host:port/bucket). Verified against the real client.
    if [ -n "$url" ]; then
        # ${url%/} strips a trailing slash so "http://host:9000/" does not
        # become "http://host:9000//bucket" in the URL-form --bucket.
        FORMAT_ARGS+=(--storage="$_storageType" --bucket="${url%/}/${bucket}")
    else
        FORMAT_ARGS+=(--storage="$_storageType" --bucket="$bucket")
    fi
    # $format_options carries extra juicefs format arguments as a
    # comma-separated key=value list (e.g. "trash-days=1,block-size=4096"),
    # mirroring the official CSI driver's Secret field (snake_case because
    # Secret keys become environment variables). Every entry becomes one
    # --key=value argv argument; keys that would override the
    # provider-composed format flags are denied.
    if [ -n "$format_options" ]; then
        IFS=, read -ra _FOPTS <<< "$format_options"
        for _opt in "${_FOPTS[@]}"; do
            [ -n "\$_opt" ] || continue
            [[ "\$_opt" == *=* ]] || { echo "ERROR: format_options entry must be key=value: \$_opt" >&2; exit 1; }
            _fkey="${_opt%%=*}"
            _fval="${_opt#*=}"
            [[ "$_fkey" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] || { echo "ERROR: invalid format_options key: \$_opt" >&2; exit 1; }
            [[ "$_fval" =~ ^[A-Za-z0-9_./%+-]*$ ]] || { echo "ERROR: invalid format_options value: \$_opt" >&2; exit 1; }
            case "$_fkey" in
                access-key|secret-key|storage|bucket)
                    echo "ERROR: format_options must not override provider-composed format flags: $_fkey" >&2
                    exit 1 ;;
            esac
            FORMAT_ARGS+=(--"$_fkey=$_fval")
        done
    fi
    echo "Formatting volume: $(mask_source "$source") name=$_JFS_NAME storage=$_storageType bucket=$bucket"
    # juicefs format 1.4.1 replays the configuration idempotently on an
    # existing volume (verified empirically: a re-run succeeds), so the
    # normal path never fails here. The fallback below still guards the
    # failure case: treat it as "already formatted" only when the error
    # says so or the volume is actually reachable, so a genuine format
    # failure (bad credentials, bad bucket) stays fatal instead of being
    # masked. Bad AK/SK cannot hide here: mounting authenticates via token
    # only, and the AK/SK pair participates solely in format — a volume
    # that exists implies the format failure was "already formatted", not a
    # credential error. The error-text check comes first because
    # `juicefs status` may itself need authentication and fail on a
    # perfectly healthy existing volume.
    if ! _fmt_err=$(juicefs format "${FORMAT_ARGS[@]}" "$source" "$_JFS_NAME" 2>&1); then
        if grep -qi "already" <<< "$_fmt_err"; then
            echo "WARNING: volume already formatted; continuing"
        elif juicefs status "$source" >/dev/null 2>&1; then
            echo "WARNING: format failed but volume already exists; continuing"
        else
            # Mask credential material before echoing: the error output
            # may replay the full command line including --access-key and
            # --secret-key values, and the source/endpoint may embed
            # credentials (redis://user:pass@..., http://user:pass@...).
            _fmt_err=$(printf '%s' "$_fmt_err" | sed -E -e 's|(--access-key=)[^ ]*|\1***|' -e 's|(--secret-key=)[^ ]*|\1***|')
            _fmt_err=$(mask_source "$_fmt_err")
            echo "ERROR: juicefs format failed: $_fmt_err" >&2
            exit 1
        fi
    fi
fi

# 3. Credential cleanup
# access_key/secret_key were consumed by format; the raw token env is no
# longer needed. JFS_TOKEN stays: mount.juicefs authenticates with it and
# is not passed --token on the command line, so clearing it here would
# break the token-only mount path entirely.
_HAS_AUTH=0
if [ -n "$token" ] || { [ -n "$access_key" ] && [ -n "$secret_key" ]; }; then
    _HAS_AUTH=1
fi
unset access_key secret_key token

# 4. Build mount options
# JuiceFS 1.x runs in the foreground by default (no -d), and the 1.4.1
# client no longer accepts the legacy "foreground" and "no-update" options
# — both are passed through to fusermount3, which rejects them. User
# options come FIRST so provider-injected options win under the last-wins
# duplicate parsing: a user-supplied rw must not weaken the read-only
# semantics derived from the CSI request. (Under a first-wins parser this
# ordering is a no-op.)
MOUNT_OPTS=""
[ -n "$otherOpts" ] && MOUNT_OPTS="${otherOpts}"

# Read-only
[ "$readOnly" = "true" ] && MOUNT_OPTS="${MOUNT_OPTS}${MOUNT_OPTS:+,}ro"

# 5. Set quota (optional)
if [ -n "$capacity" ]; then
    # The numeric prefix must be a plain non-negative integer in every
    # unit branch; a negative value must not reach quota set. The 15-digit
    # bound mirrors the shared CapacityPattern so a bypassed request cannot
    # push an out-of-range value into the unit arithmetic below.
    _cap_num=${capacity%%[A-Za-z]*}
    if [[ ! "$_cap_num" =~ ^[0-9]{1,15}$ ]]; then
        echo "ERROR: invalid capacity: $capacity" >&2
        exit 1
    fi
    case "$capacity" in
        # Fractional values never reach here: the numeric-prefix check
        # above rejects them ("1.5" is not ^[0-9]+$).
        *TiB|*Ti) capacity=$(( ${capacity%%[A-Za-z]*} * 1024 )) ;;
        *GiB|*Gi) capacity=${capacity%%[A-Za-z]*} ;;
        # Round up: a sub-GiB quota like 500Mi must not become 0.
        *MiB|*Mi) capacity=$(( (${capacity%%[A-Za-z]*} + 1023) / 1024 )) ;;
        # Round up: a sub-GiB quota must not become 0.
        *KiB|*Ki) capacity=$(( (${capacity%%[A-Za-z]*} + 1048575) / 1048576 )) ;;
        *[A-Za-z]*)
            echo "ERROR: unsupported capacity unit: $capacity" >&2
            exit 1 ;;
        *)
            # Plain integer without a unit: juicefs quota set interprets
            # --capacity in GiB. Reject anything that is not digits so a
            # negative or malformed value cannot reach quota set; the
            # 15-digit bound mirrors the shared CapacityPattern so an
            # absurdly large value fails fast instead of erroring inside
            # quota set at mount time.
            if [[ ! "$capacity" =~ ^[0-9]{1,15}$ ]]; then
                echo "ERROR: invalid capacity: $capacity" >&2
                exit 1
            fi
            ;;
    esac
    echo "Setting JuiceFS quota: capacity=${capacity}GiB"
    if ! juicefs quota set "$source" --path / --capacity "$capacity"; then
        echo "WARNING: juicefs quota set failed; mounting without quota" >&2
    fi
fi

# 6. Mount
# $path mounts a sub-directory as the volume root via --subdir. It is passed
# as an argv argument, never interpolated into -o, so it has no shell
# injection surface; its character set is already enforced by the provider.
# The checks below are defense in depth for requests that bypassed the
# provider: metacharacters are rejected and parent traversal is denied.
MOUNT_ARGS=()
if [ -n "$path" ]; then
    if [[ ! "$path" =~ ^[][A-Za-z0-9_:/@.%-]+$ ]]; then
        echo "ERROR: path contains invalid characters" >&2
        exit 1
    fi
    # Same strictness as mountpoint: empty segments would pass through
    # to --subdir as "a//b" and rely on the client to normalize them.
    if [[ "$path" == *//* ]]; then
        echo "ERROR: path must not contain empty path segments" >&2
        exit 1
    fi
    # Split on "/" exactly (IFS as a read prefix does not leak into the
    # rest of the script) so ".." segments are caught without relying on
    # unquoted word splitting.
    IFS=/ read -ra _segs <<< "$path"
    for _seg in "${_segs[@]}"; do
        [ "$_seg" = ".." ] && { echo "ERROR: path must not traverse to the parent directory" >&2; exit 1; }
    done
    # --subdir expects a path relative to the volume root; strip every
    # leading slash the PV may carry ("//shared" -> "shared"). "/" alone
    # means the root itself: omit --subdir entirely.
    _subdir="$path"
    while [[ "$_subdir" == /* ]]; do _subdir="${_subdir#/}"; done
    # "." means the volume root itself — same treatment as "/" (omit
    # --subdir entirely) instead of relying on the client to normalize.
    if [ -n "$_subdir" ] && [ "$_subdir" != "." ]; then
        MOUNT_ARGS+=(--subdir="$_subdir")
    fi
fi
if [ -z "$mountpoint" ]; then
    echo "ERROR: mountpoint is required" >&2
    exit 1
fi
# Defense in depth matching the provider's validateMountTarget: the
# mountpoint must be an absolute path of safe characters without parent
# traversal or empty segments, even for requests that bypassed the
# provider. The character set mirrors mountPathPattern exactly (path
# characters only — no URL characters like : @ % [ ]).
if [[ ! "$mountpoint" =~ ^/[A-Za-z0-9_./-]+$ ]]; then
    echo "ERROR: mountpoint must be an absolute path with safe characters" >&2
    exit 1
fi
if [[ "$mountpoint" == *//* ]]; then
    echo "ERROR: mountpoint must not contain empty path segments" >&2
    exit 1
fi
IFS=/ read -ra _segs <<< "$mountpoint"
for _seg in "${_segs[@]}"; do
    [ "$_seg" = ".." ] && { echo "ERROR: mountpoint must not traverse to the parent directory" >&2; exit 1; }
done
# The On-Demand Volume Mounting doc requires the mount target to be an
# empty directory; mounting over a non-empty one hides its content and
# fails in the CSI flow. Create the directory (the client also would) and
# refuse to proceed if anything is already in it.
mkdir -p "$mountpoint"
if [ -n "$(ls -A "$mountpoint" 2>/dev/null)" ]; then
    echo "ERROR: mountpoint $mountpoint must be an empty directory" >&2
    exit 1
fi
if [ "$_HAS_AUTH" != 1 ]; then
    echo "WARNING: mounting without token or AK/SK; it will fail unless the metadata engine requires no authentication" >&2
fi

# The log line masks option values: otherOpts may carry credential-like
# key=value pairs (the provider rejects credential keys, but a request
# that bypassed it must not leak them into logs either).
_MOUNT_OPTS_LOG=$(printf '%s' "$MOUNT_OPTS" | sed -E 's|([^, =]+=)[^, ]*|\1***|g')
echo "Mounting JuiceFS: source=$(mask_source "$source") mountpoint=$mountpoint options=$_MOUNT_OPTS_LOG subdir=${_subdir:-/}"
# The client runs in the background instead of exec so this shell can
# translate TERM into a proper unmount. mount-proxy (and start.sh) send
# SIGTERM to stop the mount, but JuiceFS buffers writes and a bare TERM
# kills the client without flushing — data written in the last seconds is
# lost. umount triggers the flush; only then is the client killed.
# The explicit `juicefs mount` subcommand is used instead of the
# mount.juicefs symlink: the symlink dispatches on argv[0], which bash
# does not preserve for backgrounded commands.
if [ -n "$MOUNT_OPTS" ]; then
    juicefs mount "$source" "$mountpoint" -o "$MOUNT_OPTS" "${MOUNT_ARGS[@]}" &
else
    juicefs mount "$source" "$mountpoint" "${MOUNT_ARGS[@]}" &
fi
JFPID=$!
# Every command carries || true: errexit is in effect inside traps, and one
# failing step (umount AND fusermount both fail, wait returning 143 after
# the kill) must not abort the trap before kill/exit run — that would leave
# the client orphaned and skip the exit-0 handshake with mount-proxy.
trap 'umount "$mountpoint" 2>/dev/null || fusermount -u "$mountpoint" 2>/dev/null || true; kill $JFPID 2>/dev/null || true; wait $JFPID 2>/dev/null || true; exit 0' TERM INT
wait $JFPID
exit $?
