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

# s3fs entrypoint for OpenKruise customfuse csi-sidecar
#
# Environment variables injected by mount-proxy-server from CSI request:
#   $source       — S3 bucket to mount ("s3://ml-datasets" or bare bucket name)
#   $url          — S3-compatible endpoint URL (MinIO, Alibaba OSS, Ceph RGW, ...)
#   $mountpoint   — target mount path inside the sandbox
#   $access_key   — object storage access key (from Secret)
#   $secret_key   — object storage secret key (from Secret)
#   $readOnly     — "true" or "false"
#   $otherOpts    — extra options from PV.Spec.VolumeAttributes
#   $path         — rejected: only the bucket root is mounted
#
# Unlike the JuiceFS entrypoint there is no format step and no metadata
# engine: s3fs mounts an S3 bucket directly. Only the bucket root is
# mounted; sub-bucket prefixes are not supported.

# Credentials may be embedded in endpoint URLs (http://user:pass@host),
# never echo them.
mask_url() {
    # The second pattern covers userinfo without a scheme (user:pass@host),
    # which the bucket charset allows through.
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
if [ -n "$otherOpts" ]; then
    # Split on space, tab and comma — the same separator set the
    # provider's ValidateMountOptions uses — so a debug option embedded
    # after any separator cannot dodge the checks below. Empty fields
    # from consecutive separators ("a,,b") are dropped so they cannot
    # reach -o.
    IFS=$' ,\t' read -ra _RAW_OPTS <<< "$otherOpts"
    _OPTS=()
    for _o in "${_RAW_OPTS[@]}"; do
        [ -n "$_o" ] && _OPTS+=("$_o")
    done
    validate_opts "${_OPTS[@]}"
    # Debug options make s3fs print full HTTP headers — including the
    # Authorization signature — to stderr, which mount-proxy forwards to
    # logs. A volume never legitimately needs them, so deny them outright.
    for _opt in "${_OPTS[@]}"; do
        # A keyless "=value" entry is malformed and must not reach -o;
        # the provider rejects it too, this mirrors that check.
        case "$_opt" in
            =*) echo "ERROR: empty option key in otherOpts" >&2; exit 1 ;;
        esac
        case "${_opt%%=*}" in
            curldbg|dbg|dbglevel|debug|verbose)
                echo "ERROR: debug option $_opt is not allowed (would leak credentials into logs)" >&2
                exit 1 ;;
            background)
                echo "ERROR: background option is not allowed (the client must stay in the foreground for TERM propagation)" >&2
                exit 1 ;;
        esac
    done
    # Rebuild the sanitized option string so the -o line carries exactly
    # what was validated, with empty fields gone.
    otherOpts=$(IFS=,; printf '%s' "${_OPTS[*]}")
fi

# 1. Validate all inputs before any credential material is written to disk
# 1a. Credentials are required for s3fs
if [ -z "$access_key" ] || [ -z "$secret_key" ]; then
    echo "ERROR: access_key and secret_key are required for s3fs" >&2
    exit 1
fi

# 1a2. A newline in either credential would inject an extra line into the
# s3fs passwd file, silently corrupting authentication; a tab, colon or
# space would break the accesskey:secretkey field split. NUL cannot reach a
# shell variable (it terminates the C string at execve), so it is not
# checked here.
case "$access_key$secret_key" in
    *$'\n'*|*$'\r'*|*$'\t'*|*:*|*' '*)
        echo "ERROR: credentials must not contain newlines, tabs, spaces or colons" >&2
        exit 1 ;;
esac

# 1b. source must be "s3://bucket" or a bare bucket name. Any other URL
# form (redis://, http://user:pass@host, ...) is not an S3 bucket and may
# embed credentials that would leak into logs or the mount attempt.
case "$source" in
    s3://*) ;; # canonical form
    *://*)
        echo "ERROR: source must be 's3://bucket' or a bare bucket name: $(mask_url "$source")" >&2
        exit 1 ;;
esac

# 1b2. The provider may forward a path volumeAttribute for the generic
# driver, but this entrypoint mounts only the bucket root. Reject it
# loudly instead of silently mounting the root while the user asked for
# a sub-directory.
if [ -n "$path" ]; then
    echo "ERROR: path is not supported by the s3fs entrypoint (only the bucket root is mounted)" >&2
    exit 1
