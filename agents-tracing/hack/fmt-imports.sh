#!/bin/bash

set -e

if ! command -v gci &> /dev/null; then
    export PATH="$(go env GOPATH)/bin:$PATH"

    # If still not found, install gci
    if ! command -v gci &> /dev/null; then
        echo "Installing gci..."
        go install github.com/daixiang0/gci@latest
        export PATH="$(go env GOPATH)/bin:$PATH"
    fi
fi

# Format all Go files using gci with proper import ordering
find . -name "*.go" | xargs gci write --skip-generated \
    -s standard \
    -s default \
    -s "prefix(github.com/openkruise/agents)"
