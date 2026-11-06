#!/bin/sh

# Check if ENVD_DIR environment variable is defined
if [ -z "$ENVD_DIR" ]; then
    echo "Error: ENVD_DIR environment variable is not defined" >&2
    exit 1
fi

# Create target directory if it doesn't exist
mkdir -p "$ENVD_DIR" || {
    echo "Error: Failed to create artifact directory $ENVD_DIR" >&2
    exit 1
}

# Check if required files exist in current directory
if [ ! -f "./envd-run.sh" ]; then
    echo "Error: envd-run.sh file not found in current directory" >&2
    exit 1
fi

if [ ! -f "./envd" ]; then
    echo "Error: envd file not found in current directory" >&2
    exit 1
fi

# $ENVD_DIR is a shared emptyDir that outlives a sidecar restart, so staging
# directories left behind by a killed process have to be removed before staging.
rm -rf "$ENVD_DIR"/.staging.* || {
    echo "Error: Failed to remove stale staging directories in $ENVD_DIR" >&2
    exit 1
}

# COMPARE_CMD is resolved once: cmp is preferred because it compares the files
# directly in a single pass, and a hash tool is the fallback. An empty value
# means no reliable comparison is available, so every artifact is republished
# instead of being kept on an unverified guess.
COMPARE_CMD=""
if command -v cmp >/dev/null 2>&1; then
    COMPARE_CMD="cmp"
elif command -v sha256sum >/dev/null 2>&1; then
    COMPARE_CMD="sha256sum"
elif command -v md5sum >/dev/null 2>&1; then
    COMPARE_CMD="md5sum"
else
    echo "Warning: no cmp, sha256sum or md5sum found; artifacts cannot be compared and will be republished"
fi

# file_hash prints the content hash of a file. It fails when the hash tool fails
# or returns nothing, so a failed comparison never looks like equality.
file_hash() {
    _digest=$("$COMPARE_CMD" "$1" 2>/dev/null) || return 1
    # Tool output is "HASH  PATH"; only the digest is compared.
    _digest=${_digest%% *}
    [ -n "$_digest" ] || return 1
    printf '%s\n' "$_digest"
}

# same_content reports whether $1 and $2 are the same regular file content.
# It returns 0 when they are equal, 1 when they differ, and 2 when the
# comparison itself failed and the caller must republish conservatively.
same_content() {
    src=$1
    dst=$2
    [ -f "$src" ] && [ -f "$dst" ] || return 1
    case "$COMPARE_CMD" in
    cmp)
        cmp -s "$src" "$dst"
        rc=$?
        [ "$rc" -eq 0 ] && return 0
        [ "$rc" -eq 1 ] && return 1
        echo "Warning: cmp failed while comparing $src with $dst (exit $rc); republishing $dst" >&2
        return 2
        ;;
    "")
        echo "Warning: no comparison tool available for $dst; republishing it" >&2
        return 2
        ;;
    esac
    src_hash=$(file_hash "$src") || {
        echo "Warning: failed to hash $src; republishing $dst" >&2
        return 2
    }
    dst_hash=$(file_hash "$dst") || {
        echo "Warning: failed to hash $dst; republishing it" >&2
        return 2
    }
    [ "$src_hash" = "$dst_hash" ] && return 0
    return 1
}

# The image ships an <artifact>.md5 next to each artifact. When one is present,
# verification only hashes the destination, so a kept artifact never reads the
# source copy, and the digest comparison replaces the source/destination
# comparison entirely.
HASH_CMD=""
if command -v md5sum >/dev/null 2>&1; then
    HASH_CMD="md5sum"
fi

file_md5() {
    _md5=$("$HASH_CMD" "$1" 2>/dev/null) || return 1
    _md5=${_md5%% *}
    [ -n "$_md5" ] || return 1
    printf '%s\n' "$_md5"
}

stored_md5() {
    _digest_file=$1
    [ -f "$_digest_file" ] || return 1
    # md5sum writes "HASH  PATH"; only the digest is used.
    read -r _digest _ < "$_digest_file" || return 1
    [ -n "$_digest" ] || return 1
    printf '%s\n' "$_digest"
}

