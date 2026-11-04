#!/bin/sh

# Check if ENVD_DIR environment variable is defined
if [ -z "$ENVD_DIR" ]; then
    echo "Error: ENVD_DIR environment variable is not defined" >&2
    exit 1
fi

# Create target directory if it doesn't exist
mkdir -p "$ENVD_DIR"

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

# HASH_CMD is resolved once: sha256sum is preferred, md5sum is the fallback, and
# with neither available the comparison degrades to size only (reported once, so
# a container that keeps re-copying large binaries does not do so silently).
if command -v sha256sum >/dev/null 2>&1; then
    HASH_CMD="sha256sum"
elif command -v md5sum >/dev/null 2>&1; then
    HASH_CMD="md5sum"
else
    HASH_CMD=""
    echo "Warning: neither sha256sum nor md5sum found; comparing staged artifacts by size only"
fi

# file_hash prints the content hash of a file, or nothing when no hash tool is
# available.
file_hash() {
    if [ -z "$HASH_CMD" ]; then
        return 0
    fi
    # Tool output is "HASH  PATH"; only the digest is compared.
    $HASH_CMD "$1" 2>/dev/null | awk '{print $1}'
}

# same_content reports whether $1 and $2 are the same regular file content.
# Size is compared first because it is free and rules out most changes.
same_content() {
    [ -f "$1" ] && [ -f "$2" ] || return 1
    [ "$(wc -c < "$1" 2>/dev/null)" = "$(wc -c < "$2" 2>/dev/null)" ] || return 1
    if [ -z "$HASH_CMD" ]; then
        return 0
    fi
    [ "$(file_hash "$1")" = "$(file_hash "$2")" ]
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
    if same_content "./$artifact" "$ENVD_DIR/$artifact"; then
        echo "Keeping existing $ENVD_DIR/$artifact (unchanged)"
        continue
    fi
    cp "./$artifact" "$STAGING_DIR/" || {
        echo "Error: Failed to stage $artifact in $STAGING_DIR" >&2
        exit 1
    }
    chmod +x "$STAGING_DIR/$artifact" || {
        echo "Error: Failed to make staged $artifact executable in $STAGING_DIR" >&2
        exit 1
    }
    STAGED_ARTIFACTS="$STAGED_ARTIFACTS $artifact"
done

# Publish the re-copied artifacts in one mv. Each rename onto the same filesystem
# is atomic, so a re-copied artifact never shows up partially written; a kept
# artifact carries no staged copy and is simply not in the list.
if [ -n "$STAGED_ARTIFACTS" ]; then
    set --
    for artifact in $STAGED_ARTIFACTS; do
        set -- "$@" "$STAGING_DIR/$artifact"
    done
    mv "$@" "$ENVD_DIR/" || {
        echo "Error: Failed to move staged artifacts to $ENVD_DIR" >&2
        exit 1
    }
fi

rmdir "$STAGING_DIR" || {
    echo "Error: Failed to remove staging directory $STAGING_DIR" >&2
    exit 1
}
trap - EXIT

echo "Artifacts ready in $ENVD_DIR"
sleep inf
