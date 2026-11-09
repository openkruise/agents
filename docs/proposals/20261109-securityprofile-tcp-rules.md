# SecurityProfile TCP destination rules

## Motivation

A domain allowlist in `SecurityProfile.spec.rules` cannot decide the fate of raw
TCP or TLS connections without matching SNI. Requiring a second TrafficPolicy
with duplicated FQDN exceptions makes this common configuration difficult to
maintain, particularly for wildcard domains and changing DNS answers.

Add `spec.tcpRules` to SecurityProfile and GlobalSecurityProfile so application
rules and gateway destination authorization can be configured in one resource.
TrafficPolicy keeps its sandbox-side execution point and existing ingress,
egress, and UDP semantics.

## API and evaluation

`tcpRules` is an ordered list of named rules. Each rule has one or more `match`
entries and a required `action`, either `Allow` or `Deny`. Match entries are ORed;
CIDRs and ports within an entry are ANDed, with entries within each field ORed.
Omitted CIDRs or ports match any address or port. An empty match entry matches
all destinations.

1. Select applicable profiles using their existing namespace, selector, and
   priority ordering. Both rule chains follow that same profile order.
2. Evaluate application rules when possible. HTTP `bypass` or `block`, and
   explicit TLS `Passthrough` or `Deny`, are terminal and skip `tcpRules`.
3. Otherwise evaluate the combined TCP chain. The first matching TCP rule ends
   TCP evaluation across profiles. Only after all profiles miss is traffic allowed.
   An empty TCP chain therefore preserves default allowance.

HTTP authorization uses the actual resolved forwarding IP and port after any
non-terminal request modifications. Unmatched TLS, including missing SNI, raw
TCP, and sniffing timeouts use the original destination. Decrypted non-HTTP
traffic also uses the original destination. A TLS connection permitted as opaque
TCP does not receive HTTP inspection or mutation.

TCP rules execute at the egress gateway. They are subsequent authorization after
application rules, so they do not override explicit `bypass` or TLS `Passthrough`.
Independent platform restrictions still apply.

## Example: permit selected HTTPS requests only

```yaml
apiVersion: agents.kruise.io/v1alpha1
kind: SecurityProfile
metadata:
  name: selected-https
  namespace: demo
spec:
  selector:
    matchLabels:
      app: sandbox
  rules:
  - name: allow-api
    match:
    - domains: ["api.example.com", "*.trusted.example.com"]
      schemes: [https]
      ports: [443]
    actions:
      bypass: true
  - name: deny-other-http
    match:
    - domains: ["*"]
    actions:
      block:
        statusCode: 403
  tcpRules:
  - name: deny-other-connections
    match:
    - cidrs: ["0.0.0.0/0", "::/0"]
    action: Deny
```

The TCP catch-all closes the remaining connection paths, including TLS without
SNI. It does not override the explicitly permitted HTTPS requests.

## Compatibility and scope

The new field is optional; profiles without TCP rules retain their existing
behavior. Action values follow existing capitalized Kubernetes API enums.
This change adds Go types, generated schemas, and documentation only. Enforcement
is provided by a supporting gateway implementation. It adds no Sandbox field,
annotation, controller behavior, or TrafficPolicy changes.
