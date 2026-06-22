# E2B SDK-Compatible API Key Design

## Context

E2B SDK `>=2.25.0` validates API keys before sending requests. The SDK
accepts only keys matching:

```text
e2b_[0-9a-f]+
```

`sandbox-manager` historically stores and authenticates raw OpenKruise Agents
keys. Those keys are commonly UUID strings or custom admin keys, such as
`admin-987654321`, and therefore fail the new SDK-side validation before the
server can authenticate them.

The compatibility layer must make server-issued and server-accepted keys pass
the E2B SDK format without changing storage semantics. Existing Secret and
MySQL backends must continue to store, index, and hash the original raw key.

## Goals

- Accept existing raw API keys exactly as before.
- Accept SDK-compatible encoded API keys and map them to the same stored raw key.
- Return SDK-compatible keys from `POST /api-keys`.
- Provide a protected endpoint for clients to retrieve the SDK-compatible form
  of the currently authenticated key.
- Keep Secret storage payloads unchanged.
- Keep MySQL schema and HMAC input unchanged.
- Avoid reflecting raw or compatible API keys in authentication failure
  responses or logs.

## Non-Goals

- Do not change the `keys.KeyStorage` interface.
- Do not change key generation. Backends still generate raw UUID/custom keys.
- Do not migrate existing Secret data or MySQL rows.
- Do not store encoded keys as canonical credentials.
- Do not make generic `e2b_...` keys valid unless they use the OpenKruise
  Agents compatibility encoding and checksum.

## Encoded Key Format

The encoded key is lowercase hex and is shaped to satisfy the SDK validator:

```text
e2b_ + magic + version + length + hex(raw bytes) + checksum
```

Fields:

- `magic`: `6f6b6167`
- `version`: `01`
- `length`: 8 lowercase hex characters, representing the raw byte length
- `hex(raw bytes)`: lowercase hex encoding of the original raw key bytes
- `checksum`: first 8 bytes of
  `SHA256("openkruise-agents/e2b-key-compat/v1" + raw bytes)`, encoded as
  lowercase hex

Example for a raw key `admin-987654321`:

```text
e2b_6f6b6167010000000f61646d696e2d393837363534333231...
```

The checksum prevents accidental decoding of unrelated `e2b_...` values that
happen to share the prefix and hex-only shape. It is not a security boundary;
the raw key remains the credential.

## Key Utility API

Add `pkg/servers/e2b/keys/compat.go` with these helpers:

```go
func IsE2BSDKCompatible(apiKey string) bool
func EncodeForE2BSDK(raw string) string
func DecodeFromE2BSDKCompatible(apiKey string) (string, bool)
func ToStoredRawAPIKey(apiKey string) string
func ConvertToE2BCompatibleCreatedAPIKey(apiKey *models.CreatedTeamAPIKey) *models.CreatedTeamAPIKey
```

`ToStoredRawAPIKey` returns the decoded raw key when the presented key is a
valid OpenKruise-compatible encoded key, and the presented key unchanged
otherwise.

`ConvertToE2BCompatibleCreatedAPIKey` returns a deep copy of the created API key model with
`Key` encoded for the SDK. It must not mutate the object returned by storage.

## Authentication Flow

`CheckApiKey` keeps `X-API-KEY` as the only authentication input.

When key storage is enabled:

1. Read the presented `X-API-KEY` header.
2. Call `keys.ToStoredRawAPIKey` to obtain the lookup key.
3. Call `sc.keys.LoadByKey(ctx, lookupKey)`.
4. If lookup fails, return `401` with a generic `Invalid API Key` message.
5. On success, store the authenticated raw lookup key in request context for
   later presentation by the compatibility endpoint.

Authentication failure responses and logs must not include either the presented
key or the decoded raw key.

When key storage is disabled, `AnonymousUser` behavior is unchanged.

## API Key Creation

`CreateAPIKey` continues to call storage with the existing raw-key semantics:

```go
createdAPIKey, err := sc.keys.CreateKey(ctx, user, keys.CreateKeyOptions{...})
```

The handler returns:

```go
keys.ConvertToE2BCompatibleCreatedAPIKey(createdAPIKey)
```

This means:

- the response `key` field is SDK-compatible;
- Secret storage still stores the raw key;
- MySQL still hashes the raw key;
- cache entries remain based on the raw key or raw-key HMAC;
- callers cannot authenticate with the encoded key unless the server decodes it
  back to the raw key first.

## Compatible Key Endpoint

Add a protected endpoint:

```text
GET /api-keys/compatible
```

Like other E2B routes, it is registered through `RegisterE2BRoute`, so both the
native path and customized `/api` prefixed path are available.

Middleware:

```text
CheckApiKey
```

Response:

```json
{
  "key": "e2b_..."
}
```

The endpoint returns the SDK-compatible form of the currently authenticated
credential. It never returns the raw key. If the request was authenticated with
an encoded key, the endpoint returns the same canonical encoded representation
for the decoded raw key.

## Storage Compatibility

Secret backend:

- Keep Secret keys and JSON payloads based on raw keys.
- Continue loading old raw keys without migration.
- Continue creating raw UUID keys internally.
- Remove raw key values from creation logs.

MySQL backend:

- Keep `team_api_keys.key_hash` as `HMAC-SHA256(pepper, rawKey)`.
- Do not add columns or indexes.
- Continue returning plaintext only from `CreateKey` once.
- Continue returning no plaintext key for DB-loaded keys.

The compatibility endpoint uses the authenticated request context rather than
the storage-returned model's `Key` field. This is required for MySQL, where
loaded keys normally do not include plaintext.

## Security and Logging

The encoded key is reversible by design. It exists only to satisfy the SDK
format validator while preserving server-side storage semantics.

Therefore:

- treat encoded keys as credentials;
- do not log raw keys;
- do not log encoded keys;
- do not include raw or encoded keys in unauthorized responses;
- only log stable non-secret identifiers such as API key ID where needed.

## Compatibility Matrix

| Presented key | Stored credential | Expected result |
| --- | --- | --- |
| raw legacy key | same raw key | authenticate |
| SDK-compatible encoded key | decoded raw key | authenticate |
| unrelated `e2b_...` value | no matching raw key | reject |
| malformed key | no matching raw key | reject |

## Tests

Add table-driven unit tests for:

- SDK format detection.
- Encode/decode round trip for UUID keys, admin keys, empty keys, and UTF-8 byte
  length handling.
- Decode rejection for wrong magic, wrong version, wrong length, checksum
  mismatch, uppercase hex, unrelated `e2b_...`, and trailing bytes.
- Canonicalization of raw, valid encoded, and invalid encoded keys.
- `ConvertToE2BCompatibleCreatedAPIKey` returning a copy and preserving the original raw key.
- `CheckApiKey` authenticating raw keys.
- `CheckApiKey` authenticating encoded keys by decoded raw lookup.
- Unauthorized errors not reflecting raw or encoded keys.
- `POST /api-keys` returning `e2b_[0-9a-f]+` while storage remains raw.
- `GET /api-keys/compatible` returning a compatible key for raw and encoded
  authentication.

Run:

```bash
go test ./pkg/servers/e2b/... ./pkg/servers/e2b/keys/...
```
