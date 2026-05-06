# [Security] Hardening Certificate and Directory Permissions

## Description
The `fsCertWriter` implementation in `pkg/utils/webhookutils/writer/fs.go` currently uses overly permissive filesystem modes when creating directories and writing certificate/key files.

Specifically:
- `os.MkdirAll(dir, 0777)` is used for directory creation.
- `Mode: 0666` is used for all certificate and private key files in the `atomic.FileProjection` map.

Using `0777` and `0666` poses a security risk, especially for private keys (`ca-key.pem`, `key.pem`, `tls.key`), as they should not be world-readable or world-writable. Following security best practices, these should be restricted to the minimum required permissions.

## Proposed Changes
Refactor `pkg/utils/webhookutils/writer/fs.go` to use more secure defaults:
1.  **Directory Creation**: Change `os.MkdirAll(dir, 0777)` to `os.MkdirAll(dir, 0755)`.
2.  **File Permissions**: Update the `certToProjectionMap` function to use:
    -   `0644` for public certificates (`CACertName`, `ServerCertName`, `ServerCertName2`).
    -   `0600` for private keys (`CAKeyName`, `ServerKeyName`, `ServerKeyName2`).
3.  **Clean up**: Resolve the existing TODO comments in the file related to these permissions.

## Rationale
Restricting permissions for cryptographic materials is a standard security hardening measure. Private keys should only be accessible by the owner of the process, and directories should not be world-writable.

## Checklist
- [ ] Implement permission changes in `fs.go`.
- [ ] Verify directory creation permissions on a local cluster.
- [ ] Verify file permissions for generated certs/keys.
- [ ] Ensure all unit tests in the package still pass.
