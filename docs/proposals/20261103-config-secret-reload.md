# Runtime configuration Secret reload

Sandbox-manager watches the Secret selected by `--secret-config=namespace/name`
using its existing Kubernetes rest.Config. A dedicated controller-runtime cache
is restricted to that namespace and the exact `metadata.name`; it runs before
component initialization waits. It has no metrics listener, health listener or
leader election. The deployment identity needs matching get/list/watch access.

The required data keys are `E2B_ADMIN_KEY`, `E2B_KEY_STORAGE_DSN`,
`QUOTA_REDIS_USERNAME` and `QUOTA_REDIS_PASSWORD`. Values are trimmed except for
the administrator key. `E2B_KEY_HASH_PEPPER` is optional and an empty effective
value uses `--e2b-key-hash-pepper-default` (community default `123456`).
Non-empty administrator keys must contain at least 12 Unicode characters; this
minimum is fixed in code. An empty value preserves existing administrator entries
without creating or rotating a key. Deployments can supply their default pepper
through the startup flag.
An explicit per-deployment pepper should be chosen before first MySQL use.

The neutral `configreload` dispatcher delivers one string-map increment per
component. A present key is an explicit update, including an empty value; absent
keys preserve accepted values. Components return Accepted, NotNecessary or
Rejected, or a retryable error. Only confirmed input advances the comparison
baseline. Accepted is returned only after the effect is published and verified,
so confirmation never runs ahead of the component and no convergence window
follows it. Retryable errors retain pending fields, so an uncertain write from A
to B followed by target A still requires confirmation of A. Retries always read
the latest cached Secret. Successfully updated components do not repeat their
work when another component fails. There is no cross-component transaction.

Inside one component the increment is a single atomic unit. The dispatcher
reports one outcome for the whole increment and never accepts per-key results,
so that outcome either confirms every key or leaves the confirmed baseline
unchanged. Retryable errors mark the increment pending; Rejected preserves only
fields already pending from an earlier uncertain attempt. A
component prepares a complete candidate and publishes it only after the whole
increment is validated; a partially applied increment is never reported as
Accepted. Keys that can take effect independently are declared as separate
components. Per-key confirmation is rejected deliberately: it could record a
combination that no implementation ever validated as a whole, and the confirmed
baseline suppresses retries, so that combination would persist. Finer
granularity is expressed by splitting a component, not by widening the result
contract.

Invalid shape, deletion and temporary read failure preserve all live components.
Before the first usable configuration, startup waits without opening business
listeners. Configuration corrections resume startup in the same process;
termination cancels startup without waiting for normal runtime cleanup. The
existing startup probe controls the container's startup budget.

API-key storage keeps one stable public object. Configured administrator-key
intent is separate from store observation: direct store edits are not overwritten
by unrelated configuration updates. Secret storage retains its bootstrap read
boundary and informer refresh protocol. The dispatcher is the sole configuration
readiness gate: startup waits for every component's first confirmation before
starting Manager and HTTP. It selects directly on the dispatcher's one-shot
initialization signal, alongside cancellation and premature source exit.
The API controller requires a configuration source;
the entrypoint supplies either the Secret observer or static flag/env input.
Empty increments leave store observation to the informer. MySQL prepares candidate connections
before publication, fixes the first successful pepper and the complete parsed
DSN identity excluding username/password, and performs schema setup only at
initialization. Administrator-key writes happen only for explicit increments.
Operations pin a runtime state; publication clears authentication caches after
old operations finish so old queries cannot refill a new cache. Old connections
are closed after draining. MySQL uses the same weighted-semaphore pattern as
Redis: operations retain one token through cache writes, while publication
acquires all tokens. Queued operations cannot overtake publication, and waiting
for tokens consumes the operation's context budget, including default list
timeouts. Canceled publication preserves the live state and closes only a newly
prepared connection. MySQL authentication uses both hash and ID caches,
including for the administrator key. Entries expire ten minutes after being
filled; hits never extend that deadline. After the shared row converges and old
reads finish, stale authentication entries therefore expire within one TTL even
under continuous traffic. Cache hits issue no SQL, and expired entries must
reload the store rather than fall back to stale credentials. This does not order
configuration writes across replicas: a delayed initial snapshot can still
temporarily restore an older administrator value. Updates compare the observed
hash before replacing the row, so direct store edits need not maintain an update
timestamp. A zero-row update returns a retryable error, so the next attempt uses
the latest configuration. This preserves the schema and initial bootstrap
behavior; strict rejection of every stale snapshot would require a durable
shared configuration revision fence.

Redis credentials are verified before publication. Only the stable backend's
client changes. The quota manager, breaker, anti-drift driver, subscription and
queued state survive credential changes. Operations finish with the connection
they acquired before it is closed. Existing steady-state fail-open behavior is
preserved for backend failures.

With no Secret reference, flag/environment inputs initialize through the same
Reload methods without starting a watcher. The configuration manager explicitly
uses the process's klog logger so controller-runtime reconcile contexts retain
configuration outcomes and component rejection reasons. Input values and underlying
backend error text are excluded from configuration logs. Pepper migration, changing
database identity, TLS/peer/IdP reload and automatic credential rotation are
outside this change. Administrator rotation revokes the previous key locally;
replicas converge asynchronously, and externally revoked credentials can still
cause an availability window. Restarting or adding a replica retains the
existing initial configured administrator-key bootstrap semantics.

Orchestration must reference a user-managed Secret, pass its fixed policy flags
and grant narrowly scoped access. Application support must be delivered before
orchestration references these flags. Helm-managed runtime Secret ownership must
be migrated safely before removing its old template. This proposal does not
change deployment manifests or perform a deployment.
