# Multiple System Keys With Per-Key Scopes

> Created 2026-06-02. This design specifies the **system-key subsystem**: a code-defined
> catalog of cluster-wide system credentials, each carrying its own scope set and
> cross-owner capability. System keys let trusted in-cluster components act across
> sandbox owners without an end-user identity. The first consumer is the sandbox-gateway
> wake path (see the [wake-on-traffic design](2026-05-14-wake-on-traffic-design.md)),
> which uses the connect-scoped key; the catalog currently has that one entry.

## Context

Some trusted in-cluster components must call owner-scoped manager APIs without an
end-user identity. The motivating case: the sandbox-gateway must call `ConnectSandbox`
on a paused sandbox it does not own in order to wake it on traffic. A normal user API
key cannot express this — it is owner-bound — and broadening the admin key into a
cross-owner god credential would be far too coarse.

The system-key subsystem fills this gap with a set of **cluster-wide system
credentials**, each:

- backed by its own dedicated Secret holding a random plaintext value;
- granted a specific set of scopes that gate which routes accept it;
- optionally able to bypass the sandbox owner-equality check (cross-owner);
- mapped to a stable synthesized principal (name + UUID) for audit.

The scope authority is **code-defined**, not data-plane mutable: a static catalog maps
each key to its scopes and capabilities, while Secrets hold only the opaque key value.
Routes opt into accepting system keys per-scope with `AllowSystemKey(scopes...)`, and a
key is accepted on a route only when its granted scopes intersect the route's accepted
scopes.

## Goals

- Support multiple cluster-wide system keys, each with an independently-defined scope
  set.
- Keep the scope authority in code: a static catalog is the single source of truth for
  "which key grants which scopes". Secrets hold only the random key value; the data
  plane cannot tamper with scopes.
- One Secret per key, so per-consumer RBAC stays tightly scoped (a consumer reads only
  its own Secret).
- Per-key synthesized identity (name + stable UUID) so logs/audit can attribute a
  cross-owner call to a specific system key.
- Decouple "cross-owner capability" from "is a system principal" so a future
  non-cross-owner system key is expressible without re-plumbing.

## Non-goals

- **No runtime CRUD / management API for system keys.** The catalog is code-defined and
  provisioned by manifest, not created/deleted at runtime.
- **No database storage for system keys.** They are not stored in the `KeyStorage`
  (Secret/MySQL) backends used for user API keys.
- **No rotation / re-issue.** Each Secret is populated once on first start and is static
  thereafter, unchanged from the wake-on-traffic design.
- **No new business scopes.** `models.SystemAuthConnect` remains the only scope. This
  change delivers the mechanism, not new capabilities.
- **No additional changes to the gateway consumer.** The gateway still reads its single
  Secret (`ConnectSystemKeySecretName`) and is unaware of scopes (scopes are enforced
  manager-side). This multi-key generalization does not further change
  `wake/systemkey.go`, `wake/client.go`, or gateway RBAC.
- **No changes to user API-key authentication or to `ConnectSandbox` /
  `SetSandboxTimeout` semantics.**

## Design

### System key catalog (authority for scopes)

A code-defined catalog in the `keys` package is the single source of truth. Each entry
declares a logical name, a stable principal UUID, its dedicated Secret name, its granted
scopes, and whether it bypasses the owner-equality check.

```go
// keys/systemkey.go
type SystemKeyDef struct {
    Name       string              // logical id; also the synthesized principal name
    ID         uuid.UUID           // stable principal id for audit
    SecretName string              // one Secret per key
    Scopes     []models.SystemAuth // granted scopes
    CrossOwner bool                // bypass the sandbox owner-equality check
}

var systemKeyCatalog = []SystemKeyDef{
    {
        Name:       models.SystemKeyName,            // "system" — the connect credential
        ID:         models.SystemKeyID,              // 00000000-0000-0000-0000-000000000001
        SecretName: ConnectSystemKeySecretName,      // e2b-connect-system-key-store
        Scopes:     []models.SystemAuth{models.SystemAuthConnect},
        CrossOwner: true,                            // consumed by the gateway wake path
    },
}
```

