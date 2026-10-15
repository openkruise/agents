---
title: Declarative OAuth2 credential isolation in SecurityProfile
authors:
  - "@delavet"
creation-date: 2026-10-05
last-updated: 2026-10-13
status: provisional
see-also:
  - "/docs/proposals/20260521-traffic-policy-and-security-profile.md"
---

# Declarative OAuth2 credential isolation in SecurityProfile

## Summary

Add `tokenTransformation.type: OAuth2` and an `oauth2` configuration to
SecurityProfile and GlobalSecurityProfile. A trusted egress policy enforcer
(EPE) brokers authorization and token requests, returns placeholder credentials
to workloads, and injects real Bearer credentials into selected resource
requests. Policies select endpoints and client compatibility fields; the
credential service owns third-party credentials and authorization.

This PR publishes the API and generated schemas. The protocol and credential
client foundation was merged in [Agentio #105](https://github.com/openkruise/agentio/pull/105).
The Agentio API dependency update, policy projection, and production activation
will be delivered in a separate PR. Installing these CRDs alone does not enable
OAuth handling in an existing gateway.

## Motivation

An agent may need to use an existing CLI to access user-authorized resources.
Ordinary OAuth clients obtain access tokens and often refresh tokens. When
those credentials enter an untrusted workload, they can be copied and used
outside the intended egress policy. Header injection alone handles resource
requests but does not handle authorization, token polling, refresh responses,
or authorization errors expected by existing clients.

The API must describe OAuth endpoint roles without embedding a vendor's URLs,
credential-service RPC names, or request parameter mappings. It must also
support the small response extensions required by some existing clients.

## API

Configuration lives at `spec.rules[].actions.tokenTransformation.oauth2` in
both profile kinds. Add `OAuth2` to the existing transformation type enum;
`ApiKey` and `AliyunSTS` retain their existing contracts. `oauth2` is required
exactly when the transformation type is `OAuth2`.

`operation` contains exactly one branch: `deviceAuthorization`, `token`, or
`resource`. Each branch accepts only its own options. `customResponse` is
shared configuration alongside `operation`, while its values belong to the
individual rule. There is no global response inheritance or branch override.

```yaml
actions:
  tokenTransformation:
    type: OAuth2
    credentialRef:
      credentialProvider: {name: organization-service}
    oauth2:
      operation:
        token:
          # Optional. Omitted or empty adds no policy grant-type restriction.
          allowedGrantTypes:
            - urn:ietf:params:oauth:grant-type:device_code
            - refresh_token
      # Optional application fields for EPE-generated replies.
      customResponse:
        # Only the Token operation supports success extensions.
        success: {code: 0}
        error: {code: 20050}
        # Overrides matching application fields from error when new
        # authorization is required. Standard OAuth fields stay protected.
        authorizationRequired: {code: 20026}
```

The other two operation configurations are equally small:

```yaml
# Device authorization, including a service-confirmed existing authorization.
oauth2:
  operation:
    deviceAuthorization: {}
---
# Business resource access. Only EPE-generated errors are extended;
# the upstream business response is passed through.
oauth2:
  operation: {resource: {}}
  customResponse:
    error: {code: 20050}
    authorizationRequired: {code: 99991663}
```

Minimal configurations are `operation: {deviceAuthorization: {}}`,
`operation: {token: {}}`, and `operation: {resource: {}}` inside `oauth2`.
The [lark-cli example](../../config/samples/agents_v1alpha1_securityprofile_oauth2.yaml)
combines these with endpoint matches and the client's numeric response codes.
Two Token rules share the same decoding logic; the JSON-specific rule only
selects different response extensions and precedes the general Token rule.

| Field | Required / default | Meaning |
| --- | --- | --- |
| `operation` | Required | Select exactly one endpoint role; branch-count validation belongs to the EPE integration. |
| `operation.deviceAuthorization` | Selected branch; empty object | Creates a device authorization session or accepts an existing authorization confirmed by the service. |
| `operation.token` | Selected branch; may be empty | Processes `grant_type` and returns placeholder token metadata. |
| `operation.token.allowedGrantTypes` | Optional / no additional restriction | A set of nonempty exact strings, including extension grant URIs. Omitted or empty does not bypass protocol support or service authorization. |
| `operation.resource` | Selected branch; empty object | Injects the current real Bearer token and forwards the original resource request. |
| `customResponse` | Optional / no extra fields | Literal public JSON extensions for replies generated by EPE. |
| `customResponse.success` | Optional; Token only | Adds application fields to a successful token reply. |
| `customResponse.error` | Optional; all operations | Adds application fields to OAuth errors. |
| `customResponse.authorizationRequired` | Optional; all operations | Adds fields after a confirmed, non-retryable requirement for new authorization. |

### Request decoding and grants

There is no `requestFormat`: Content-Type selects
`application/x-www-form-urlencoded` or `application/json`, including charset
parameters. The same Token rule can process either encoding. Existing header
matches can select separate rules when clients require different response
extensions. Resource requests do not need their bodies decoded.

`grant_type` is an OAuth request-body parameter, not a policy operation or an
HTTP header. `allowedGrantTypes` only restricts that parameter. Leaving it
unset does not implement additional grants in the credential backend.
Password grants are prohibited by RFC 9700; unsupported grants fail explicitly.

The operation branches are this policy's endpoint-role abstraction. OAuth RFCs
define the corresponding endpoints and flows, not this configuration object.
There is no browser `authorization` branch until that operation has a usable
credential-service contract.

### Response extensions

Some clients require application fields in addition to standard OAuth fields.
For example, lark-cli's JSON refresh handling requires a numeric `code`.
`customResponse` supplies such literals without provider-specific code in EPE.
When omitted, EPE returns its standard OAuth responses.

The three extension values are JSON objects preserved using RawExtension.
EPE rejects fields that would replace `access_token`, `refresh_token`,
`token_type`, `expires_in`, `scope`, `error`, `error_description`, `error_uri`,
`device_code`, `user_code`, `verification_uri`, `verification_uri_complete`,
or `interval`. Resource success replies and DeviceAuthorization success
replies are not extended. `customResponse.success` applies only to Token;
the EPE integration must reject it for other operations.

### Validation ownership

CRD schemas enforce field types and grant set
semantics. No OAuth2 cross-field CEL validation is added in this API PR.
The EPE integration must validate transformation/configuration pairing,
exactly one operation branch, and the Token-only success extension.
Defaults do not create an unselected operation branch.

The credential service decides whether a new authorization is needed. EPE
accepts a service-confirmed authorized session without a new verification URI,
while still validating its explicit authorization state and required session
metadata. The policy does not force a new prompt.

The follow-up EPE integration enforces CredentialProvider selection, Block
failure behavior, protected response fields, request parameters, and verified
HTTPS transport. Policies use a parameter-free CredentialProvider reference;
Secret credentials and ApiKey options do not implement this OAuth contract.
Existing domain, path, method, port, and header matches select traffic eligible
for credential injection. OAuth scope remains a service/provider authorization
constraint and is not replaced by these HTTP matches.

## Request flow and credential boundary

1. The CLI posts to its usual device authorization endpoint. EPE matches the
   DeviceAuthorization rule, asks the credential service for a session, and
   returns public verification information and a broker session handle.
2. The user opens the verification URI and approves access. Token polling
   carries the broker session handle in `device_code`; the credential service
   owns the provider's original device code and real token exchange.
3. Successful polling or refresh returns `access_token: unused` and, where
   applicable, `refresh_token: unused`, plus available scope/expiry metadata.
4. A Resource rule obtains the current authorized credential, overwrites
   Authorization, and forwards the request to the verified HTTPS target.
   The original business body and upstream response are preserved.

Concrete credential-service RPC names and standard parameter conversion belong
inside credential integration, not the persisted policy. Client IDs, client
secrets, and CLI placeholder refresh tokens cannot select or replace the
service's real credentials.

Authorization is **per Sandbox and configured provider**, using the trusted
platform identity. Clients sharing that binding use the current authorization.
Placeholders do not identify a CLI login. Clearing a CLI cache does not revoke
authorization; replacing the binding's authorization changes the credential
used by clients sharing it. Independent CLI sessions, reliable CLI logout,
and concurrent account isolation require a coordinated client/service contract.

The current Identity integration supports device creation, device-code polling,
and current-token retrieval/refresh. It rejects other Token grants instead of
invoking a device API for them. Protocol support for a normalized backend is
not evidence that every deployed provider supports every grant.

EPE can cache Resource credentials only for a positive service-granted cache
TTL, capped by token lifetime and anchored before the service request. Zero,
missing, or negative cache TTL disables caching; there is no fallback to
token lifetime alone. The current Identity contract returns zero, so each
matching resource request still performs a credential lookup. Authorization
and Token operations call the service and invalidate the binding's Resource
cache. These implementation semantics add no cache knob to SecurityProfile.

This contract supports Bearer credentials. Sender-constrained token proofs
require a separate service and transport design.

## Alternatives and review decisions

- A flat operation enum plus unrelated options permits invalid combinations.
  Keep endpoint-specific options in mutually exclusive operation branches.
- An explicit request-format option duplicates Content-Type and can disagree
  with the actual request. Decode using the request header instead.
- Repeating `customResponse` inside every branch duplicates a common concept.
  Keep it beside `operation`, with runtime validation restricting `success`
  to Token.
- RPC actions and parameter maps expose credential integration details in a
  traffic policy. Derive those internally from operation and grant semantics.

## Compatibility and rollout

This is an additive community API change. Community master has no OAuth2 or
OAuth2Device transformation; no legacy alias is introduced. Existing ApiKey
and AliyunSTS policies retain their field shapes and behavior.

Unreleased test policies using operation strings, `requestFormat`, or branch
`customResponse` need conversion before adopting this schema. Select one
operation branch, move only its dedicated options into it, and keep
`customResponse` at the `oauth2` level. Update the matching EPE integration
before activating policies. Generated SDK publication follows the Agents
source repository; agents-api is not edited independently.

## Validation

- Run `make generate manifests` to generate DeepCopy/client code and both
  SecurityProfile CRDs from API source.
- Validate both complete CRDs using Kubernetes schema validation.
- Exercise minimum configurations, selected-device defaults, explicit false,
  grant set constraints, and preserved response objects.
- Validate existing ApiKey and AliyunSTS configurations against the resulting
  transformation schemas.
- In the follow-up Agentio integration, test cross-field constraints,
  CRD-to-neutral-payload projection, and the real lark-cli
  login/poll/refresh/resource path on ACS. Earlier test
  deployment evidence is not a validation of the new public wiring.

## References

- [OAuth 2.0, RFC 6749](https://www.rfc-editor.org/rfc/rfc6749.html)
- [Device Authorization, RFC 8628](https://www.rfc-editor.org/rfc/rfc8628.html)
- [Bearer Token Usage, RFC 6750](https://www.rfc-editor.org/rfc/rfc6750.html)
- [OAuth Security Best Current Practice, RFC 9700](https://www.rfc-editor.org/rfc/rfc9700.html)
