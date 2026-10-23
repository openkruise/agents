# Best Practice: Signing Keys for the Agent Identity Provider

This document covers the signing key the agent identity provider uses to mint agent tokens: how to
generate one the provider accepts, how its key ID is derived, and how to rotate it without
invalidating tokens that are still in use.

A sandbox opts into the identity path by setting the `security.agents.kruise.io/agent-name`
annotation. Once it does, the path fails closed: if the identity provider cannot mint a token, the
sandbox does not fall back to a random one.

> The provider reads this key but does not generate it. Key material is yours to create, store and
> rotate, because the provider must never be the only place a key exists.

## Prerequisites

1. `openssl` available locally, for generating keys
2. `kubectl` available with permission to create Secrets in the identity provider's namespace

## Step 1: Generate a signing key

Any of the following will produce a key the provider accepts. ECDSA P-256 is a reasonable default:
the tokens are small and the signatures are fast to verify.

```bash
# ECDSA P-256 (recommended)
openssl ecparam -name prime256v1 -genkey -noout -out signing.key

# ECDSA P-384 or P-521
openssl ecparam -name secp384r1 -genkey -noout -out signing.key
openssl ecparam -name secp521r1 -genkey -noout -out signing.key

# Ed25519
openssl genpkey -algorithm ed25519 -out signing.key

# RSA, 2048 bits minimum
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out signing.key
```

What is accepted, and what each key selects as its JWS algorithm:

| Key | Algorithm | Notes |
|---|---|---|
| ECDSA P-256 | `ES256` | recommended |
| ECDSA P-384 | `ES384` | |
| ECDSA P-521 | `ES512` | |
| Ed25519 | `EdDSA` | |
| RSA, 2048 bits or more | `RS256` | |

The algorithm is chosen from the key, never configured. Anything else is refused at startup rather
than at the first issuance: an unsupported ECDSA curve, an RSA key below 2048 bits, or a key type
the issuer does not know.

The 2048-bit RSA floor is enforced here deliberately. RFC 7518 section 3.3 requires it for `RS256`,
but a verifier that pairs on key type alone will happily accept a signature from a weaker key, so
the issuer declines to create the problem rather than relying on something downstream to notice.

PEM files in PKCS#8, PKCS#1 and SEC1 encodings are all read, so the key does not have to be
converted after generating it with any of the commands above.

## Step 2: Store the key as a Secret

```bash
kubectl create secret generic agent-identity-signing-key \
  --namespace sandbox-system \
  --from-file=tls.key=signing.key
```

Then project it into the identity provider as a read-only volume and point the provider at the
mounted path. Keep this Secret out of every sandbox: a sandbox holds the tokens minted for it, never
the key that minted them.

Restrict access with RBAC so only the identity provider's ServiceAccount can read it. A reader of
this Secret can mint a token for any agent in any namespace, which is strictly more than any single
agent token grants.

## Step 3: Confirm the derived key ID

The key ID is not configured. It is the RFC 7638 JWK thumbprint of the public key, SHA-256,
base64url encoded without padding. Every replica that loads the same key material therefore agrees
on the same `kid` without coordinating, and the `kid` changes if and only if the key does.

After the provider starts, the published key set shows the ID it derived:

```bash
curl -s https://<issuer-host>/jwks.json | jq '.keys[] | {kid, alg, kty}'
```

```json
{
  "kid": "m2B9UMeFGc61Hzo5xbryRGqoE1zqNjgYBjYSstD_Bj4",
  "alg": "ES256",
  "kty": "EC"
}
```

Every token the provider mints carries that `kid` in its JWS header, which is how a verifier picks
the right key out of the set. The discovery document is served alongside it:

```bash
curl -s https://<issuer-host>/.well-known/openid-configuration | jq
```

The `issuer` value in that document must match how callers actually reach the provider, because a
verifier compares it against the `iss` claim exactly. Configure the issuer URL as the externally
reachable address, not the address the pod binds to.

Only public key material is ever published. If a `d` field or any other private parameter appears in
the JWKS output, stop and treat it as an incident.

## Step 4: Rotate the key

A rotation has to overlap. A verifier that has already fetched the key set does not necessarily
refetch it on seeing an unknown `kid`, and tokens signed by the old key stay valid until they
expire. Replacing the key in one step therefore rejects traffic that was perfectly valid when it was
issued.

Rotate in three stages:

1. **Publish the new key alongside the old one.** Generate it, add it to the Secret, and configure
   it as retained. Both keys now appear in the JWKS; the old one still signs.
2. **Switch signing to the new key**, keeping the old one retained. New tokens carry the new `kid`.
   Tokens already in circulation still verify, because their key is still published.
3. **Drop the old key** once every token signed by it has expired. Wait at least one token lifetime
   after stage 2 before doing this.

Shortening stage 3 is the lever for an emergency. Dropping a key immediately invalidates every
outstanding token signed by it, which is the only way to revoke tokens in bulk: the tokens carry no
`jti` and the provider maintains no revocation list, so a leaked token is otherwise valid until it
expires.

That trade-off is deliberate. A revocation list would mean a lookup on every verification, which is
the cost a signed token exists to avoid. Keep token lifetimes short instead.

## Step 5: Verify a minted token

The claims an agent token carries, which a consumer can check without calling back to the provider:

| Claim | Meaning |
|---|---|
| `iss` | the issuer URL, matched exactly |
| `sub` | `agent:<namespace>:<agent-identity-name>` |
| `aud` | `agent-identity-provider` |
| `token_type` | `agent` |
| `namespace` | the sandbox's namespace |
| `agent_identity_name` | the AgentIdentity the sandbox selected |
| `sandbox_uid` | the sandbox's immutable UID |
| `iat`, `nbf`, `exp` | all present; `nbf` equals `iat` |

`namespace`, `agent_identity_name` and `sandbox_uid` are read by the control plane from the Sandbox
object. A caller cannot assert them, which is why a consumer can trust them.

Check `token_type` and `aud` as well as the signature. The agent token and the sandbox gateway
access token may be signed by the same key, so a consumer that validates only the signature would
accept either one wherever it expects the other. Those two claims are what keeps them distinct.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Provider refuses to start, complaining about the key | Unsupported key type, an unsupported ECDSA curve, or an RSA key below 2048 bits |
| Provider refuses to start, complaining about the issuer URL | The issuer URL must be an absolute `https://` URL |
| Verifier rejects tokens after a rotation | The old key was dropped before its tokens expired; re-add it as retained |
| Verifier rejects every token with an issuer mismatch | The configured issuer URL is not how callers actually reach the provider |
| `nbf` rejections on some replicas | Clock skew between the provider and the verifier beyond the verifier's tolerance |
