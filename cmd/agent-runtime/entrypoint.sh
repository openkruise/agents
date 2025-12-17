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

# Copy files to target directory with error checking
cp ./envd-run.sh "$ENVD_DIR/" || {
    echo "Error: Failed to copy envd-run.sh to $ENVD_DIR" >&2
    exit 1
}

cp ./envd "$ENVD_DIR/" || {
    echo "Error: Failed to copy envd to $ENVD_DIR" >&2
    exit 1
}

chmod +x "$ENVD_DIR/envd-run.sh" || {
    echo "Error: Failed to make $ENVD_DIR/envd-run.sh executable" >&2
    exit 1
}

chmod +x "$ENVD_DIR/envd" || {
    echo "Error: Failed to make $ENVD_DIR/envd executable" >&2
    exit 1
}

echo "Files successfully copied to $ENVD_DIR"
sleep inf
