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

# The image ships an <artifact>.md5 next to each artifact, so verification only
# hashes the destination and never reads the source copy. This script is the
# agent-runtime image entrypoint, and that image always provides md5sum.
# destination_matches_digest returns 0 when the destination matches the shipped
# digest, 1 when it differs, and 2 when it cannot be verified; the caller
# republishes on anything but 0.
destination_matches_digest() {
    digest_file=$2
    [ -f "$digest_file" ] || {
        echo "Warning: no usable digest in $digest_file; republishing $1" >&2
        return 2
    }
    read -r expected _ < "$digest_file" && [ -n "$expected" ] || {
        echo "Warning: no usable digest in $digest_file; republishing $1" >&2
        return 2
    }
    actual=$(md5sum "$1") && actual=${actual%% *} && [ -n "$actual" ] || {
        echo "Warning: failed to hash $1; republishing it" >&2
        return 2
    }
    [ "$expected" = "$actual" ]
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
        destination_matches_digest "$destination" "./$artifact.md5"
        rc=$?
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
