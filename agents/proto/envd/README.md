# envd protocol buffer files

This directory contains the protocol buffer files for E2B envd.

## How to update

If necessary, update this directory using the following steps:

1. Copy the latest files from
   the [envd repository](https://github.com/e2b-dev/infra/tree/main/packages/shared/pkg/grpc/envd)
2. Modify the import path for the `process` package in the `process.connect.go` file to
   `"github.com/openkruise/agents/proto/envd/process"`
