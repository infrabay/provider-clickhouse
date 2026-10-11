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

## Kinds

| Kind | Short name | ClickHouse object |
|---|---|---|
| `ClusterProviderConfig`, `ProviderConfig` | `chcpc`, `chpc` | connection and identity; the cluster kind also holds the tenant policy |
| `Database` | `chdb` | `CREATE DATABASE`, with an explicit UUID |
| `User` | `chuser` | `CREATE USER`: a password user (stored as a salted SHA-256 hash, password published in the connection Secret) or a JWT user pinned to a token processor (Antalya) |
| `Role` | `chrole` | `CREATE ROLE` |
| `Grant` | `chgrant` | privileges of one user or role on one exact target (`*.*`, `db.*`, `db.table`, `db.table(columns)`) |
| `RoleGrant` | `chrolegrant` | membership of one user or role in one role, with or without the admin option |

Every kind is packaged together with its controller (a unit test keeps the
two in sync), and every kind is namespaced.

## Compatibility

| | Supported |
|---|---|
| Crossplane | v2.0 and later |
| Upstream ClickHouse | 25.3 and later (password identities and password users) |
| Altinity Antalya | 26.3 and later (password and JWT identities and users) |

Server differences are detected at run time, and a missing capability fails
only the resource that needs it. CI tests these images:

| | 25.3 (25.3.3.42) | 25.8 | 26.8 | Antalya 26.3 | Antalya 26.6 |
|---|---|---|---|---|---|
| JWT users | no | no | no | yes | yes |
| Database comment change | no | yes | yes | yes | yes |
| Grants of a grantee with wildcard grants (`ON team_*.*`) | no | no | yes | no | no |
| Expiry (`VALID UNTIL`) drift read from | `SHOW CREATE USER` | `SHOW CREATE USER` | `system.users` | `system.users` | `system.users` |

- A JWT `User` on upstream ClickHouse is reported as `Unsupported` and nothing
  is written (the provider's capability probe gets code 62 on 25.3 and 36 on
  25.8 and 26.8).
- A database comment cannot be changed on 25.3; the change is reported as
  `Unsupported`.
- Without `system.grants.is_wildcard` (before 26.8) a wildcard grant cannot be
  told apart from a grant on a database named like its prefix, so a `Grant`
  whose grantee holds wildcard grants is `Unsupported`, and deleting it
  leaves its privileges in place (Warning event `Orphaned`).
- Password users need `writeConnectionSecretToRef`: the connection Secret is
  the provider's record of the password that was last applied (the server
  never shows the hash).

## Quick start

This walks through a single-tenant setup on upstream ClickHouse with a
password identity. Every name below is an example.

### 1. Create the provider identity on the server

Define the identity in the server's `users.d` so that the provider can never
modify it, and give it only the grants it needs. This one manages databases,
users and roles whose names start with `app_`, and may hand out `SELECT`,
`INSERT` and `SHOW TABLES` on those databases:

```yaml
# /etc/clickhouse-server/users.d/provider-clickhouse.yaml
users:
  provider-clickhouse:
    password_sha256_hex: "<printf '%s' \"$PASSWORD\" | sha256sum>"
    networks: {ip: ["10.0.0.0/8"]}
    profile: default
    grants:
      query:
        # Database
        - "GRANT CREATE DATABASE, DROP DATABASE, SHOW TABLES ON app_*.*"
        - "GRANT ALTER MODIFY DATABASE COMMENT ON app_*.*"   # omit on 25.3
        # User, Role, Grant, RoleGrant
        - "GRANT CREATE USER, ALTER USER, DROP USER ON `app_`*"
        - "GRANT CREATE ROLE, DROP ROLE ON `app_`*"
        - "GRANT SHOW USERS, SHOW ROLES ON *.*"
        - "GRANT ROLE ADMIN ON *.*"   # RoleGrant of roles the provider creates
        - "GRANT SELECT ON system.user_directories"
        - "GRANT SELECT ON system.role_grants"
        - "GRANT SELECT ON system.grants"
        - "GRANT SELECT ON system.settings_profile_elements"   # only for User.settingsProfile
        # The ceiling of what Grant may hand out (and revoke).
        - "GRANT SELECT, INSERT, SHOW TABLES ON app_*.* WITH GRANT OPTION"
```

Do not grant `DROP TABLE`: without it a single server refuses to drop a
database that still has tables, whatever the provider decides. The prefix
grants (`` ON `app_`* ``) limit user and role DDL to names that start with
`app_` on the server side, whatever the provider is asked to do.

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

### 5. Create a role, a user and their grants

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: Role
metadata: {name: reader, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: clickhouse-main}
  forProvider:
    name: app_reader
---
# SELECT and INSERT on every table of the database, for the role.
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: Grant
metadata: {name: reader-analytics, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: clickhouse-main}
  forProvider:
    privileges: ["SELECT", "INSERT"]
    databaseRef: {name: analytics}
    roleRef: {name: reader}
---
# A user with a generated password, published with the endpoint in the
# connection Secret "api-clickhouse".
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: User
metadata: {name: api, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: clickhouse-main}
  forProvider:
    name: app_api
    authentication:
      password:
        generate: {}
    defaultDatabase: app_analytics
  writeConnectionSecretToRef: {name: api-clickhouse}
---
# The user becomes a member of the role. New users start with DEFAULT ROLE
# ALL, so the role is active at login.
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: RoleGrant
metadata: {name: api-reader, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: clickhouse-main}
  forProvider:
    memberOfRef: {name: reader}
    userRef: {name: api}
```

References (`databaseRef`, `roleRef`, `userRef`, `memberOfRef`) and
selectors (`databaseSelector`, `roleSelector`, `userSelector`,
`memberOfSelector`, for example with `matchControllerRef` in a composition)
resolve to the ClickHouse names of MRs in the same namespace that use the same
ProviderConfig. Change `generate.rotationToken` to rotate the password. On
Antalya a user can log in with JWTs instead
(`authentication: {jwt: {processor: <token processor>, claims: {...}}}`); its
connection Secret then holds no secret.

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

The server never shows a password, and the provider's record of the password
it applied is the connection Secret, which an adopted user does not have yet.
So adopting a password user with `generate` applies a new password at once
(Warning event `PasswordRegenerated`), and clients that use the old one can no
longer log in. To keep the current password, adopt the user with
`passwordSecretRef` pointing at a Secret that holds it. Once the connection
Secret records it, switching to `generate` keeps it until
`generate.rotationToken` is set or changed.

Grants and role grants have no UUID of their own, so they are bound to their
grantee: a `Grant` or `RoleGrant` grants to and revokes from a user or role
only when a `User` or `Role` in the same namespace, using the same
ProviderConfig, created or adopted it. The grantee's UUID is read again right
before every `GRANT` and `REVOKE`. To manage the grants of an existing user,
adopt it with a `User` first. A role and a user that share a name are
refused, because ClickHouse resolves grantees user-first.

Deleting a `Grant` or `RoleGrant` revokes its privileges or membership, even
if they existed before the resource. To import existing grants, leave
`Delete` out of the management policies:

```yaml
spec:
  managementPolicies: ["Observe", "Create", "Update"]
```

Default roles that a `User` late-initializes from an adopted user are not
filtered by the policy: if they lie outside `grantableRoles`, set
`defaultRoles` explicitly.

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
