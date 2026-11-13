# Best Practice: Encryption Keys for the Agent Identity Provider

The identity provider seals the records it persists before storing them. OAuth sessions and user
delegations hold third-party access tokens, refresh tokens, PKCE verifiers and external account
identifiers, and they are kept in Kubernetes Secrets, whose contents are base64 but not encrypted.
Anything that can read the Secret can read the values.

This guide covers the root keys that sealing uses. Signing keys are a separate thing with a
separate lifecycle; see [Signing Keys](agent-identity-signing-keys.md).

## Prerequisites

1. `openssl` available locally, for generating keys
2. A way to place files into the provider's filesystem that does not depend on read access to
   Secrets in `sandbox-system`, which the next section explains

## The one rule that shapes everything else

**The root key must not live in a Secret in `sandbox-system`.**

Session and delegation records are stored as Secrets in that namespace. A root key held in a Secret
beside them is reachable by exactly the same RBAC grant that reaches the ciphertext, so anyone able
to read the records could also read the key that decrypts them. Encrypting under it would protect
nothing.

The provider therefore reads keys from a mounted directory and never from the API server. How the
files arrive is a deployment decision and deliberately out of scope here. A CSI secret driver, a
KMS-backed projection, and a Secret in a namespace only the provider's ServiceAccount can read all
present a directory of files and all keep the key separable from the records.

The security property is **key custody**, not namespace placement. Putting the records somewhere
else would not help if the key followed them.

## Step 1: Generate a key

A root key is 32 raw bytes. AES-256 is not negotiable: a shorter key would silently select AES-128
or AES-192 from the same call, so the provider refuses anything that is not exactly 32 bytes.

```bash
openssl rand 32 > 2026-11
```

The file holds the raw bytes, not PEM and not base64. The file name is the key ID, which is
recorded in every record the key seals, so choose something an operator can reason about later. A
date is the usual choice because it makes the rotation order obvious.

## Step 2: Lay out the directory

```text
/etc/agent-identity/encryption/
├── active     # contains the text "2026-11"
├── 2026-11    # 32 raw bytes, seals new records
└── 2026-05    # 32 raw bytes, retained so older records still open
```

The `active` file contains a key ID, not key material. Every other file is a key.

```bash
printf '2026-11' > active
```

Point the provider at the directory:

```yaml
args:
  - --encryption-key-dir=/etc/agent-identity/encryption
```

Leaving the flag unset disables credential storage entirely, which is the right setting for a
deployment that has not enabled the credential flow yet. A flag pointing at a directory that is not
there is a different matter and stops the process, because the alternative is a provider that comes
up healthy and then cannot serve the first delegation a user tries to establish.

## Step 3: Confirm what loaded

The provider logs the active key ID at startup, and only the ID:

```text
credential record encryption is enabled  activeKeyID=2026-11
```

A record sealed by that key carries the same ID, so the log line and the stored record can be
matched up without decrypting anything. If the flag is unset you get the opposite line, which is
worth checking before concluding that encryption is on:

```text
credential record encryption is disabled: no key directory configured
```

## Step 4: Rotate the key

A rotation has to overlap, for the same reason signing rotation does: records sealed under the old
key stay sealed under it until something re-seals them. Dropping a key while records still need it
makes those records permanently unreadable, which for a delegation means the user has to authorize
again.

Rotate in three stages:

1. **Add the new key file.** Place it in the directory and let it reach every replica. Nothing
   changes yet: `active` still names the old key, which still seals.
2. **Repoint `active` at the new key.** New records seal under it. Existing records still open,
   because the old file is still present and still loaded.
3. **Remove the old key file** once nothing sealed under it remains. For delegations that means
   after the longest-lived one has been re-sealed or deleted, which is not a fixed interval and has
   to be established from the data rather than assumed.

Staging it this way is what makes the rotation safe to abandon halfway. After stage 1 you can
remove the new file again with no effect; after stage 2 you can repoint `active` back.

Unlike a signing key, dropping an encryption key is **not** a revocation lever. It does not
invalidate anything an attacker already holds; it only stops the provider reading its own records.

## A note on key lifetime

Nothing counts how many records a key has sealed. The envelope uses a random 96-bit nonce, and
NIST SP 800-38D puts the safe ceiling for random nonces at 2^32 messages under one key. That
ceiling is a reason to rotate on a schedule rather than only after a suspected compromise.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Provider refuses to start, complaining the directory is empty | The mount did not arrive, or arrived empty |
| Provider refuses to start, complaining about the `active` file | No `active` file, or it names a key with no file beside it |
| Provider refuses to start, complaining a key is the wrong length | A key file is not exactly 32 bytes, often from `echo` adding a newline or from base64 text rather than raw bytes |
| Records fail to open after a rotation | The old key file was removed before everything sealed under it was re-sealed |
| Records fail to open with no rotation involved | The record was moved between namespaces, principals, agents or providers; sealing binds a record to one address and moving it breaks authentication by design |
| Encryption silently not enabled | `--encryption-key-dir` is unset; check the startup line in Step 3 |