# destination_matches_digest returns 0 when the destination matches the digest
# shipped with the image, 1 when it differs, and 2 when it cannot be verified.
destination_matches_digest() {
    dst=$1
    digest_file=$2
    [ -n "$HASH_CMD" ] || return 2
    expected=$(stored_md5 "$digest_file") || {
        echo "Warning: no usable digest in $digest_file; republishing $dst" >&2
        return 2
    }
    actual=$(file_md5 "$dst") || {
        echo "Warning: failed to hash $dst; republishing it" >&2
        return 2
    }
    [ "$expected" = "$actual" ] && return 0
    return 1
}

# Stage both artifacts in one temporary directory inside $ENVD_DIR, then move
# them into place with a single mv. The business container may already run
# $ENVD_DIR/envd-run.sh from a postStart hook, and a rename on the same
# filesystem replaces each destination atomically, so it can never observe a
# partially copied executable.
#
# $ENVD_DIR outlives a container restart, so an artifact that is already
# published and byte-identical is kept as it is: the copy is skipped and the
# destination inode stays untouched, which matters for a process that is already
# running from that path.
STAGING_DIR="$(mktemp -d "$ENVD_DIR/.staging.XXXXXX")" || {
    echo "Error: Failed to create staging directory in $ENVD_DIR" >&2
    exit 1
}
trap 'rm -rf "$STAGING_DIR"' EXIT

STAGED_ARTIFACTS=""
for artifact in envd-run.sh envd; do
    destination="$ENVD_DIR/$artifact"
    reason=""
    if [ ! -f "$destination" ]; then
        reason="missing"
    else
        rc=1
        if [ -n "$HASH_CMD" ] && [ -f "./$artifact.md5" ]; then
            destination_matches_digest "$destination" "./$artifact.md5"
            rc=$?
        else
            same_content "./$artifact" "$destination"
            rc=$?
        fi
        if [ "$rc" -eq 0 ]; then
            # Keeping the file preserves its inode for a running process, but
            # the surviving copy may have lost its execute bits, so repair them
            # here exactly like the publication path does.
            chmod +x "$destination" || {
                echo "Error: Failed to make kept artifact $destination executable" >&2
                exit 1
            }
            echo "artifact=$artifact action=kept reason=unchanged destination=$destination"
            continue
        elif [ "$rc" -eq 2 ]; then
            reason="unverified"
        else
            reason="changed"
        fi
    fi
    cp "./$artifact" "$STAGING_DIR/" || {
        echo "Error: Failed to stage $artifact in $STAGING_DIR" >&2
        exit 1
    }
    chmod +x "$STAGING_DIR/$artifact" || {
        echo "Error: Failed to make staged $artifact executable in $STAGING_DIR" >&2
        exit 1
    }
    STAGED_ARTIFACTS="$STAGED_ARTIFACTS $artifact:$reason"
done

# Publish the re-copied artifacts in one mv. Each rename onto the same filesystem
# is atomic, so a re-copied artifact never shows up partially written; a kept
# artifact carries no staged copy and is simply not in the list. Publication is
# only reported after mv succeeded.
if [ -n "$STAGED_ARTIFACTS" ]; then
    set --
    for entry in $STAGED_ARTIFACTS; do
        set -- "$@" "$STAGING_DIR/${entry%%:*}"
    done
    mv "$@" "$ENVD_DIR/" || {
        echo "Error: Failed to move staged artifacts to $ENVD_DIR" >&2
        exit 1
    }
    for entry in $STAGED_ARTIFACTS; do
        artifact=${entry%%:*}
        echo "artifact=$artifact action=published reason=${entry#*:} destination=$ENVD_DIR/$artifact"
    done
fi

rmdir "$STAGING_DIR" || {
    echo "Error: Failed to remove staging directory $STAGING_DIR" >&2
    exit 1
}
trap - EXIT

echo "Runtime artifacts prepared in $ENVD_DIR"
sleep inf