The catalog lives in `keys` (which already imports `models`); no import cycle. Adding a
key later is a catalog entry plus manifest/RBAC — see [Adding a new system key](#adding-a-new-system-key).

### `SystemKeyStore`

`SystemKeyStore` ensures every catalog Secret is populated, loads each plaintext value,
and serves O(1) lookups by value. System keys are static (no rotation), so the lookup
map is built once at startup and read-only thereafter.

```go
type SystemKeyStore struct {
    Namespace string
    Client    client.Client
    APIReader client.Reader

    // retryInterval / retryTimeout default to systemKeyRetryInterval /
    // systemKeyRetryTimeout in NewSystemKeyStore and are overridden in tests to
    // exercise the cap quickly (see Testing).
    retryInterval time.Duration
    retryTimeout  time.Duration

    mu      sync.RWMutex
    byValue map[string]*SystemKeyDef // raw Secret key value (verbatim) -> def
}

func NewSystemKeyStore(c client.Client, r client.Reader, namespace string) *SystemKeyStore

// Lookup reports whether the presented value is a system key, returning its def.
func (s *SystemKeyStore) Lookup(presented string) (*SystemKeyDef, bool) {
    if presented == "" {
        return nil, false
    }
    s.mu.RLock()
    defer s.mu.RUnlock()
    def, ok := s.byValue[presented]
    return def, ok
}
```

Lookup is a plain map read rather than a constant-time comparison: system keys are
256-bit random values and Go maps use a randomized hash seed, so the residual timing
side-channel on a map lookup is negligible and brute force is infeasible regardless.

The map is keyed by the **verbatim** Secret value (no normalization), matching what the
gateway sends. `TrimSpace` is used only to decide whether a Secret is blank, never to
transform the stored/compared value (see [Secret creation and loading](#secret-creation-and-loading)).

### Secret creation and loading

`EnsureKeys` iterates the catalog, ensures each Secret, and builds the lookup map in a
single atomic swap. Entries are ensured sequentially (the catalog is tiny; no parallelism).

```go
func (s *SystemKeyStore) EnsureKeys(ctx context.Context) error {
    if err := validateCatalog(); err != nil { // fail-closed: see Catalog and value validation
        return err
    }
    m := make(map[string]*SystemKeyDef, len(systemKeyCatalog))
    for i := range systemKeyCatalog {
        def := &systemKeyCatalog[i]
        value, err := s.ensureOne(ctx, def)
        if err != nil {
            return err
        }
        if existing, dup := m[value]; dup { // two Secrets resolved to the same value
            return fmt.Errorf("system key %q and %q resolved to the same Secret value; values must be unique", existing.Name, def.Name)
        }
        m[value] = def
    }
    s.mu.Lock()
    s.byValue = m
    s.mu.Unlock()
    return nil
}
```

#### Catalog and value validation (fail-closed)

`EnsureKeys` validates both the static catalog and the loaded values at startup; any
violation aborts manager startup:

- **Catalog uniqueness** (`validateCatalog`, runs before any I/O): `Name`, `ID`, and
  `SecretName` must each be unique across entries.
- **v1 capability constraint**: every entry must have `CrossOwner=true`. A
  `CrossOwner=false` entry is rejected because owner binding for a non-cross-owner system
  principal is not yet modeled (see [Future work](#future-work-non-cross-owner-system-keys)).
- **Value uniqueness** (checked while building the map): if two Secrets resolve to the
  same key value, startup fails. This is not merely a random-collision guard — because
  step (2) loads any pre-existing non-empty Secret value, an operator could pre-seed or
  copy the same value into two Secrets. Without this check the second entry would silently
  overwrite the first in `byValue`, collapsing two distinct scope sets onto one value and
  breaking the per-key isolation boundary.

`ensureOne` enforces the per-key creation rules with a fixed retry policy:

1. If the Secret has no key, randomly generate one and try to `Update` the Secret.
2. If the Secret already has a key, load it into memory.
3. If a step fails (including a conflicting concurrent write), retry at a fixed **1
   second** interval, up to a total of **30 seconds**, then fail.

The defaults below are the production retry policy; `NewSystemKeyStore` assigns them to
the per-store `retryInterval` / `retryTimeout` fields, and tests override those fields
with millisecond values to exercise the cap without real waiting.

```go
const (
    systemKeyRetryInterval = 1 * time.Second  // default for SystemKeyStore.retryInterval
    systemKeyRetryTimeout  = 30 * time.Second // default for SystemKeyStore.retryTimeout
)

func (s *SystemKeyStore) ensureOne(ctx context.Context, def *SystemKeyDef) (string, error) {
    log := klog.FromContext(ctx).WithValues("secret", def.SecretName, "key", def.Name, "namespace", s.Namespace)
    deadline := time.Now().Add(s.retryTimeout)
    var lastErr error
    for {
        value, done, err := s.tryEnsureSecret(ctx, def)
        if done {
            return value, nil
        }
        lastErr = err
        if err != nil {
            log.Error(err, "system-key Secret not ready; will retry")
        }
        if time.Now().After(deadline) {
            return "", fmt.Errorf("ensure system key %q within %s: %w", def.Name, s.retryTimeout, lastErr)
        }
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        case <-time.After(s.retryInterval):
        }
    }
}

// tryEnsureSecret performs one get -> (load | generate+update) cycle.
// done=true means the key value is ready to load into the map.
func (s *SystemKeyStore) tryEnsureSecret(ctx context.Context, def *SystemKeyDef) (value string, done bool, err error) {
    secret := &corev1.Secret{}
    if err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: def.SecretName}, secret); err != nil {
        return "", false, fmt.Errorf("get secret %q: %w", def.SecretName, err)
    }
    raw := string(secret.Data[SystemKeyDataKey])
    if strings.TrimSpace(raw) != "" {
        return raw, true, nil // (2) already populated -> load the value VERBATIM
    }
    generated, err := generateSystemKey() // crypto/rand 32 bytes -> hex
    if err != nil {
        return "", false, fmt.Errorf("generate system key %q: %w", def.Name, err)
    }
    cp := secret.DeepCopy()
    if cp.Data == nil {
        cp.Data = map[string][]byte{}
    }
    cp.Data[SystemKeyDataKey] = []byte(generated)
    if err := s.Client.Update(ctx, cp); err != nil {
        return "", false, fmt.Errorf("update secret %q: %w", def.SecretName, err) // (3) retry
    }
    return generated, true, nil // (1) generated and written
}
```

Behavior notes:

- **The value is stored verbatim.** `TrimSpace` is applied only to test for a blank
  Secret; the returned/stored/compared value is the raw Secret bytes. The gateway reads
  and sends the same raw bytes, so a pre-seeded value containing whitespace (e.g.
  `"abc\n"`) still authenticates.
- **Get uses `APIReader`** (direct API read, bypassing the cache) and **Update uses
  `Client`**. This avoids reading a stale empty Secret from the cache.
- **Multi-replica conflict converges naturally.** If another manager replica writes
  first, this replica's `Update` returns `Conflict`, which is treated as a generic
  retryable failure. The next iteration's `Get` reads the value the other replica wrote
  and loads it. No special-casing of `IsConflict` is needed.
- **Fail-closed.** A missing (not pre-created) Secret or an RBAC error keeps retrying for
  30 seconds and then returns an error, which propagates out of `EnsureKeys` and aborts
  manager startup — a bounded, loud failure rather than an unbounded wait.
- The catalog is ensured sequentially, so worst-case startup blocking is 30s × number of
  keys. With a single key this is at most 30s.

### Authentication and authorization flow

`CheckApiKey` is a dispatcher that splits into two focused helpers. Route opt-in
(`AllowSystemKey`) and the `acceptedSystemScopesFromContext` / `systemScopesIntersect`
helpers gate acceptance; the granted scope set comes from the matched key's `def.Scopes`.

```go
func (sc *Controller) CheckApiKey(ctx context.Context, r *http.Request) (context.Context, *web.ApiError) {
    logger := klog.FromContext(ctx)
    mwLog := logger.WithValues("middleware", "CheckApiKey").V(utils.DebugLogLevel)

    if sc.keys == nil { // auth disabled: validate nothing
        return sc.authorizeUserForSandbox(ctx, r, logger, mwLog, AnonymousUser)
    }

    apiKey := r.Header.Get("X-API-KEY")
    if sc.systemKeys != nil {
        if def, ok := sc.systemKeys.Lookup(apiKey); ok { // (1) O(1) system-key detection
            return sc.checkSystemApiKey(ctx, mwLog, def)
        }
    }
    return sc.checkUserApiKey(ctx, r, logger, mwLog, apiKey)
}

func (sc *Controller) checkSystemApiKey(ctx context.Context, mwLog klog.Logger, def *keys.SystemKeyDef) (context.Context, *web.ApiError) {
    accepted := acceptedSystemScopesFromContext(ctx)
    if len(accepted) == 0 {
        mwLog.Info("system key rejected: route did not opt in", "key", def.Name)
        return ctx, &web.ApiError{Code: http.StatusForbidden, Message: "system key not permitted on this route"}
    }
    if !systemScopesIntersect(accepted, def.Scopes) { // (2) accepted ∩ granted
        mwLog.Info("system key scope denied", "key", def.Name, "accepted", accepted, "granted", def.Scopes)
        return ctx, &web.ApiError{Code: http.StatusForbidden, Message: "system key scope not permitted on this route"}
    }
    ctx = WithSystemCaller(ctx, &SystemCaller{Name: def.Name, ID: def.ID, Scopes: def.Scopes, CrossOwner: def.CrossOwner})
    user := models.NewSystemUser(def.Name, def.ID)
    mwLog.Info("system key accepted", "key", def.Name, "scopes", def.Scopes)
    return context.WithValue(klog.NewContext(ctx, klog.FromContext(ctx).WithValues("user", user.Name)), "user", user), nil
}

func (sc *Controller) checkUserApiKey(ctx context.Context, r *http.Request, logger, mwLog klog.Logger, apiKey string) (context.Context, *web.ApiError) {
    user, ok := sc.keys.LoadByKey(ctx, apiKey)
    if !ok {
        mwLog.Info("failed to load key by API-KEY")
        return ctx, &web.ApiError{Code: http.StatusUnauthorized, Message: fmt.Sprintf("Invalid API Key: %s", apiKey)}
    }
    return sc.authorizeUserForSandbox(ctx, r, logger, mwLog, user)
}
```

Decision flow:

1. Auth disabled (`sc.keys == nil`) → `AnonymousUser`, validate nothing.
2. Lookup the presented key in the system store (O(1)). If found → `checkSystemApiKey`.
3. Otherwise → `checkUserApiKey` (the existing `LoadByKey` + owner-equality path).

**Scope semantics.** A route opts in with `AllowSystemKey(scopes...)` declaring the scope
set it accepts; a system key is allowed iff its granted scopes have a non-empty
intersection with the route's accepted scopes (OR semantics). By convention a route
declares the single scope corresponding to its operation. A route with no
`AllowSystemKey` has an empty accepted set, so every system key is rejected (default
deny). The two 403 messages ("route did not opt in" vs "scope not permitted") share the
same status code and differ only for operability; the granted/accepted sets are logged on
denial to attribute over-privilege attempts to a specific key.

`checkSystemApiKey` does not call `authorizeUserForSandbox`: cross-owner access is granted
downstream via `SystemCaller.CrossOwner`, not by owner equality.

### Decoupling cross-owner from system-principal identity

Two orthogonal concepts replace the single overloaded `allowAnyOwnerCtxKey` boolean,
carried in one context value:

```go
// routes.go (e2b package, alongside the existing ctx helpers)
type SystemCaller struct {
    Name       string
    ID         uuid.UUID
    Scopes     []models.SystemAuth
    CrossOwner bool
}

func WithSystemCaller(ctx context.Context, c *SystemCaller) context.Context
func SystemCallerFromContext(ctx context.Context) *SystemCaller

// AllowAnyOwnerFromContext reports cross-owner capability for the current caller.
func AllowAnyOwnerFromContext(ctx context.Context) bool {
    c := SystemCallerFromContext(ctx)
    return c != nil && c.CrossOwner
}
```

| Concept | Source | Consumer |
|---|---|---|
| Cross-owner capability | `def.CrossOwner` → `SystemCaller.CrossOwner` | `sandbox.go` `GetClaimedSandbox(AllowAnyOwner: AllowAnyOwnerFromContext(ctx))` |
| Is a system principal | `SystemCallerFromContext(ctx) != nil` | `pause_resume.go` `isSystemCaller` → error mapping (`409`) and access-token suppression |

`pause_resume.go` derives `isSystemCaller := SystemCallerFromContext(ctx) != nil` rather
than from cross-owner capability. For a cross-owner key the two coincide, but the
decoupling keeps `isSystemCaller` (error mapping / token suppression) correct
independently of cross-owner capability, so that a future non-cross-owner system key is
still recognized as a system principal. v1 ships only cross-owner keys, and catalog
validation rejects `CrossOwner=false`; the full non-cross-owner path is deferred (see
[Future work](#future-work-non-cross-owner-system-keys)).

`SystemCaller.Scopes` is carried for audit logging and optional defense-in-depth in
handlers; no handler branches on it today.

### Future work: non-cross-owner system keys

`CrossOwner` is modeled as a per-def capability so the mechanism can later express a
system key that does **not** bypass owner equality, but that path is intentionally not
implemented in v1 and is rejected by catalog validation.

The reason it cannot simply be "turned on" is that `checkSystemApiKey` authenticates
every system key as the admin-team system principal, so `getNamespaceOfUser` resolves to
cluster scope and the synthesized identity is the system UUID. With `CrossOwner=false`,
the downstream `GetClaimedSandbox(AllowAnyOwner: false)` would then enforce owner equality
against that system UUID — which no real sandbox owns — so the key could access nothing
useful. A meaningful non-cross-owner system key must instead be bound to a concrete
tenant: either a per-def `Team` / `Namespace`, or a more general `OwnerStrategy`, with
`checkSystemApiKey` routed through team-scoped namespace resolution and the standard owner
check rather than the admin/cluster-scope shortcut. Designing that ownership model, and
the tests that pin it down, is out of scope here.

### Per-key synthesized identity

`models.NewSystemUser` is parameterized so each key yields a distinct principal. It takes
primitive arguments (not `keys.SystemKeyDef`) to avoid a `models` → `keys` import cycle.

```go
// models/api_key.go
func NewSystemUser(name string, id uuid.UUID) *CreatedTeamAPIKey {
    return &CreatedTeamAPIKey{ID: id, Name: name, Team: AdminTeam()}
}
```

`SystemKeyID` and `SystemKeyName` identify the connect catalog entry; its team resolves
to admin (cluster scope via `getNamespaceOfUser`), and `routes_test.go` asserts
`expectUserID == SystemKeyID` for it.

### Controller wiring

```go
// core.go
type Controller struct {
    // ...
    systemKeys *keys.SystemKeyStore // system-key store for CheckApiKey
}

func (sc *Controller) initSystemKey() {
    if sc.keyCfg == nil || sc.cache == nil {
        sc.systemKeys = nil
        return
    }
    sc.systemKeys = keys.NewSystemKeyStore(sc.cache.GetClient(), sc.cache.GetAPIReader(), sc.systemNamespace)
}
```

`Run()` calls `sc.systemKeys.EnsureKeys(ctx)` before serving; an error aborts startup.

## Code Impact

| File | Change |
|---|---|
| `pkg/servers/e2b/keys/systemkey.go` | Replace `SystemKey` with `SystemKeyStore` (incl. injectable `retryInterval`/`retryTimeout` fields) + `SystemKeyDef` (with `CrossOwner`) + `systemKeyCatalog`; add `EnsureKeys`/`ensureOne`/`tryEnsureSecret`/`Lookup`/`validateCatalog` (catalog + value-uniqueness + `CrossOwner=true` fail-closed checks); new retry constants (`systemKeyRetryInterval`, `systemKeyRetryTimeout`) as defaults; remove `Match` and the exponential backoff constants. Keep `ConnectSystemKeySecretName`, `SystemKeyDataKey`, `generateSystemKey`. Replace `SetKeyForUnitTest` with a store-level test injector. |
| `pkg/servers/e2b/models/api_key.go` | `NewSystemUser(name string, id uuid.UUID)`; keep `SystemKeyID`/`SystemKeyName`/`SystemAuth`/`SystemAuthConnect`. |
| `pkg/servers/e2b/routes.go` | Split `CheckApiKey` into dispatcher + `checkSystemApiKey`/`checkUserApiKey`; replace hard-coded `granted` with `def.Scopes`; add `SystemCaller` + `WithSystemCaller`/`SystemCallerFromContext`; reimplement `AllowAnyOwnerFromContext` over `SystemCaller`. `AllowSystemKey`, `acceptedSystemScopesFromContext`, `systemScopesIntersect` unchanged. |
| `pkg/servers/e2b/core.go` | `systemKey *keys.SystemKey` → `systemKeys *keys.SystemKeyStore`; `initSystemKey` builds the store; `Run()` calls `EnsureKeys`. |
| `pkg/servers/e2b/pause_resume.go` | `isSystemCaller := SystemCallerFromContext(ctx) != nil`. |
| `pkg/servers/e2b/sandbox.go` | No change (still calls `AllowAnyOwnerFromContext`). |
| `pkg/sandbox-gateway/wake/*`, `config/sandbox-gateway/rbac.yaml` | No additional change from this multi-key generalization. |

## Deployment and RBAC

The catalog's single entry maps to the `e2b-connect-system-key-store` Secret;
`config/sandbox-manager/rbac.yaml` grants the manager `get`/`update` on it, and the
gateway reads it via its own `get`-only Role (`config/sandbox-gateway/rbac.yaml`).

### Adding a new system key

1. Add a `SystemKeyDef` to `systemKeyCatalog` (name, fresh UUID, dedicated SecretName,
   scopes, `CrossOwner`).
2. If a new capability is needed, add a `models.SystemAuth` constant and annotate the
   target route(s) with `AllowSystemKey(...)`.
3. Add an empty `data: {}` Secret for the new SecretName to the manifests.
4. Grant the manager `get`/`update` on the new SecretName in
   `config/sandbox-manager/rbac.yaml`.
5. Grant the consuming component `get` on the new SecretName (its own Role/RoleBinding),
   mirroring `config/sandbox-gateway/rbac.yaml`.

No change to the authentication core code is required to add a key.

## Testing

- `keys/systemkey_test.go` (table-driven): `ensureOne`/`tryEnsureSecret` — generate when
  empty, load when present, **store the value verbatim** (a pre-seeded `"abc\n"` is
  loaded and matched as `"abc\n"`, not `"abc"`), retry on `Update` conflict/transient
  error then converge on the winner's value, and **fail after the retry cap** when the
  Secret stays missing — exercised with millisecond `retryInterval`/`retryTimeout`
  overrides so the test does not wait in real time. `EnsureKeys` builds the map across the
  catalog and **fails closed** on (a) duplicate catalog `Name`/`ID`/`SecretName`
  (`validateCatalog`), (b) `CrossOwner=false`, and (c) two Secrets resolving to the same
  value. `Lookup` hit/miss/empty.
- `routes_test.go`: system-key branch organized around per-key scopes (key's
  `def.Scopes` intersected with the route's accepted scopes), using the store test
  injector; assert the two 403 paths, system acceptance, and `SystemCaller` population;
  `expectUserID == SystemKeyID` for the connect key.
- `models/api_key_test.go`, `pause_resume_test.go`: follow the `NewSystemUser` signature
  change; `pause_resume_test.go` sets a `SystemCaller` in ctx instead of the raw
  `allowAnyOwnerCtxKey` boolean.
- `core_test.go`: `initSystemKey` builds a `SystemKeyStore`.