fi

# 1c. Resolve the bucket name: "s3://bucket" or a bare bucket name
BUCKET="${source#s3://}"
BUCKET="${BUCKET%/}"
if [ -z "$BUCKET" ]; then
    echo "ERROR: source must name an S3 bucket (e.g. s3://ml-datasets)" >&2
    exit 1
fi
# Only the bucket root is mounted; a prefix like "s3://bucket/sub" must
# be rejected instead of being passed to s3fs as a bucket name.
case "$BUCKET" in
    */*)
        echo "ERROR: sub-bucket prefixes are not supported (only the bucket root is mounted): $(mask_url "$BUCKET")" >&2
        exit 1 ;;
esac
# Same character set the provider enforces for forwarded fields, so a
# bare bucket name stays safe even for requests that bypassed it.
if [[ ! "$BUCKET" =~ ^[][A-Za-z0-9_:/@.%-]+$ ]]; then
    echo "ERROR: bucket name contains invalid characters: $(mask_url "$BUCKET")" >&2
    exit 1
fi

# 1d. mountpoint is injected by mount-proxy; guard against a missing value
# so the failure is readable instead of an s3fs usage message. The path
# checks mirror the provider's validateMountTarget, matching the JuiceFS
# entrypoint's defense in depth.
if [ -z "$mountpoint" ]; then
    echo "ERROR: mountpoint is empty" >&2
    exit 1
fi
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

# 1e. url is embedded into the comma-separated -o options, so any
# character outside the provider's forwarded-field set — a comma would
# split options, whitespace would break parsing — is rejected up front.
if [ -n "$url" ] && [[ ! "$url" =~ ^[][A-Za-z0-9_:/@.%-]+$ ]]; then
    echo "ERROR: url contains invalid characters: $(mask_url "$url")" >&2
    exit 1
fi

# 2. s3fs reads credentials from a passwd file in "accesskey:secretkey"
# format. The file stays for the mount's lifetime (s3fs may re-read it on
# reload); it lives in the container's private /tmp and disappears on
# restart.
umask 077
# The file lives for the container's lifetime: the script execs s3fs below,
# so an EXIT trap would never run — the credential file disappears when the
# container (and its /tmp) is torn down with the sandbox.
PASSWD_FILE="$(mktemp)"
printf '%s:%s\n' "$access_key" "$secret_key" > "$PASSWD_FILE"

# 3. Build mount options
# s3fs stays in foreground via -f below so mount-proxy can track the process.
# (The `foreground` option is rejected by newer libfuse, do not add it back.)
# allow_other: let business containers access the mount
# use_path_request_style: required by MinIO and other IP-addressed endpoints
# User options come FIRST so provider-injected options win under the
# last-wins duplicate parsing s3fs uses: a user-supplied passwd_file or
# rw must not override the provider's credential file or read-only
# semantics. (Under a first-wins parser this ordering is a no-op.)
MOUNT_OPTS=""
[ -n "$otherOpts" ] && MOUNT_OPTS="${otherOpts},"
MOUNT_OPTS="${MOUNT_OPTS}passwd_file=${PASSWD_FILE},allow_other"

# Endpoint. use_path_request_style is required by MinIO and other
# IP-addressed endpoints, but must NOT be forced for AWS S3 itself:
# newer buckets reject path-style requests, so it is only added together
# with a self-hosted url.
[ -n "$url" ] && MOUNT_OPTS="${MOUNT_OPTS},use_path_request_style,url=${url}"

# Read-only
[ "${readOnly,,}" = "true" ] && MOUNT_OPTS="${MOUNT_OPTS},ro"

# 4. Mount
URL_LOG=""
[ -n "$url" ] && URL_LOG=" url=$(mask_url "$url")"
# Bucket is masked as a final line of defense even though source was
# validated above, in case a future validation change relaxes the charset.
echo "Mounting s3fs: bucket=$(mask_url "$BUCKET")${URL_LOG} mountpoint=$mountpoint"
exec s3fs "$BUCKET" "$mountpoint" -o "$MOUNT_OPTS" -f
