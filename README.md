# provider-clickhouse

A [Crossplane](https://crossplane.io) v2 provider that manages SQL access on
**self-hosted ClickHouse**: databases, users, roles, grants and role
membership, as namespaced managed resources. It is not a ClickHouse Cloud
provider; packages with similar names that manage Cloud organisations and
services are a different thing.

- **Token authentication first.** On [Altinity Antalya](https://altinity.com)
  builds the provider logs in with a Google ID token, a token file (for
  example a projected Kubernetes service account token) or a token stored in a
  Secret, and it can create users that log in with JWTs
  (`IDENTIFIED WITH jwt PROCESSOR ...`). Upstream ClickHouse is supported with
  password authentication.
- **Safe by construction.** An identity that can manage users can take over
  existing users. The provider therefore never alters or drops an object it did
  not create (ownership is anchored on the object's UUID and re-checked right
  before every write, on every host of a cluster), never writes to objects
  from `users.xml`/`users.d`, and enforces a closed-by-default policy on every
  `ClusterProviderConfig`, also while resources are being deleted.
- **Crossplane v2 only.** Namespaced managed resources, a namespaced
  `ProviderConfig` owned by the tenant and a cluster-scoped
  `ClusterProviderConfig` owned by the platform team.

> **Status: alpha.** The API is `clickhouse.m.infrabay.io/v1alpha1` and may
> change between minor releases. The full design is in
> [`docs/design.md`](docs/design.md).

## Implementation status

| Kind | Installed by the package | Controller |
|---|---|---|
| `ClusterProviderConfig`, `ProviderConfig`, `ProviderConfigUsage` | yes | yes (usage tracking) |
| `Database` | yes | **yes** |
| `User` | no | planned |
| `Role` | no | planned |
| `Grant` | no | planned |
| `RoleGrant` | no | planned |

Only kinds with a controller are packaged. The API types of the other kinds
are in [`apis/v1alpha1`](apis/v1alpha1) for review, and their CRDs are
generated into [`apis/unreleased-crds`](apis/unreleased-crds), but they are
not installed, so no resource of those kinds can be created and then ignored.

## Compatibility

| | Supported |
|---|---|
| Crossplane | v2.0 and later |
| Upstream ClickHouse | 25.3 and later (password identities) |
| Altinity Antalya | 26.3 and later (password and JWT identities) |

Server differences are detected at run time. On 25.3 a database comment cannot
be changed after creation (the change is reported as `Unsupported`).

## Quick start

This walks through a single-tenant setup on upstream ClickHouse with a
password identity. Every name below is an example.

### 1. Create the provider identity on the server

Define the identity in the server's `users.d` so that the provider can never
modify it, and give it only the grants it needs. This one manages databases
whose names start with `app_`:

```yaml
# /etc/clickhouse-server/users.d/provider-clickhouse.yaml
users:
  provider-clickhouse:
    password_sha256_hex: "<printf '%s' \"$PASSWORD\" | sha256sum>"
    networks: {ip: ["10.0.0.0/8"]}
    profile: default
    grants:
      query:
        - "GRANT CREATE DATABASE, DROP DATABASE, SHOW TABLES ON app_*.*"
        - "GRANT ALTER MODIFY DATABASE COMMENT ON app_*.*"   # omit on 25.3
```

Do not grant `DROP TABLE`: without it a single server refuses to drop a
database that still has tables, whatever the provider decides.

### 2. Install the provider

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-clickhouse
spec:
  package: ghcr.io/infrabay/provider-clickhouse:v0.1.0
---
# The provider uses safe-start: activate its managed resource kinds.
apiVersion: apiextensions.crossplane.io/v1alpha1
kind: ManagedResourceActivationPolicy
metadata:
  name: provider-clickhouse
spec:
  activate: ["*.clickhouse.m.infrabay.io"]
```

### 3. Configure the connection

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: clickhouse-provider
  namespace: crossplane-system
stringData:
  password: change-me
---
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: ClusterProviderConfig
metadata:
  name: clickhouse-main
spec:
  endpoint:
    host: clickhouse.clickhouse.svc.cluster.local   # port 9440 with TLS
  tls:
    enabled: true
    caCertSecretRef: {namespace: crossplane-system, name: clickhouse-ca, key: ca.crt}
  credentials:
    source: PasswordSecret
    passwordSecret:
      username: provider-clickhouse
      passwordSecretRef: {namespace: crossplane-system, name: clickhouse-provider, key: password}
  policy:
    unrestricted: true   # single tenant; see "Multi-tenant clusters"
```

TLS is required unless `allowInsecureTransport: true` is set: the native
protocol sends the password or token in clear text otherwise.

### 4. Create a database

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: Database
metadata:
  name: analytics
  namespace: team-a
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: clickhouse-main}
  # Without Delete, deleting the MR never drops data.
  managementPolicies: ["Observe", "Create", "Update", "LateInitialize"]
  forProvider:
    name: app_analytics
    comment: Analytics events
```

```console
$ kubectl get chdb -n team-a
NAME        READY   SYNCED   EXTERNAL-NAME   ENGINE   AGE
analytics   True    True     app_analytics   Atomic   12s
```

More examples, including `GoogleIDToken` and `TokenFile` identities and the
`DeploymentRuntimeConfig`s they need, are in [`examples/`](examples).

## Provider identities

| `credentials.source` | Use | Allowed on |
|---|---|---|
| `PasswordSecret` | user name plus a password from a Secret | both config kinds |
| `TokenSecret` | a JWT stored in a Secret, re-read on every new connection | both config kinds |
| `GoogleIDToken` | Google ID tokens, from a service account key or, on GKE with Workload Identity, from the metadata server | both; without a key only on `ClusterProviderConfig` |
| `TokenFile` | a JWT file, for example a projected service account token | `ClusterProviderConfig` only |

Before a token from a file or a Secret is sent, the provider checks that it
is a JWT, that its `aud` claim contains the configured audience and none of
the audiences of the provider's own Kubernetes API token, and that it has not
expired. Service account keys are validated before use. On authentication
failure (error 516) the provider fetches a fresh token, rebuilds the
connection pool and retries once.

## Ownership and adoption

A managed resource only changes or drops the ClickHouse object it created.
The claim is kept in annotations bound to the resource's UID, so a manifest
copied with `kubectl get -o yaml` or restored from a backup does not take over
anything. The UUID is read again right before every `ALTER` or `DROP`. When
an object with the same name already exists, the resource reports `NotOwned`
with the object's UUID. Deleting such a resource leaves the object in place
(Warning event `Orphaned`). To take it over, set

```yaml
metadata:
  annotations:
    clickhouse.m.infrabay.io/adopt: "<uuid from the NotOwned message>"
```

On a `ClusterProviderConfig` this also needs `policy.allowAdoption: true`.

Management policies are always honoured: a resource with
`managementPolicies: ["Observe"]` never alters or drops anything.

## Multi-tenant clusters

A `ClusterProviderConfig` policy is closed by default: an empty policy allows
nothing. Name patterns can contain `{namespace}`, which is replaced with the
managed resource's namespace:

```yaml
policy:
  allowedNamespaces: ["team-.*"]   # "{namespace}" is not allowed here
  databaseNamePatterns: ["{namespace}_[a-z0-9_]+"]
  # Replicated databases may always use /clickhouse/databases/{uuid}; other
  # Keeper paths must match this list.
  zooKeeperPathPatterns: ["/clickhouse/databases/{namespace}/.+"]
  userNamePatterns: ["{namespace}_[a-z0-9_]+", "system:serviceaccount:{namespace}:[a-z0-9-]+"]
  roleNamePatterns: ["{namespace}_[a-z0-9_]+"]
  grantableRoles: ["{namespace}_[a-z0-9_]+"]
  allowedAuthentication: [JWT]
  allowedJWTProcessors: [kubernetes]
```

The policy also applies while a resource is being deleted: a name outside it
is never dropped, the resource is released with a Warning event
(`OrphanedByPolicy`). A Replicated database registers a replica under its
Keeper path, so a path chosen by another tenant would join that tenant's
database; that is why custom Keeper paths need `zooKeeperPathPatterns`.

Combine the policy with server-side prefix grants for the identity
(`GRANT CREATE USER ... ON \`team-a_\`*`), and preferably give each tenant its
own identity and `ClusterProviderConfig`. Namespaced `ProviderConfig`s can be
disabled (`--enable-namespaced-provider-configs=false`) or limited to known
endpoints (`--namespaced-endpoint-pattern`). See section 9 of the
[design](docs/design.md#9-security-model).

## Clusters

With `spec.cluster` on the config, `Database` DDL uses `ON CLUSTER`. Before
every `ALTER ... ON CLUSTER` or `DROP ... ON CLUSTER` the provider reads the
database's UUID on every host and refuses the statement if any host has a
database of that name with another UUID. Deleting a cluster `Database` always
needs `forceDrop: true`: the provider cannot prove that remote hosts show it
every table, and `DROP DATABASE ... ON CLUSTER` drops non-empty databases on
remote hosts. Leaving `Delete` out of the management policies is recommended.

The identity then also needs:

```yaml
        - "GRANT CLUSTER ON *.*"            # ON CLUSTER
        - "GRANT SELECT ON system.clusters" # cluster check
        - "GRANT REMOTE ON *.*"             # per-host UUID check (comment changes, Delete)
```

## How this differs

- JWT users and JWT provider identities on Antalya, with the token processor
  always pinned.
- UUID-based ownership and adoption, so name collisions never lead to a
  takeover.
- A closed-by-default policy on shared configs.
- Native Crossplane v2 (namespaced managed resources, safe-start).

[lansweeper-oss/provider-clickhousedbops](https://github.com/lansweeper-oss/provider-clickhousedbops),
an Upjet provider for self-hosted SQL objects, covers settings profiles, row
policies and masking policies, which this provider does not manage yet.

## Documentation

- [`docs/design.md`](docs/design.md): the design of v1alpha1.
- [`DEVELOPMENT.md`](DEVELOPMENT.md): building, testing and releasing.
- [`CONTRIBUTING.md`](CONTRIBUTING.md): how to contribute.
- [`SECURITY.md`](SECURITY.md): how to report a vulnerability.

## License

[Apache License 2.0](LICENSE). See [`NOTICE`](NOTICE) for attributions.
