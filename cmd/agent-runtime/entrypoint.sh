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

# Stage both artifacts in one temporary directory inside $ENVD_DIR, then move
# them into place with a single mv. The business container may already run
# $ENVD_DIR/envd-run.sh from a postStart hook, and a rename on the same
# filesystem replaces each destination atomically, so it can never observe a
# partially copied executable.
STAGING_DIR="$(mktemp -d "$ENVD_DIR/.staging.XXXXXX")" || {
    echo "Error: Failed to create staging directory in $ENVD_DIR" >&2
    exit 1
}
trap 'rm -rf "$STAGING_DIR"' EXIT

cp ./envd-run.sh "$STAGING_DIR/" || {
    echo "Error: Failed to stage envd-run.sh in $STAGING_DIR" >&2
    exit 1
}

cp ./envd "$STAGING_DIR/" || {
    echo "Error: Failed to stage envd in $STAGING_DIR" >&2
    exit 1
}

chmod +x "$STAGING_DIR/envd-run.sh" "$STAGING_DIR/envd" || {
    echo "Error: Failed to make staged artifacts executable in $STAGING_DIR" >&2
    exit 1
}

mv "$STAGING_DIR/envd-run.sh" "$STAGING_DIR/envd" "$ENVD_DIR/" || {
    echo "Error: Failed to move staged artifacts to $ENVD_DIR" >&2
    exit 1
}

rmdir "$STAGING_DIR" || {
    echo "Error: Failed to remove staging directory $STAGING_DIR" >&2
    exit 1
}
trap - EXIT

echo "Files successfully copied to $ENVD_DIR"
sleep inf
