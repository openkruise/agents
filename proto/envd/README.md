# envd protocol buffer files

This directory contains the protocol buffer files for E2B envd.

## How to update

If necessary, update this directory using the following steps:

1. Copy the latest files from
   the [envd repository](https://github.com/e2b-dev/infra/tree/main/packages/shared/pkg/grpc/envd)
2. Modify the import path of the copied package in each `*.connect.go` file to
   `"github.com/openkruise/agents/proto/envd/<package>"`, e.g. the `process`
   import in `process.connect.go` and the `filesystem` import in
   `filesystem.connect.go`
