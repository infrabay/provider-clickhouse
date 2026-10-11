# provider-clickhouse: design for v1alpha1

| | |
|---|---|
| Status | Revision 3, after the code review of the first implementation (ClusterProviderConfigs, ProviderConfigs and the Database controller). Revision 2 followed the design review (ClickHouse semantics, Crossplane security, open-source usability). This document is the reference for implementing v1alpha1. |
| Module and repository | `github.com/infrabay/provider-clickhouse` |
| Package | `ghcr.io/infrabay/provider-clickhouse` |
| API group and version | `clickhouse.m.infrabay.io/v1alpha1` |
| Crossplane | v2 only (namespaced managed resources) |
| License | Apache-2.0 |
| Supported servers | Upstream ClickHouse 25.3 and later (password users). Altinity Antalya 26.3 and later (password and JWT users). See [section 7](#7-server-capability-detection). |

Evidence markers used below:

- **[V]**: verified against a running server during design. Without a qualifier it means verified on Antalya `altinity/clickhouse-server:26.6.4.20001.altinityantalya` and upstream `clickhouse/clickhouse-server:26.8.23.6`. Other versions are named when they were checked too or when they behave differently: **25.3** (`25.3.14.14`), **25.8** (`25.8.33.6`), **A26.3** (`26.3.13.20001.altinityantalya`). "[V all]" means all five.
- **[C]**: taken from reading source code. Not run.
- **[T]**: an assumption. An integration test must confirm it before the feature is called done.

Decisions taken during the review are collected in [section 12](#12-decisions) (D-n). Open questions are marked **OQ-n** where they come up and are collected in [section 13](#13-open-questions).

### What changed in revision 3

- **Deletion and tenancy**: the name policy also applies while an MR is being deleted; outside it the MR is orphaned without SQL (D-25). Management policies are always honoured, and while deleting only the MR's own object is ever reported (D-26) ([4.2](#42-ownership), [5.8](#58-policy-evaluation)).
- **Database**: ownership is re-checked immediately before every write; with `cluster` the UUID is checked on every host before any ON CLUSTER write, and Delete always needs `forceDrop` (D-18). Replicated Keeper paths are policy-controlled (`zooKeeperPathPatterns`, D-27) ([3.4](#34-database), [5.3](#53-database)).
- **Policy patterns** are parsed with `regexp/syntax`, so `|` and unbalanced groups cannot escape the namespace; `{namespace}` is refused in `allowedNamespaces` ([3.3](#33-providerconfig-and-clusterproviderconfig)).
- **Clients**: the 516 renewal never rebuilds a stale configuration ([6.4](#64-516-retry)); the HTTP bearer transport strips every credential header, which makes HTTPS work (D-28, [6.6](#66-deferred-http-transport)).
- **Packaging**: only kinds with a controller are packaged; the other CRDs are generated into `apis/unreleased-crds` ([11.1](#111-layout)).

### What changed in revision 2

- **Ownership** is recorded in annotations bound to the MR's UID, not in `status`. Adoption needs the object's UUID. Every kind uses deterministic external names, and a crash during Create is recoverable ([4.2](#42-ownership)).
- **ClusterProviderConfig policy** is required and closed by default. It gains `databaseNamePatterns`, `allowGlobalGrants`, `allowGrantOption`, `allowAdminOption` and `unrestricted`. Namespaced ProviderConfigs have no policy ([3.3](#33-providerconfig-and-clusterproviderconfig), [5.8](#58-policy-evaluation)).
- **Grant** reasoning now accounts for partial revokes, wildcard rows, grant option, source privileges, unsupported privilege levels, a user and a role with the same name, and deletion of covered Grants ([5.6](#56-grant)).
- **Database** names its UUID in `CREATE DATABASE`, checks existence with `EXISTS DATABASE`, and with `cluster` checks emptiness on every replica ([5.3](#53-database)).
- **User**: `processor` is required for JWT users, `claims` is a YAML object, the password hash is specified exactly, password rotation is explicit, `defaultDatabase` is added, and expiry (`VALID UNTIL`) and `HOST NONE` count as drift ([3.5](#35-user), [5.4](#54-user)).
- **RoleGrant** fields are renamed: `memberOf` is the role being granted, `user`/`role` is the grantee ([3.8](#38-rolegrant)).
- **Transport**: Native only in v1alpha1. TLS is required unless explicitly waived. Token sources check the audience before sending ([6](#6-authentication-and-clients)).
- **Capabilities** are probed lazily and per MR. Upstream 25.3 and 25.8 are supported and tested ([7](#7-server-capability-detection)).
- **Identity scoping**: the server can limit user and role DDL to a name prefix; section 9 now recommends it ([9.2](#92-blast-radius), [9.4](#94-minimal-grants-for-the-provider-identity)).
- **CI and release**: envtest and example validation are mandatory, the integration matrix covers five server versions, and releases are signed ([10](#10-testing-strategy), [11](#11-repository-build-packaging-ci-and-versioning)).

---

## Contents

1. [Goals and non-goals](#1-goals-and-non-goals)
2. [Architecture overview](#2-architecture-overview)
3. [API](#3-api)
4. [External names, ownership and adoption](#4-external-names-ownership-and-adoption)
5. [Reconcile logic](#5-reconcile-logic)
6. [Authentication and clients](#6-authentication-and-clients)
7. [Server capability detection](#7-server-capability-detection)
8. [Topology: replicas and clusters](#8-topology-replicas-and-clusters)
9. [Security model](#9-security-model)
10. [Testing strategy](#10-testing-strategy)
11. [Repository, build, packaging, CI and versioning](#11-repository-build-packaging-ci-and-versioning)
12. [Decisions](#12-decisions)
13. [Open questions](#13-open-questions)
14. [Appendix A: verified ClickHouse behaviour](#appendix-a-verified-clickhouse-behaviour)
15. [Appendix B: server-side configuration examples](#appendix-b-server-side-configuration-examples)

---

## 1. Goals and non-goals

### Goals

- Manage SQL access entities and databases on **self-hosted** ClickHouse from Kubernetes, using Crossplane v2 namespaced managed resources.
- Kinds in v1alpha1: `Database`, `User`, `Role`, `Grant` (privileges) and `RoleGrant` (role membership). Plus `ProviderConfig` (namespaced), `ClusterProviderConfig` (cluster-scoped) and `ProviderConfigUsage`, which crossplane-runtime v2 requires.
- **First-class token (JWT) authentication on Altinity Antalya builds**, in two places:
  - **Provider identity.** The provider logs in with a Google ID token, a token file (for example a projected Kubernetes service account token) or a token stored in a Secret.
  - **Managed users.** The provider creates users with `IDENTIFIED WITH jwt PROCESSOR .. [CLAIMS ..]`. The processor is always pinned.
- **Password authentication on upstream ClickHouse** (and on Antalya), for both the provider identity and managed users. Managed passwords are sent as `sha256_hash`, so the plaintext is never sent to ClickHouse. It is published only to the user's connection Secret.
- **Safe by construction.** A provider identity that can manage users can, in effect, take over existing SQL users (see [section 9](#9-security-model)). The provider therefore:
  - never alters or drops an object it did not create, unless told to adopt that exact object by UUID;
  - never writes to objects defined in `users.xml` / `users.d`, and never modifies its own identity;
  - enforces a closed-by-default policy that the `ClusterProviderConfig` owner controls.
- Drift-free reconciles. Observed state is read structurally from `system.*` tables and compared after normalisation. The provider does not regex-parse `SHOW GRANTS`; it only scans it for wildcard objects on servers that lack `system.grants.is_wildcard` ([5.6](#56-grant)).
- Works across the supported server versions through capability detection. A missing capability fails only the MR that needs it, never the whole ProviderConfig.

### Non-goals (v1alpha1)

- No ClickHouse Cloud API (organisations, services, API keys). Cloud uses a different JWT model (ephemeral users from claims).
- No table, view, dictionary or schema management.
- No settings profiles, quotas, row policies, masking policies, per-user settings, `VALID UNTIL`, `HOST REGEXP/LIKE` or role settings. Users can reference an existing settings profile by name. The rest may come later (**OQ-11**).
- No LDAP, Kerberos, SSL-certificate, SSH-key, bcrypt, HTTP or `no_password` users (see [3.5](#35-user)).
- No management of `token_processors`, `user_directories` or `users.d`. These are server configuration and stay with whatever deploys ClickHouse (Helm, an operator, Terraform).
- No `ON CLUSTER` for access entities, ever (see [section 8](#8-topology-replicas-and-clusters)).
- No source privileges in `Grant` (`S3`, `URL`, `REMOTE`, `FILE`, ..., `READ`/`WRITE ON <source>`) and no wildcard targets (`ON team_*.*`). Existing grants of these kinds are left untouched ([5.6](#56-grant)).
- No HTTP transport in v1alpha1. The design for adding it later is kept in [6.6](#66-deferred-http-transport) (**OQ-18**).
- No legacy cluster-scoped managed resources (Crossplane v1 style).

---

## 2. Architecture overview

```
                   Kubernetes API
  ┌────────────────────────────────────────────────────────────┐
  │ ClusterProviderConfig (connection, auth, policy)            │
  │ ProviderConfig        (connection, auth; tenant-owned)      │
  │ Database  User  Role  Grant  RoleGrant   (namespaced MRs)   │
  └───────────────┬────────────────────────────────────────────┘
                  │ watch / status / annotations / connection Secrets
  ┌───────────────▼────────────────────────────────────────────┐
  │ provider-clickhouse (one active replica, leader election)   │
  │                                                             │
  │  controllers (crossplane-runtime v2 managed.Reconciler,     │
  │  typed ExternalConnector/ExternalClient per kind)           │
  │      │                                                      │
  │  config resolution ── policy ── ownership claims            │
  │      │                                                      │
  │  client cache (one pool per ProviderConfig, ref-counted;    │
  │  capabilities, catalogue, currentUser, server UUIDs)        │
  │      │   token sources: GoogleIDToken | TokenFile |         │
  │      │                  TokenSecret | Password              │
  │  clickhouse-go v2 (Native protocol, GetJWT, TLS)            │
  └──────┬──────────────────────────────────────────────────────┘
         │ SQL (DDL built with quoting helpers;
         │      SELECTs with server-side parameters)
  ┌──────▼──────────────────────────────────────────────────────┐
  │ ClickHouse (Antalya or upstream)                              │
  │  users_xml (read-only)  |  local_directory or replicated      │
  └───────────────────────────────────────────────────────────────┘
```

**Stack** (see [11.2](#112-dependencies-and-versions) for exact pins):

- Go 1.26.
- crossplane-runtime v2.4.2, with shared types from `github.com/crossplane/crossplane/apis/v2`.
- clickhouse-go v2.49.0.
- `google.golang.org/api/idtoken`.
- The provider-template v2 layout.
- Patterns adapted from crossplane-contrib/provider-sql: namespaced ProviderConfig resolution, the mock-DB unit-test style and the grant diff. Their Apache headers are kept and a NOTICE is added.

---

## 3. API

### 3.1 Conventions

- All kinds are in `clickhouse.m.infrabay.io/v1alpha1`.
- Managed resources are namespaced.
  - They embed `xpv2.ManagedResourceSpec` (`writeConnectionSecretToRef`, `providerConfigRef`, `managementPolicies`) and `xpv2.ManagedResourceStatus`.
  - `providerConfigRef` defaults to `{kind: ClusterProviderConfig, name: default}`, which is the crossplane-runtime default. Because the policy is closed by default ([3.3](#33-providerconfig-and-clusterproviderconfig)), a `ClusterProviderConfig` named `default` grants nothing unless its policy says so. The docs advise against naming a shared config `default`.
  - Each kind asserts `var _ resource.ModernManaged = &Kind{}`.
- Namespaced MRs have **no `deletionPolicy`**. Leave `Delete` out of `managementPolicies` to keep the external object (see [9.6](#96-recommended-management-policies)).
- Markers on every MR:
  - `+kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,clickhouse},shortName=<short>`. Short names: `chdb`, `chuser`, `chrole`, `chgrant`, `chrolegrant`. The config kinds use `chpc` and `chcpc`. Kind names such as `User` and `Grant` collide with other providers in `kubectl get`, and the short names avoid that.
  - `+kubebuilder:subresource:status`
  - Printer columns READY, SYNCED and AGE, plus the kind-specific columns listed below.
- **Nil means "not managed".**
  - An optional pointer field left nil is not managed. If `LateInitialize` is in the management policies and the field is late-initializable, it is filled from the server.
  - A field set to a value is enforced.
- **Identifiers.** ClickHouse names can contain `@`, `.` and other characters. Every name field uses `Pattern=^[^\x00-\x1F\x7F]+$` (no control characters) and `MaxLength=255`. Escaping is handled in code ([5.1](#51-sql-safety)). Database names have an extra length rule ([3.4](#34-database)).
- **References.** `Grant` and `RoleGrant` reference other MRs through angryjet resolvers. A field `x *string` has a matching `xRef *xpv2.NamespacedReference` and `xSelector *xpv2.NamespacedSelector`, marked `+crossplane:generate:reference:type=<Kind>`. The resolver extracts the referenced MR's external name, which is the ClickHouse name.
  - **References never leave the MR's namespace.** `xpv2.NamespacedReference` and `xpv2.NamespacedSelector` carry an optional `namespace`. The generated resolver would read the referenced MR from that namespace with the provider's cluster-wide RBAC, before Connect and before any policy check [C]. Every `*Ref` and `*Selector` therefore has a CEL rule that forbids a non-empty namespace (`namespace` is a CEL keyword and is written `__namespace__`):

    ```go
    // +kubebuilder:validation:XValidation:rule="!has(self.userRef) || !has(self.userRef.__namespace__) || self.userRef.__namespace__ == ''",message="cross-namespace references are not allowed"
    // +kubebuilder:validation:XValidation:rule="!has(self.userSelector) || !has(self.userSelector.__namespace__) || self.userSelector.__namespace__ == ''",message="cross-namespace selectors are not allowed"
    ```

    As defence in depth, the controller wraps the generated resolver with `managed.WithReferenceResolver` and rejects a non-empty namespace before resolution.
  - A referenced MR must use the same ProviderConfig (kind and name) as the referencing MR. Otherwise the result is `InvalidSpec`. Plain names are not checked.

### 3.2 Kinds at a glance

| Kind | Scope | ClickHouse object | External name | Late-initialized fields |
|---|---|---|---|---|
| `ClusterProviderConfig` | Cluster | connection + identity + policy | n/a | n/a |
| `ProviderConfig` | Namespaced | connection + identity (tenant-owned, no policy) | n/a | n/a |
| `ProviderConfigUsage` | Namespaced | usage tracking (one type for both PC kinds) | n/a | n/a |
| `Database` | Namespaced | `CREATE DATABASE` | database name | `engine`, `comment` |
| `User` | Namespaced | `CREATE USER` | user name | `settingsProfile`, `defaultRoles`, `defaultDatabase` |
| `Role` | Namespaced | `CREATE ROLE` | role name | — |
| `Grant` | Namespaced | `GRANT <privileges> ON <target> TO <grantee>` | not used | — |
| `RoleGrant` | Namespaced | `GRANT <memberOf> TO <grantee>` | not used | — |

### 3.3 ProviderConfig and ClusterProviderConfig

Both kinds share the connection settings. They differ in three ways:

1. **Secret references.**
   - `ProviderConfig` uses `xpv2.LocalSecretKeySelector`. The Secret is read from the ProviderConfig's own namespace, which is also the MR's namespace.
   - `ClusterProviderConfig` uses `xpv2.SecretKeySelector`, which carries an explicit namespace.

   This follows provider-sql. The provider-template instead lets a namespaced config point at any namespace, and we do not do that.
2. **Ambient identities** are allowed **only** on `ClusterProviderConfig`. These are `TokenFile`, and `GoogleIDToken` without a credentials Secret.
   - A namespaced ProviderConfig is tenant-controlled. Allowing ambient identity there would let a tenant make the provider send its own pod token, or a Google ID token for the provider's service account with an audience the tenant chooses, to an endpoint the tenant chooses.
   - That would leak a credential that may be accepted by other services. See [9.3](#93-provider-identity-sources-and-tenancy).
3. **Policy** exists **only** on `ClusterProviderConfig`, where it is required and closed by default (D-4).
   - A namespaced ProviderConfig is owned by the tenant, together with its credentials. A policy there could only protect the tenant from themselves, and it would double the policy surface. Ownership claims ([4.2](#42-ownership)) already prevent the accidents such a policy would catch (D-3).
   - Operators control namespaced ProviderConfigs with two provider flags: `--enable-namespaced-provider-configs` (default `true`) and `--namespaced-endpoint-pattern` (RE2 on `host:port`, default any). See [9.3](#93-provider-identity-sources-and-tenancy).

```go
// apis/v1alpha1/providerconfig_types.go (sketch)

// Only the native protocol is supported in v1alpha1. The enum keeps its
// type so that HTTP can be added later without a breaking change (OQ-18).
// +kubebuilder:validation:Enum=Native
type Protocol string

const ProtocolNative Protocol = "Native"

// Endpoint is where the provider connects. Usually a Kubernetes Service.
type Endpoint struct {
	// Host name or IP address.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// Port. Defaults to 9000, or 9440 with TLS.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port *int32 `json:"port,omitempty"`
}

// TLSCommon holds TLS settings that do not reference Secrets.
type TLSCommon struct {
	// Enabled turns on TLS. Required unless allowInsecureTransport is true.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`
	// ServerName overrides the name used for SNI and certificate checks.
	// +optional
	ServerName string `json:"serverName,omitempty"`
	// InsecureSkipVerify disables certificate verification. Not allowed with
	// token credentials. Discouraged with passwords: the password then
	// travels to an unauthenticated peer. Use caCertSecretRef instead.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

type LocalTLS struct {
	TLSCommon `json:",inline"`
	// CA bundle (PEM). Defaults to the system roots.
	// +optional
	CACertSecretRef *xpv2.LocalSecretKeySelector `json:"caCertSecretRef,omitempty"`
}

type ClusterTLS struct {
	TLSCommon `json:",inline"`
	// +optional
	CACertSecretRef *xpv2.SecretKeySelector `json:"caCertSecretRef,omitempty"`
}

// Pool limits for the provider's connection pool to this server. Dial and
// read timeouts (5 s, 60 s) and idle connections (maxOpenConns/2) are
// internal defaults, not API.
type Pool struct {
	// +optional
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxOpenConns *int32 `json:"maxOpenConns,omitempty"`
	// Native connections are authenticated only at handshake, so this also
	// bounds how long a revoked or expired provider token keeps working.
	// +optional
	// +kubebuilder:default="30m"
	ConnMaxLifetime *metav1.Duration `json:"connMaxLifetime,omitempty"`
}

// ConnectionSpec is shared by both ProviderConfig kinds.
type ConnectionSpec struct {
	Endpoint Endpoint `json:"endpoint"`
	// +optional
	// +kubebuilder:default=Native
	Protocol Protocol `json:"protocol,omitempty"`
	// Database selected for the provider's session.
	// +optional
	// +kubebuilder:default=default
	DefaultDatabase string `json:"defaultDatabase,omitempty"`
	// +optional
	Pool *Pool `json:"pool,omitempty"`
	// Cluster names a cluster from <remote_servers>. When set:
	//  - Database DDL uses ON CLUSTER <cluster>;
	//  - Database deletion checks emptiness on every replica (section 5.3);
	//  - the provider refuses to write access entities unless they are
	//    stored in a replicated user directory (section 8).
	// Access entities never use ON CLUSTER.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Cluster *string `json:"cluster,omitempty"`
	// AllowInsecureTransport permits a connection without TLS. The native
	// protocol then sends the provider's password or token in clear text.
	// Only for lab setups or traffic that is encrypted below ClickHouse.
	// +optional
	// +kubebuilder:default=false
	AllowInsecureTransport bool `json:"allowInsecureTransport,omitempty"`
}

// +kubebuilder:validation:Enum=PasswordSecret;TokenSecret;GoogleIDToken;TokenFile
type CredentialsSource string

// --- namespaced ProviderConfig credentials ---

// +kubebuilder:validation:XValidation:rule="(self.source == 'PasswordSecret') == has(self.passwordSecret) && (self.source == 'TokenSecret') == has(self.tokenSecret) && (self.source == 'GoogleIDToken') == has(self.googleIDToken)",message="set exactly the block that matches source"
// +kubebuilder:validation:XValidation:rule="self.source != 'TokenFile'",message="TokenFile is only allowed on ClusterProviderConfig"
type LocalCredentials struct {
	Source CredentialsSource `json:"source"`
	// +optional
	PasswordSecret *LocalPasswordCredentials `json:"passwordSecret,omitempty"`
	// +optional
	TokenSecret *LocalTokenSecretCredentials `json:"tokenSecret,omitempty"`
	// On a namespaced ProviderConfig a credentials Secret is mandatory:
	// ambient Google identity is not available to tenants.
	// +optional
	GoogleIDToken *LocalGoogleIDTokenCredentials `json:"googleIDToken,omitempty"`
}

type LocalPasswordCredentials struct {
	// +kubebuilder:validation:MinLength=1
	Username          string                      `json:"username"`
	PasswordSecretRef xpv2.LocalSecretKeySelector `json:"passwordSecretRef"`
}

type LocalTokenSecretCredentials struct {
	// Secret key that holds a JWT. It is re-read (uncached) on every new
	// connection, so external rotation needs no restart.
	SecretRef xpv2.LocalSecretKeySelector `json:"secretRef"`
	// Audience the token must carry in its aud claim. Checked before the
	// token is sent (section 6.1).
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

type LocalGoogleIDTokenCredentials struct {
	// Audience requested in the ID token. It must equal expected_audience
	// of the server's token processor. Use a value unique to this ClickHouse
	// deployment, for example its URL.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
	// Service account key JSON. Validated before use (section 6.1).
	CredentialsSecretRef xpv2.LocalSecretKeySelector `json:"credentialsSecretRef"`
}

// --- ClusterProviderConfig credentials ---

// +kubebuilder:validation:XValidation:rule="(self.source == 'PasswordSecret') == has(self.passwordSecret) && (self.source == 'TokenSecret') == has(self.tokenSecret) && (self.source == 'GoogleIDToken') == has(self.googleIDToken) && (self.source == 'TokenFile') == has(self.tokenFile)",message="set exactly the block that matches source"
type ClusterCredentials struct {
	Source CredentialsSource `json:"source"`
	// +optional
	PasswordSecret *ClusterPasswordCredentials `json:"passwordSecret,omitempty"`
	// +optional
	TokenSecret *ClusterTokenSecretCredentials `json:"tokenSecret,omitempty"`
	// +optional
	GoogleIDToken *ClusterGoogleIDTokenCredentials `json:"googleIDToken,omitempty"`
	// +optional
	TokenFile *TokenFileCredentials `json:"tokenFile,omitempty"`
}

type ClusterPasswordCredentials struct {
	// +kubebuilder:validation:MinLength=1
	Username          string                 `json:"username"`
	PasswordSecretRef xpv2.SecretKeySelector `json:"passwordSecretRef"`
}

type ClusterTokenSecretCredentials struct {
	SecretRef xpv2.SecretKeySelector `json:"secretRef"`
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

type ClusterGoogleIDTokenCredentials struct {
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
	// Optional service account key JSON. When omitted, Application Default
	// Credentials are used: on GKE with Workload Identity, the metadata server.
	// +optional
	CredentialsSecretRef *xpv2.SecretKeySelector `json:"credentialsSecretRef,omitempty"`
}

type TokenFileCredentials struct {
	// Absolute path inside the provider container, for example a projected
	// service account token mounted through a DeploymentRuntimeConfig.
	// The file is re-read on every new connection. Paths under
	// /var/run/secrets/kubernetes.io/serviceaccount/ are refused: that is the
	// provider's own Kubernetes API credential.
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`
	// Audience the token must carry in its aud claim, for example the
	// audience of the projected volume ("clickhouse"). Checked before the
	// token is sent (section 6.1).
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

// +kubebuilder:validation:Enum=Password;JWT
type AuthenticationMethod string

// ClusterPolicy limits what managed resources may do through a
// ClusterProviderConfig. It is closed by default: an empty or absent list
// allows nothing, and every boolean defaults to false. Write [".*"] (or set
// unrestricted) to allow everything.
//
// Pattern semantics (allowedNamespaces and every *Patterns / grantableRoles
// entry):
//  - RE2, implicitly anchored (^(?:p)$);
//  - "{namespace}" is replaced with the MR's namespace, regexp-quoted;
//  - "{namespace}" may appear at most once. The text before it must be a
//    plain literal (no regexp operators), and every match of the regexp
//    after it must start with a literal character that cannot occur in a
//    namespace name (anything but a-z, 0-9 and '-'), in every alternative,
//    for example _ @ \. : /
//    This makes the namespace part of a name unambiguous (D-5). Each regexp
//    part is parsed on its own (regexp/syntax) and then wrapped in its own
//    group, ^(?:<prefix><namespace>(?:<suffix>))$, so an unbalanced ")" or a
//    top-level "|" cannot escape the anchors or the namespace. Static
//    patterns are parsed on their own before ^(?:p)$ is added, too;
//  - "{namespace}" is not allowed in allowedNamespaces: it would match the
//    namespace against itself, i.e. every namespace (rejected in CEL and in
//    Go).
//    A pattern that breaks these rules makes the ClusterProviderConfig
//    invalid.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.unrestricted) && self.unrestricted) || [has(self.allowedNamespaces), has(self.userNamePatterns), has(self.roleNamePatterns), has(self.databaseNamePatterns), has(self.zooKeeperPathPatterns), has(self.grantableRoles), has(self.allowedAuthentication), has(self.allowedJWTProcessors)].all(x, !x)",message="unrestricted cannot be combined with restriction lists"
type ClusterPolicy struct {
	// Unrestricted allows every namespace, name, role, authentication method
	// and processor, global grants, grant option and admin option. Meant for
	// single-tenant installs. Adoption still needs allowAdoption.
	// +optional
	Unrestricted bool `json:"unrestricted,omitempty"`
	// Namespaces whose MRs may use this ClusterProviderConfig.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self.all(x, !x.contains('{namespace}'))",message="{namespace} is not allowed in allowedNamespaces"
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`
	// Names that User may create, manage or observe. Also applies to user
	// grantees of Grant and RoleGrant.
	// +optional
	UserNamePatterns []string `json:"userNamePatterns,omitempty"`
	// Names that Role may create, manage or observe. Also applies to role
	// grantees of Grant and RoleGrant.
	// +optional
	RoleNamePatterns []string `json:"roleNamePatterns,omitempty"`
	// Names that Database may create, manage or observe, and databases a
	// Grant may target.
	// +optional
	DatabaseNamePatterns []string `json:"databaseNamePatterns,omitempty"`
	// Keeper paths a Replicated Database may use besides the default
	// "/clickhouse/databases/{uuid}", which is always allowed (D-27).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	ZooKeeperPathPatterns []string `json:"zooKeeperPathPatterns,omitempty"`
	// Roles that RoleGrant may grant (memberOf) and that User.defaultRoles
	// may list.
	// +optional
	GrantableRoles []string `json:"grantableRoles,omitempty"`
	// Authentication methods User may use.
	// +optional
	// +listType=set
	AllowedAuthentication []AuthenticationMethod `json:"allowedAuthentication,omitempty"`
	// Exact token processor names that jwt Users may pin. The server accepts
	// unknown processors at DDL time, and those users then cannot log in.
	// +optional
	// +listType=set
	AllowedJWTProcessors []string `json:"allowedJWTProcessors,omitempty"`
	// AllowGlobalGrants lets Grant use database "*" (ON *.*).
	// +optional
	AllowGlobalGrants bool `json:"allowGlobalGrants,omitempty"`
	// AllowGrantOption lets Grant set withGrantOption. A grantee holding a
	// grant option can grant and revoke outside the provider (section 9.3).
	// +optional
	AllowGrantOption bool `json:"allowGrantOption,omitempty"`
	// AllowAdminOption lets RoleGrant set adminOption, with the same caveat.
	// +optional
	AllowAdminOption bool `json:"allowAdminOption,omitempty"`
	// AllowAdoption lets MRs take over pre-existing objects with the
	// clickhouse.m.infrabay.io/adopt annotation (section 4.2).
	// +optional
	AllowAdoption bool `json:"allowAdoption,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.tls) && self.tls.enabled) || (has(self.allowInsecureTransport) && self.allowInsecureTransport)",message="tls.enabled must be true unless allowInsecureTransport is true: the native protocol sends the password or token in clear text"
// +kubebuilder:validation:XValidation:rule="self.credentials.source == 'PasswordSecret' || !has(self.tls) || !has(self.tls.insecureSkipVerify) || !self.tls.insecureSkipVerify",message="insecureSkipVerify is not allowed with token credentials"
type ProviderConfigSpec struct {
	ConnectionSpec `json:",inline"`
	// +optional
	TLS         *LocalTLS        `json:"tls,omitempty"`
	Credentials LocalCredentials `json:"credentials"`
}

// Same two rules as ProviderConfigSpec.
type ClusterProviderConfigSpec struct {
	ConnectionSpec `json:",inline"`
	// +optional
	TLS         *ClusterTLS        `json:"tls,omitempty"`
	Credentials ClusterCredentials `json:"credentials"`
	// Required. Write {unrestricted: true} for a single-tenant install.
	Policy ClusterPolicy `json:"policy"`
}

type ProviderConfigStatus struct {
	xpv2.ProviderConfigStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,provider,clickhouse},shortName=chpc
// +kubebuilder:printcolumn:name="ENDPOINT",type="string",JSONPath=".spec.endpoint.host"
// +kubebuilder:printcolumn:name="SOURCE",type="string",JSONPath=".spec.credentials.source"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
type ProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProviderConfigSpec   `json:"spec"`
	Status            ProviderConfigStatus `json:"status,omitempty"`
}

// ClusterProviderConfig: same markers with scope=Cluster and shortName=chcpc.

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,provider,clickhouse}
// +kubebuilder:printcolumn:name="CONFIG-KIND",type="string",JSONPath=".providerConfigRef.kind"
// +kubebuilder:printcolumn:name="CONFIG-NAME",type="string",JSONPath=".providerConfigRef.name"
// +kubebuilder:printcolumn:name="RESOURCE-KIND",type="string",JSONPath=".resourceRef.kind"
// +kubebuilder:printcolumn:name="RESOURCE-NAME",type="string",JSONPath=".resourceRef.name"
type ProviderConfigUsage struct {
	metav1.TypeMeta               `json:",inline"`
	metav1.ObjectMeta             `json:"metadata,omitempty"`
	xpv2.TypedProviderConfigUsage `json:",inline"`
}
```

**Validation outside CEL.** The config controller compiles every policy pattern and checks the `{namespace}` placement rule. An invalid pattern makes every MR that uses the config fail with `InvalidConfig`, and the controller emits a Warning event on the config. Writing the error into the config's status needs a health controller (**OQ-7**).

**What is in v1 and what is left out.**

- **In v1, as above:**
  - endpoint, TLS, credentials, default database, `maxOpenConns`, `connMaxLifetime`, `cluster` and `allowInsecureTransport`;
  - on the cluster kind, the full `ClusterPolicy`.
- **Left out:**
  - the HTTP protocol (**OQ-18**);
  - mTLS client certificates;
  - Google service account impersonation and `external_account` credentials (**OQ-9**);
  - an OIDC client-credentials token source for non-Google identity providers (**OQ-19**);
  - multiple endpoints;
  - overriding the endpoint published in connection Secrets (**OQ-8**);
  - a label selector for namespaces (**OQ-25**);
  - per-config custom condition types.

**Example: a shared ClusterProviderConfig.** Tenants get names prefixed with their namespace. Workloads log in with their own Kubernetes service account tokens through a `kubernetes` token processor (Appendix B), so their user names start with `system:serviceaccount:<namespace>:`.

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: ClusterProviderConfig
metadata:
  name: shared-clickhouse
spec:
  endpoint: {host: clickhouse.clickhouse.svc.cluster.local}   # port 9440 (TLS)
  tls:
    enabled: true
    caCertSecretRef: {namespace: crossplane-system, name: clickhouse-ca, key: ca.crt}
  credentials:
    source: GoogleIDToken
    googleIDToken:
      audience: https://clickhouse.example.com   # unique per deployment; = expected_audience
  cluster: my-cluster
  policy:
    allowedNamespaces: ['team-.*']
    userNamePatterns:
      - '{namespace}_[a-z0-9_]+'
      - 'system:serviceaccount:{namespace}:[a-z0-9-]+'
    roleNamePatterns: ['{namespace}_[a-z0-9_]+']
    databaseNamePatterns: ['{namespace}_[a-z0-9_]+']
    grantableRoles: ['{namespace}_[a-z0-9_]+', 'analytics_reader']
    allowedAuthentication: [JWT]
    allowedJWTProcessors: [kubernetes]
```

`allowGlobalGrants`, `allowGrantOption`, `allowAdminOption` and `allowAdoption` stay false. Google service account emails such as `svc@my-project.iam.gserviceaccount.com` cannot carry a namespace prefix. Tenants that need such users get their own `ClusterProviderConfig` with explicit patterns, ideally with their own provider identity ([9.3](#93-provider-identity-sources-and-tenancy)).

**Example: a single-tenant ClusterProviderConfig** on upstream ClickHouse with a password identity:

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: ClusterProviderConfig
metadata:
  name: clickhouse-main
spec:
  endpoint: {host: clickhouse.clickhouse.svc.cluster.local}
  tls:
    enabled: true
    caCertSecretRef: {namespace: crossplane-system, name: clickhouse-ca, key: ca.crt}
  credentials:
    source: PasswordSecret
    passwordSecret:
      username: provider-clickhouse
      passwordSecretRef: {namespace: crossplane-system, name: clickhouse-provider, key: password}
  policy: {unrestricted: true}
```

### 3.4 Database

```go
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed after creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.engine) || has(self.engine)",message="engine cannot be removed"
type DatabaseParameters struct {
	// Name in ClickHouse. Defaults to the effective external name (4.1).
	// Immutable. The server stores the name in file names, escaped
	// (non-alphanumeric bytes become %XX); the escaped form plus ".sql.tmp"
	// must fit in 255 bytes. The provider checks this in Go (InvalidSpec).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	// +kubebuilder:validation:XValidation:rule="self != '*'",message="'*' is not a valid database name"
	Name *string `json:"name,omitempty"`

	// Engine. Immutable. When omitted, the ENGINE clause is omitted, the
	// server default (Atomic) is used, and the engine is late-initialized.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="engine is immutable"
	Engine *DatabaseEngine `json:"engine,omitempty"`

	// Comment. An empty string clears the comment. Changing it needs
	// ALTER DATABASE ... MODIFY COMMENT, which upstream 25.3 lacks: there a
	// change is reported as Unsupported (section 5.3).
	// +optional
	Comment *string `json:"comment,omitempty"`

	// ForceDrop lets Delete drop a database that still has tables, or whose
	// emptiness cannot be verified on every replica.
	// Default false: Delete refuses and the MR stays in deletion until fixed.
	// +optional
	ForceDrop *bool `json:"forceDrop,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.name == 'Replicated' || !has(self.replicated)",message="replicated is only valid with name: Replicated"
type DatabaseEngine struct {
	// +kubebuilder:validation:Enum=Atomic;Replicated
	Name string `json:"name"`
	// +optional
	Replicated *ReplicatedEngine `json:"replicated,omitempty"`
}

// Every value is sent as a quoted string literal. Macros such as {uuid},
// {shard} and {replica} are expanded by the server when the database is
// attached. The {uuid} macro works because the provider always sends an
// explicit UUID (section 5.3). system.databases.engine_full keeps the
// arguments exactly as written [V], so they are compared verbatim.
// On a ClusterProviderConfig that is not unrestricted, zooKeeperPath must be
// the default or match policy.zooKeeperPathPatterns (D-27).
type ReplicatedEngine struct {
	// +kubebuilder:default="/clickhouse/databases/{uuid}"
	ZooKeeperPath string `json:"zooKeeperPath,omitempty"`
	// +kubebuilder:default="{shard}"
	ShardName string `json:"shardName,omitempty"`
	// +kubebuilder:default="{replica}"
	ReplicaName string `json:"replicaName,omitempty"`
}

type DatabaseObservation struct {
	// UUID from system.databases.uuid. Informational; ownership is recorded
	// in annotations (section 4.2).
	ID         string `json:"id,omitempty"`
	Engine     string `json:"engine,omitempty"`
	EngineFull string `json:"engineFull,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

// Printer columns: READY, SYNCED, EXTERNAL-NAME, ENGINE (.status.atProvider.engine), AGE.
```

The engine is a closed enum with structured arguments, never a raw SQL fragment. A free-form engine string would be an injection point. More engines can be added later as new enum values.

**Keeper paths are a tenancy boundary** (D-27). A Replicated database registers a replica under its Keeper path. A tenant that chooses another tenant's path registers a replica of *that* database: the victim's next DDL then waits for the foreign replica and fails with 159 `TIMEOUT_EXCEEDED` ("not finished on 1 of 2 hosts"), and a host that lacks the victim's replica would copy its table definitions and, through `ReplicatedMergeTree`, its data [V 26.8 with embedded Keeper]. A path nested under another database's node (`.../victim/replicas/{uuid}`) is just as harmful, so "contains `{uuid}`" is not enough. On a `ClusterProviderConfig` that is not `unrestricted`, the provider therefore allows only the default `/clickhouse/databases/{uuid}`, which expands to a path unique per database UUID, and paths that match `policy.zooKeeperPathPatterns`, for example `/clickhouse/databases/{namespace}/.+`. The check runs in Create, before the CREATE registers anything. Residual risk: the UUID comes from the `owned-id` annotation, which the tenant can edit; the server rejects a UUID that is already in use on a host (57), so a forged UUID can only reach hosts that lack that database but share the Keeper. Namespace-anchored `zooKeeperPathPatterns` close that gap.

### 3.5 User

```go
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed after creation"
type UserParameters struct {
	// Name in ClickHouse, for example "svc@my-project.iam.gserviceaccount.com".
	// Defaults to the effective external name (4.1). Immutable. The provider
	// never splits it on '@' or '.'.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name *string `json:"name,omitempty"`

	Authentication UserAuthentication `json:"authentication"`

	// Allowed client addresses (IP or CIDR; mask notation such as
	// 10.0.0.0/255.0.0.0 is accepted). With hostNames, this forms the HOST
	// clause. If both are empty the user may connect from ANY host. Hosts are
	// always enforced, never late-initialized. 0.0.0.0/0 or ::/0 together
	// with hostNames is InvalidSpec: the server would drop the names [V].
	// +optional
	// +listType=set
	HostIP []string `json:"hostIP,omitempty"`
	// Exact client host names (HOST NAME). Compared case-sensitively, as the
	// server stores them [V].
	// +optional
	// +listType=set
	HostNames []string `json:"hostNames,omitempty"`

	// Existing settings profile to inherit. The profile must already exist
	// (users.xml profiles or SQL); otherwise the server returns 180.
	//  - nil: not managed (late-initialized);
	//  - "": no profile (SETTINGS NONE);
	//  - "x": exactly profile x.
	// Setting a profile replaces all per-user settings elements.
	// +optional
	SettingsProfile *string `json:"settingsProfile,omitempty"`

	// Database selected when a client does not name one.
	//  - nil: not managed (late-initialized);
	//  - "": DEFAULT DATABASE NONE;
	//  - "db": DEFAULT DATABASE `db`.
	// The server does not check that the database exists [V all]; a missing
	// default database makes logins fail, so the docs advise pairing it with
	// a Database MR.
	// +optional
	DefaultDatabase *string `json:"defaultDatabase,omitempty"`

	// Which granted roles are active at login. nil means not managed
	// (late-initialized). Membership itself is managed with RoleGrant.
	// +optional
	DefaultRoles *DefaultRoles `json:"defaultRoles,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.password) != has(self.jwt)",message="set exactly one of password or jwt"
type UserAuthentication struct {
	// +optional
	Password *PasswordAuthentication `json:"password,omitempty"`
	// JWT users are supported only on servers that accept
	// IDENTIFIED WITH jwt (Altinity Antalya).
	// +optional
	JWT *JWTAuthentication `json:"jwt,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.passwordSecretRef) != has(self.generate)",message="set exactly one of passwordSecretRef or generate"
type PasswordAuthentication struct {
	// Password taken from a Secret in the MR's namespace. Changing the
	// Secret's value changes the password.
	// +optional
	PasswordSecretRef *xpv2.LocalSecretKeySelector `json:"passwordSecretRef,omitempty"`
	// Generate a random password and publish it in the connection Secret.
	// +optional
	Generate *GeneratePassword `json:"generate,omitempty"`
}

type GeneratePassword struct {
	// Length in characters, drawn from [A-Za-z0-9] with crypto/rand.
	// +optional
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=16
	// +kubebuilder:validation:Maximum=128
	Length *int32 `json:"length,omitempty"`
	// RotationToken: set or change it to any new value to rotate the
	// password. The token that was last applied is stored in the connection
	// Secret (section 5.4).
	// +optional
	// +kubebuilder:validation:MaxLength=63
	RotationToken *string `json:"rotationToken,omitempty"`
}

type JWTAuthentication struct {
	// Name of a token_processors entry. Required: without it the server
	// accepts a token from ANY configured processor whose username claim
	// equals the user name, which allows cross-issuer impersonation [V].
	// +kubebuilder:validation:MinLength=1
	Processor string `json:"processor"`
	// A JSON object the token must contain. The server matches it as a
	// subset; arrays match by inclusion, e.g. {groups: [ch-analysts]} accepts
	// a token whose groups contain ch-analysts [V]. The provider canonicalizes
	// it (sorted keys, no insignificant whitespace) before sending and
	// comparing.
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	Claims *apiextensionsv1.JSON `json:"claims,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(self.policy in ['Listed', 'AllExcept']) == (has(self.roles) && size(self.roles) > 0)",message="roles is required for Listed and AllExcept and forbidden otherwise"
type DefaultRoles struct {
	// +kubebuilder:validation:Enum=All;None;Listed;AllExcept
	// +kubebuilder:default=All
	Policy string `json:"policy"`
	// +optional
	// +listType=set
	Roles []string `json:"roles,omitempty"`
}

// Spec-level rules on UserSpec (it embeds xpv2.ManagedResourceSpec):
// +kubebuilder:validation:XValidation:rule="!has(self.forProvider.authentication.password) || has(self.writeConnectionSecretToRef)",message="password users require writeConnectionSecretToRef"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.forProvider.authentication.password) || !has(self.forProvider.authentication.password) || !has(oldSelf.writeConnectionSecretToRef) || (has(self.writeConnectionSecretToRef) && self.writeConnectionSecretToRef.name == oldSelf.writeConnectionSecretToRef.name)",message="writeConnectionSecretToRef.name is immutable for password users; rotate with generate.rotationToken"

type UserObservation struct {
	// UUID from system.users.id. Informational; ownership is recorded in
	// annotations (section 4.2).
	ID              string   `json:"id,omitempty"`
	Storage         string   `json:"storage,omitempty"`   // local_directory, replicated, users_xml
	AuthTypes       []string `json:"authTypes,omitempty"` // e.g. ["jwt"], ["sha256_password"]
	JWTProcessor    string   `json:"jwtProcessor,omitempty"`
	HostIP          []string `json:"hostIP,omitempty"`
	HostNames       []string `json:"hostNames,omitempty"`
	SettingsProfile string   `json:"settingsProfile,omitempty"`
	DefaultDatabase string   `json:"defaultDatabase,omitempty"`
	// Rendered: "ALL", "NONE", "r1, r2" or "ALL EXCEPT r1".
	DefaultRoles string `json:"defaultRoles,omitempty"`
}

// Printer columns: READY, SYNCED, EXTERNAL-NAME, AUTH (.status.atProvider.authTypes),
// STORAGE (.status.atProvider.storage, priority=1), AGE.
```

**Design decisions for User.**

- **Passwords.**
  - Passwords are stored as `IDENTIFIED WITH sha256_hash BY '<hash>' SALT '<salt>'`, computed exactly like this:
    - `salt := hex.EncodeToString(16 bytes from crypto/rand)`, a 32-character ASCII string, fresh every time the password is applied;
    - `hash := hex.EncodeToString(sha256.Sum256([]byte(password + salt)))`. The salt is appended **as its hex text**, not as the decoded bytes. The server concatenates the SALT literal exactly as sent. Hashing the decoded bytes creates a user who can never log in (516) [V 25.3, 26.6, 26.8].
    - Test vector: password `Pa55word`, salt `00112233445566778899aabbccddeeff`, hash `6257310c34f98d6e96c672db8cee44e0077f4e03eb3581a7260ccd6861848188`. A user created with these values logs in with `Pa55word` [V 25.3, 26.6, 26.8].
  - Every build requires a 64-hex-digit hash. A shorter value fails with 36 [V 25.3, 26.6, 26.8].
  - The server's `query_log` masks the clause as `IDENTIFIED WITH sha256_password` [V].
  - `sha256_hash` bypasses the server's `password_complexity_rules`. `generate` produces strong passwords. For `passwordSecretRef`, the Secret's owner is responsible for strength. This is documented.
- **Password users need `writeConnectionSecretToRef`.** The connection Secret is the record of the password that was last applied:
  - the server never shows the hash (`SHOW CREATE USER` prints `IDENTIFIED WITH sha256_password` only [V]);
  - status can be lost.

  The Secret is read with the uncached API reader, and it is trusted only when its controller reference is this MR. Its name is immutable for password users. See [5.4](#54-user).
- **Rotation is explicit.** For `passwordSecretRef`, change the Secret's value. For `generate`, change `generate.rotationToken`. A missing connection Secret still triggers a new password, but only as recovery, with a Warning event (D-12).
- **No `noPassword` method in v1.**
  - `CREATE USER` without an `IDENTIFIED` clause creates a `no_password` user [V].
  - A passwordless SQL user lets anyone who can reach an allowed host log in. Combined with the takeover risk ([9.2](#92-blast-radius)) it would also be the easiest escalation path.
  - Users that really need it (for example local monitoring) can be defined in `users.d`.
  - The provider always emits an explicit `IDENTIFIED` clause. If it observes `no_password` on a managed user, it treats that as drift and re-applies the desired method.
- **JWT users always pin a processor.** On Antalya 26.6.4, with two processors configured, a jwt user without `PROCESSOR` accepted a token from the second issuer whose `preferred_username` claim equalled the user's name. After `ALTER USER ... IDENTIFIED WITH jwt PROCESSOR 'first'` the same token failed with 516 [V]. Making the field optional later is additive. Making it required later would break users (D-13).
- **Default roles.**
  - `DEFAULT ROLE` is a property of the user and replaces the whole list. A per-membership flag on `RoleGrant` would need a read-modify-write of that list from several reconcilers. So `RoleGrant` has no `default` field in v1 (**OQ-4**).
  - New users start with `DEFAULT ROLE ALL`, so every role granted later is active. That covers the common case [V].
  - `Listed` requires the roles to be granted first: otherwise the server returns 512 `SET_NON_GRANTED_ROLE` [V]. The provider creates such users with `DEFAULT ROLE NONE` and applies only the listed roles that are currently granted ([5.4](#54-user)).
  - `AllExcept` does not require the roles to be granted. It needs only that they exist (511 otherwise) [V 25.3, 26.6, 26.8], so it is set at creation.
- **Hosts.**
  - Hosts are always enforced. Empty means `HOST ANY`.
  - They are deliberately not late-initialized. Telling "unset" apart from `ANY` would need tri-state lists, which do not survive JSON `omitempty`.
  - `HOST REGEXP`, `HOST LIKE` and `HOST NONE` cannot be requested. Observing any of them on a managed user is drift ([5.4](#54-user)).
- **Expiry.** `VALID UNTIL` cannot be requested. An expiry set out of band locks the user out (516) while every other column looks unchanged [V 25.3, 26.6, 26.8]. The provider therefore treats it as drift and clears it.

**Examples.**

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: User
metadata: {name: reporting-api, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: shared-clickhouse}
  forProvider:
    name: system:serviceaccount:team-a:reporting-api
    authentication:
      jwt:
        processor: kubernetes
    hostIP: ["10.0.0.0/8"]
    defaultDatabase: team-a_analytics
  writeConnectionSecretToRef: {name: reporting-api-clickhouse}   # username, endpoint, port, protocol, tls, jwtProcessor
---
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: User
metadata: {name: analyst, namespace: data}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: analytics-clickhouse}
  forProvider:
    name: alice@example.com
    authentication:
      jwt:
        processor: keycloak
        claims: {groups: [ch-analysts]}   # login only with this group in the token
  writeConnectionSecretToRef: {name: analyst-clickhouse}
---
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: User
metadata: {name: etl, namespace: team-b}
spec:
  providerConfigRef: {kind: ProviderConfig, name: upstream}
  forProvider:
    name: etl
    authentication:
      password:
        generate: {rotationToken: "2026-10"}
    hostIP: ["10.0.0.0/8"]
    settingsProfile: default
  writeConnectionSecretToRef: {name: etl-clickhouse}             # + password, rotationToken
```

### 3.6 Role

```go
// +kubebuilder:validation:XValidation:rule="has(self.name) == has(oldSelf.name)",message="name cannot be added or removed after creation"
type RoleParameters struct {
	// Name in ClickHouse. Defaults to the effective external name (4.1).
	// Immutable. It must not collide with a role from users.xml/users.d
	// (the server returns 495 ACCESS_STORAGE_READONLY), and no user with the
	// same name may exist (NameCollision, section 5.5).
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[^\x00-\x1F\x7F]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name *string `json:"name,omitempty"`
	// Role settings (SETTINGS ...) are planned for a later version.
}

type RoleObservation struct {
	ID      string `json:"id,omitempty"`      // system.roles.id, informational
	Storage string `json:"storage,omitempty"`
}

// Printer columns: READY, SYNCED, EXTERNAL-NAME, AGE.
```

### 3.7 Grant

A `Grant` owns **one exact target** for **one grantee**. The target is `*.*`, `db.*`, `db.table` or `db.table(columns)`. See [5.6](#56-grant) for the semantics.

```go
// +kubebuilder:validation:XValidation:rule="has(self.database) || has(self.databaseRef) || has(self.databaseSelector)",message="database is required (\"*\" for all databases)"
// +kubebuilder:validation:XValidation:rule="[has(self.user) || has(self.userRef) || has(self.userSelector), has(self.role) || has(self.roleRef) || has(self.roleSelector)].exists_one(x, x)",message="set exactly one grantee: user(Ref|Selector) or role(Ref|Selector)"
// +kubebuilder:validation:XValidation:rule="!has(self.columns) || (has(self.table) && self.table != '*')",message="columns require a specific table"
// +kubebuilder:validation:XValidation:rule="!has(self.table) || self.table == '*' || !has(self.database) || self.database != '*'",message="a specific table requires a specific database"
// Target and grantee are immutable. A reference may fill an empty field once:
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.database) || (has(self.database) && self.database == oldSelf.database)",message="database is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.table) == has(oldSelf.table) && (!has(self.table) || self.table == oldSelf.table)",message="table is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.columns) == has(oldSelf.columns) && (!has(self.columns) || self.columns == oldSelf.columns)",message="columns are immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.user) || (has(self.user) && self.user == oldSelf.user)",message="user is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.role) || (has(self.role) && self.role == oldSelf.role)",message="role is immutable"
// Plus the cross-namespace rules of 3.1 for databaseRef/Selector, userRef/Selector and roleRef/Selector.
type GrantParameters struct {
	// Privileges, case-insensitive. Names and aliases from system.privileges
	// are accepted, e.g. SELECT, INSERT, "ALTER UPDATE", dictGet, ALL.
	// Source privileges (S3, URL, REMOTE, FILE, ..., READ, WRITE, SOURCES)
	// and privileges of the levels NAMED_COLLECTION, USER_NAME, TABLE_ENGINE,
	// DEFINER and SOURCE are rejected in v1 (section 5.6).
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z][A-Za-z0-9_ ]*$`
	Privileges []string `json:"privileges"`

	// Database name, or "*" for a global grant (ON *.*, which needs
	// policy.allowGlobalGrants on a ClusterProviderConfig). Wildcard targets
	// such as "team_*" are not supported in v1 (OQ-3). A '*' inside a name is
	// quoted and therefore literal.
	// +optional
	// +crossplane:generate:reference:type=Database
	Database *string `json:"database,omitempty"`
	// +optional
	DatabaseRef *xpv2.NamespacedReference `json:"databaseRef,omitempty"`
	// +optional
	DatabaseSelector *xpv2.NamespacedSelector `json:"databaseSelector,omitempty"`

	// Table name, or "*" (the default) for all tables.
	// +optional
	Table *string `json:"table,omitempty"`

	// Columns. Only valid with a specific table and column-level privileges.
	// +optional
	// +listType=set
	Columns []string `json:"columns,omitempty"`

	// Grantee: a user...
	// +optional
	// +crossplane:generate:reference:type=User
	User *string `json:"user,omitempty"`
	// +optional
	UserRef *xpv2.NamespacedReference `json:"userRef,omitempty"`
	// +optional
	UserSelector *xpv2.NamespacedSelector `json:"userSelector,omitempty"`

	// ...or a role. No user with the same name may exist: the server resolves
	// grantee names user-first (AmbiguousGrantee, section 5.6).
	// +optional
	// +crossplane:generate:reference:type=Role
	Role *string `json:"role,omitempty"`
	// +optional
	RoleRef *xpv2.NamespacedReference `json:"roleRef,omitempty"`
	// +optional
	RoleSelector *xpv2.NamespacedSelector `json:"roleSelector,omitempty"`

	// Needs policy.allowGrantOption on a ClusterProviderConfig.
	// +optional
	// +kubebuilder:default=false
	WithGrantOption *bool `json:"withGrantOption,omitempty"`
}

type GrantObservation struct {
	// "user:<name>" or "role:<name>".
	Grantee string `json:"grantee,omitempty"`
	// Rendered target, e.g. "*.*", "app.*", "app.events(a, b)".
	Target string `json:"target,omitempty"`
	// Privileges stored at exactly this target, in the server's
	// group-compressed form, sorted. Informational only: the compressed form
	// differs between server versions (section 5.6) and is never compared.
	Privileges []string `json:"privileges,omitempty"`
}

// Printer columns: READY, SYNCED, GRANTEE (.status.atProvider.grantee),
// TARGET (.status.atProvider.target), AGE.
```

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: Grant
metadata: {name: reader-events, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: shared-clickhouse}
  forProvider:
    privileges: ["SELECT", "SHOW TABLES"]
    databaseRef: {name: analytics}       # Database MR team-a/analytics, ClickHouse name team-a_analytics
    table: events
    roleRef: {name: reader}              # Role MR team-a/reader, ClickHouse name team-a_reader
```

### 3.8 RoleGrant

In both `Grant` and `RoleGrant`, `user`/`role` always name the **grantee**, as in provider-sql. In `RoleGrant`, `memberOf` names the role being granted (D-14).

```go
// +kubebuilder:validation:XValidation:rule="has(self.memberOf) || has(self.memberOfRef) || has(self.memberOfSelector)",message="memberOf is required"
// +kubebuilder:validation:XValidation:rule="[has(self.user) || has(self.userRef) || has(self.userSelector), has(self.role) || has(self.roleRef) || has(self.roleSelector)].exists_one(x, x)",message="set exactly one grantee: user(Ref|Selector) or role(Ref|Selector)"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.memberOf) || (has(self.memberOf) && self.memberOf == oldSelf.memberOf)",message="memberOf is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.user) || (has(self.user) && self.user == oldSelf.user)",message="user is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.role) || (has(self.role) && self.role == oldSelf.role)",message="role is immutable"
// Plus the cross-namespace rules of 3.1 for every *Ref and *Selector.
type RoleGrantParameters struct {
	// The role being granted; the grantee becomes a member of it. It may be a
	// Role MR or a role defined in users.xml/users.d (plain name). It must
	// match policy.grantableRoles on a ClusterProviderConfig.
	// +optional
	// +crossplane:generate:reference:type=Role
	MemberOf *string `json:"memberOf,omitempty"`
	// +optional
	MemberOfRef *xpv2.NamespacedReference `json:"memberOfRef,omitempty"`
	// +optional
	MemberOfSelector *xpv2.NamespacedSelector `json:"memberOfSelector,omitempty"`

	// Grantee: an SQL user, not one from users.xml (see 5.7)...
	// +optional
	// +crossplane:generate:reference:type=User
	User *string `json:"user,omitempty"`
	// +optional
	UserRef *xpv2.NamespacedReference `json:"userRef,omitempty"`
	// +optional
	UserSelector *xpv2.NamespacedSelector `json:"userSelector,omitempty"`

	// ...or a role (role inheritance). No user with the same name may exist.
	// +optional
	// +crossplane:generate:reference:type=Role
	Role *string `json:"role,omitempty"`
	// +optional
	RoleRef *xpv2.NamespacedReference `json:"roleRef,omitempty"`
	// +optional
	RoleSelector *xpv2.NamespacedSelector `json:"roleSelector,omitempty"`

	// Needs policy.allowAdminOption on a ClusterProviderConfig.
	// +optional
	// +kubebuilder:default=false
	AdminOption *bool `json:"adminOption,omitempty"`
}

type RoleGrantObservation struct {
	Grantee     string `json:"grantee,omitempty"` // "user:<name>" or "role:<name>"
	AdminOption bool   `json:"adminOption,omitempty"`
	// granted_role_is_default, reported for information. Use User.defaultRoles to change it.
	IsDefault bool `json:"isDefault,omitempty"`
}

// Printer columns: READY, SYNCED, MEMBER-OF (.spec.forProvider.memberOf),
// GRANTEE (.status.atProvider.grantee), AGE.
```

```yaml
apiVersion: clickhouse.m.infrabay.io/v1alpha1
kind: RoleGrant
metadata: {name: reporting-api-reader, namespace: team-a}
spec:
  providerConfigRef: {kind: ClusterProviderConfig, name: shared-clickhouse}
  forProvider:
    memberOf: analytics_reader          # a role from users.d
    userRef: {name: reporting-api}
```

---

## 4. External names, ownership and adoption

### 4.1 External name

**For `Database`, `User` and `Role`, the external name is the ClickHouse name.** The **effective external name** is decided as follows:

1. `spec.forProvider.name`, if set;
2. otherwise the `crossplane.io/external-name` annotation, if set (the Crossplane import convention);
3. otherwise `metadata.name`.

If `forProvider.name` and the annotation are both set and differ, the reconcile fails with `ExternalNameConflict`. The annotation is never silently rewritten. **All policy checks and all SQL use the effective external name**, never `metadata.name` or `forProvider.name` alone. Otherwise a tenant could bypass name patterns by setting only the annotation.

- A custom initializer replaces crossplane-runtime's `NewNameAsExternalName`. If the annotation is empty, it sets it to the effective name. The initializer does nothing when `meta.WasDeleted(mg)` is true. Otherwise an `ExternalNameConflict` could block deletion. Observe does the same: while deleting, an unresolvable name orphans the MR (Warning event `Orphaned`, `ResourceExists=false`, no SQL).
- `spec.forProvider.name` exists because names such as `svc@my-project.iam.gserviceaccount.com` cannot be `metadata.name`.
  - CEL cannot validate annotations; only `metadata.name` is visible to CRD rules.
  - A spec field can be validated and made immutable.
- The annotation is still written. Tools, printer columns and reference resolvers read it.

**`Grant` and `RoleGrant` do not use an external name.** Their identity is the immutable spec tuple: grantee, target and role. Their controllers are built with no name initializer.

**Every kind uses `managed.WithDeterministicExternalName(true)`** (D-8). Without it, a crash between crossplane-runtime's `external-create-pending` and `external-create-succeeded` annotations makes the reconciler stop with `Requeue: false` until a human removes the annotation. That check runs before Observe, even while the MR is being deleted [C reconciler.go]. The runtime guard exists for external names that the server assigns. Here every name comes from the spec. Creates are made recoverable by the ownership rules below:

- GRANT and REVOKE are idempotent.
- A Database's UUID is chosen before the CREATE.
- A User or Role created by a call whose result was lost is recognised by its owner claim and the create annotations.

### 4.2 Ownership

The provider identity can alter or drop SQL users and roles it did not create ([9.2](#92-blast-radius)). So the controller must know whether the object behind a name is *its* object.

- ClickHouse has no server-side marker it could use for this: `CREATE USER ... COMMENT` is a syntax error [V 25.3, 26.6, 26.8].
- Ownership is therefore anchored on the entity UUID: `system.users.id`, `system.roles.id` or `system.databases.uuid`.
- **The claim lives in annotations, not in `status`** (D-6). crossplane-runtime resets in-memory status on full-object updates, so a UUID written to status during Create is always lost [C]. `status.atProvider.id` stays purely informational, and observe-only MRs fill it without claiming anything.

**Annotations.**

| Annotation | Value | Written by |
|---|---|---|
| `clickhouse.m.infrabay.io/owner-uid` | `metadata.uid` of the MR that holds the claim | the initializer; adoption; recovery |
| `clickhouse.m.infrabay.io/owned-id` | UUID of the ClickHouse object | Database: the initializer, before the CREATE. User and Role: Create, after reading the UUID back. Also adoption and recovery. |
| `clickhouse.m.infrabay.io/create-outcome` | `rejected` or `unknown` | User and Role Create, on failure |
| `clickhouse.m.infrabay.io/adopt` | UUID of the object to adopt | a human; removed by the provider after adoption |

- **A claim is valid only when `owner-uid` equals the MR's current `metadata.uid`.** The API server assigns a new UID on create. So a manifest copied with `kubectl get -o yaml`, or an MR restored from a backup, carries an **invalid** claim that is ignored. The copy cannot take over the original's object by accident. A restore needs re-adoption (**OQ-21**, runbook in `docs/operations.md`).
- **The initializer** (Database, User, Role) runs only when the management policies include `Create` and the MR is not being deleted. If the claim is absent or invalid, it writes `owner-uid` = `metadata.uid`. For a Database it also writes a fresh random `owned-id` (UUID v4). For a User or Role it removes any `owned-id`. It persists the change with a `client.Update`, as `NameAsExternalName` does. Observe-only MRs never get a claim.
- **Create** (User, Role) reads the new UUID back and sets `owned-id` on the in-memory object. crossplane-runtime's `RetryingCriticalAnnotationUpdater` persists every in-memory annotation in the same update that records `external-create-succeeded` [C api.go, reconciler.go]. On failure, Create sets `create-outcome`, which is persisted together with `external-create-failed`:
  - `rejected` for any `*clickhouse.Exception`, including 493 and 82;
  - `unknown` for transport errors, a context deadline, EOF and 159 `TIMEOUT_EXCEEDED`, because the statement may still have run.

  A successful Create removes `create-outcome`.
- **Create** (Database) sends ``CREATE DATABASE `n` UUID '<owned-id>' ...`` ([5.3](#53-database)). Ownership is then UUID equality, which survives crashes, lost responses and ON CLUSTER timeouts. A database dropped out of band can be re-created with the same UUID immediately [V all].
- **Adoption and recovery happen in Observe.** The external client writes the claim and removes `adopt` with a JSON merge patch on `metadata.annotations` before Observe returns, then fills `status.atProvider`. Annotation changes made in Observe are otherwise not persisted unless late-initialization runs.

**Adoption.** Set `clickhouse.m.infrabay.io/adopt: "<uuid>"` to the UUID shown in the `NotOwned` error, or in `system.users.id`, `system.roles.id` or `system.databases.uuid`. The provider adopts only when the value equals the observed UUID, so an object replaced between the human's check and the reconcile is not adopted (D-7). The value `"true"` is not accepted. Adoption must be allowed by the config:

- on a `ClusterProviderConfig`: `policy.allowAdoption: true`;
- on a namespaced `ProviderConfig`: always allowed. The tenant owns that identity, and the server's grants bound it.

After adoption the annotation is spent. A later out-of-band replacement yields `Replaced`, not a silent re-adoption.

**Decision table in Observe**, when an object with the effective external name exists. The first matching row wins.

| # | Condition | Result |
|---|---|---|
| 1 | storage is `users_xml` | **Read-only.** Never written. If the MR is being deleted, report `ResourceExists=false` so the finalizer is released. Otherwise error `ReadOnlyStorage` while the management policies allow writes. Observe-only MRs just get status. |
| 2 | valid claim, `owned-id` equals the observed UUID | **Owned.** A leftover `adopt` annotation is removed. |
| 3 | `adopt` equals the observed UUID and adoption is allowed | **Adopted.** Write the claim, remove `adopt`, emit event `Adopted`. |
| 4 | `adopt` is set, but differs from the observed UUID or adoption is not allowed | Error `AdoptionRejected` (names the observed UUID, or says that `policy.allowAdoption` is false). |
| 5 | User or Role, valid claim, no `owned-id`, and either `external-create-pending` is newer than both `external-create-succeeded` and `external-create-failed`, or `create-outcome` is `unknown` and `external-create-failed` is the newest | **Recovered.** Treated as owned: write `owned-id` and emit event `CreateRecovered`. This is the crash or lost-response case. An object created by someone else inside that same window would also be claimed; the window is a single reconcile, and the risk is accepted. |
| 6 | valid claim, `owned-id` set and different from the observed UUID, and `external-create-succeeded` is the newest create annotation | **Replaced out of band.** Error `Replaced: <kind> "<name>" was replaced (id X, now Y); set clickhouse.m.infrabay.io/adopt="Y" to take over the new object`. |
| 7 | anything else | **Not owned.** Error `NotOwned: <kind> "<name>" already exists (id Y) and was not created by this resource; to take it over set annotation clickhouse.m.infrabay.io/adopt="Y" (requires policy.allowAdoption on a ClusterProviderConfig)`. |

The table has these consequences:

- **While the MR is being deleted**, every decision except Owned, Adopted and Recovered makes Observe return `ResourceExists=false`, with a Warning event `Orphaned`. That includes observe-only and import MRs: Delete is reached only for the MR's own object even if crossplane-runtime did not honour the management policies (a second guard to D-26). The MR goes away and the server object is untouched.
- **Management policies with only `Observe` and/or `LateInitialize`** (observe-only or import mode): rows 3 to 7 produce no error, status is filled, and no claim is written. Name policy still applies ([5.8](#58-policy-evaluation)). Switching such an MR to write policies later makes the initializer write a claim that does not match. The MR then needs adoption (row 3).
- **The object does not exist:** `ResourceExists=false` and Create runs. An object deleted out of band is re-created, which is normal Crossplane behaviour. A Database is re-created with its recorded UUID.
- **Annotations are writable by anyone who can edit the MR.** The claims prevent *accidents*: copy-pasted or restored manifests, name collisions with admin-created users, and lost responses. They do not stop a malicious tenant who forges `owner-uid` and `owned-id`. Protection against tenants comes from:
  - the `ClusterProviderConfig` policy (`allowedNamespaces`, name patterns with `{namespace}`, `grantableRoles`, `zooKeeperPathPatterns`). Name checks also apply while the MR is being deleted ([5.8](#58-policy-evaluation)), so a forged claim on a name outside the tenant's patterns never leads to a DROP, and row 5 can only recover names inside them;
  - server-side prefix grants for the identity ([9.4](#94-minimal-grants-for-the-provider-identity));
  - Kubernetes RBAC and admission control.

  See [9.3](#93-provider-identity-sources-and-tenancy).
- **`CREATE OR REPLACE USER` by an administrator keeps the UUID and the grants** on 25.3, 26.6.4 and 26.8 [V]. Such a replacement is undetectable. `DROP` followed by `CREATE` changes the UUID and is detected (row 6). The provider itself never uses `OR REPLACE`.
- **Engines without a UUID** (`Memory`, `uuid = 00000000-...`) cannot be told apart from a re-created object. v1 only creates `Atomic` and `Replicated`, which have UUIDs. A Database MR whose observed UUID is zero is reported as `NotOwned` unless adopted, and its adoption annotation uses the value `00000000-0000-0000-0000-000000000000`.
- **The provider never modifies its own identity.** Any write to a user named `currentUser()` fails with `SelfModification`. That covers User, and Grant or RoleGrant with that user as grantee. This holds even if the identity is an SQL user and an MR claims or adopts it.

`Grant` and `RoleGrant` have no UUID, and GRANT is idempotent.

- They are bounded instead by policy on the grantee, the target and the role, and by the rule that the provider never creates partial revokes ([5.6](#56-grant)).
- **Deleting a `Grant` or a `RoleGrant` revokes the privileges or the membership, even if someone else had granted them before.** To avoid that, for example when importing existing grants, use management policies without `Delete`.

---

## 5. Reconcile logic

### 5.1 SQL safety

Two rules apply everywhere: **DDL is built as text from validated input and quoting helpers. Reads use server-side parameters.**

```go
// internal/clients/clickhouse/quote.go
var identEscaper = strings.NewReplacer(`\`, `\\`, "`", "\\`")
var stringEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`)   // same set as clickhouse-go bind.go
var paramEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`, "\x00", `\0`)

func QuoteIdent(s string) string  { return "`" + identEscaper.Replace(s) + "`" }
func QuoteString(s string) string { return "'" + stringEscaper.Replace(s) + "'" }
// ParamValue encodes a value for clickhouse.Parameters. The server parses
// parameter values in the escaped (TSV) text format.
func ParamValue(s string) string { return paramEscaper.Replace(s) }
```

**Rules.**

- **Identifiers** (database, table, column, user and role names) always go through `QuoteIdent`. Never split names on `@` or `.`; provider-sql's `SplitUserHost` and MySQL quoting must not be copied. A quoted identifier is always literal: a `*` inside a quoted name is not a wildcard [V all].
- **String literals** (processor, claims, hash, salt, comment, host values, profile name, Replicated engine arguments, database UUID, cluster name in `clusterAllReplicas`) always go through `QuoteString`.
- **Privilege keywords** are not quoted. They are emitted only after they have been resolved to a canonical name from `system.privileges` ([5.6](#56-grant)). The CRD pattern `^[A-Za-z][A-Za-z0-9_ ]*$` blocks anything else before it gets that far. Digits and underscores occur in real names, such as `SYSTEM DROP S3 CLIENT CACHE` and `SYSTEM DROP ICEBERG_METADATA_CACHE` [V].
- **DDL is executed with `Exec(ctx, sql)` and no args.** clickhouse-go's client-side binder only runs when args are passed, so `?`, `@x` and `$1` inside names are inert.
- **SELECTs use `{name:String}` placeholders** with `clickhouse.Context(ctx, clickhouse.WithParameters(clickhouse.Parameters{"name": ParamValue(n)}))` [V].
  - Without `ParamValue`, a name containing a TAB fails with 457 `BAD_QUERY_PARAMETER`.
  - Without it, a backslash sequence inside a name (for example the two characters `\t`) would be decoded by the server, so the lookup would miss [C]. With `ParamValue`, both cases round-trip [V].
- **Every statement carries `log_comment = '<kind>/<namespace>/<name>'`**, set per query through `clickhouse.WithSettings`. Every DDL in `system.query_log` is then attributable to an MR.
- **The client identifies itself** as `provider-clickhouse/<version>` (ClientInfo).
- **Logging.** The provider logs DDL at debug level with the `IDENTIFIED WITH ...` clause redacted. Tokens, passwords and hashes are never logged.
- **Error redaction.** ClickHouse `SYNTAX_ERROR` messages quote the statement from the failure position onward. A malformed `IDENTIFIED` clause would put the hash and salt into the error [V 26.8]. Codes 36 and 180 do not echo the query [V]. For every statement that carries `IDENTIFIED`, server errors are therefore reduced to code, name and a message in which every single-quoted literal is replaced with `'***'`. Errors are never wrapped with SQL text.
- **Unit tests** cover every helper with adversarial names: backtick, backslash, quote, NUL, TAB, newline, `@`, `.`, `*`, Unicode, and `\t` written as two characters. Further tests assert that no hash, salt, token or password appears in any returned error.

### 5.2 Errors and "not found"

A resource is reported as not existing (`ResourceExists=false`) **only** when an identifying query that cannot be silently filtered succeeded and said "absent".

- Every other failure is returned as an error and is never turned into "not found". That includes network errors, timeouts, `context.Canceled`, EOF, TLS errors, 516, 497 and decode errors.
- Zero rows from a privilege-filtered table (`system.databases`, `system.tables`) are not "not found" ([5.3](#53-database)).
- ClickHouse errors are classified with `errors.As(err, &clickhouse.Exception{})` on `Code` [V].

| Code | Name | Where | Handling |
|---|---|---|---|
| 516 | AUTHENTICATION_FAILED | any | Token sources: invalidate the token, rebuild the pool, retry **once** ([6.4](#64-516-retry)). Then error `AuthenticationFailed`. Also caused by clock skew beyond the 60 s server leeway. |
| 497 | ACCESS_DENIED | any | Error `PermissionDenied`, with the server message (it names the missing grant). Never "not found". |
| 495 | ACCESS_STORAGE_READONLY | writes | Error `ReadOnlyStorage` (the object is from users.xml/users.d). Returned even with `IF EXISTS` / `IF NOT EXISTS` [V 25.3, 26.6, 26.8]. |
| 493 | ACCESS_ENTITY_ALREADY_EXISTS | Create User/Role | Error `AlreadyExists` (a concurrent creator). Not adopted. |
| 82 | DATABASE_ALREADY_EXISTS | Create Database | Error `AlreadyExists`. |
| 57 | TABLE_ALREADY_EXISTS | Create Database with UUID | The UUID is in use by another database. Not expected with random UUIDs. Error `InvalidSpec`. |
| 192 | UNKNOWN_USER | ALTER USER | Error `DependencyMissing`. In Delete paths, treat as already gone. |
| 511 | UNKNOWN_ROLE | GRANT, REVOKE, DEFAULT ROLE | Error `DependencyMissing`. Also returned for an unknown grantee in both `GRANT r TO x` and privilege `GRANT`/`REVOKE` [V 25.3, 26.6, 26.8]. In Delete paths, treat as already gone. |
| 512 | SET_NON_GRANTED_ROLE | DEFAULT ROLE | Not expected: `Listed` applies only granted roles ([5.4](#54-user)). If it occurs (race with a revoke): error `DependencyMissing`. |
| 180 | THERE_IS_NO_PROFILE | SETTINGS PROFILE | Error `DependencyMissing`. |
| 81 | UNKNOWN_DATABASE | Database Update | The database disappeared between Observe and Update. Return without error; the next Observe decides. Delete uses `IF EXISTS`. |
| 509 | INVALID_GRANT | GRANT | A privilege that does not exist at the target's level, e.g. "CREATE DATABASE cannot be granted on the table level" [V 25.3, 26.6, 26.8]. Means the catalogue model and the server disagree. Error `InvalidSpec`. |
| 458 | CANNOT_UNLINK | CREATE DATABASE | "File name too long" for an over-long escaped name [V]. Error `InvalidSpec`. The Go length check should catch it first. |
| 36 | BAD_ARGUMENTS | Create, probes | From the jwt probe: JWT unsupported ([section 7](#7-server-capability-detection)). Otherwise a configuration error, e.g. a jwt user on upstream ("CREATE USER is not supported for JWT"): `Unsupported` or `InvalidSpec`. |
| 159 | TIMEOUT_EXCEEDED | ON CLUSTER DDL | Transient. The DDL may finish in the background; the next Observe decides. In Create this sets `create-outcome: unknown` ([4.2](#42-ownership)). |
| 62 | SYNTAX_ERROR | probes, any | From the jwt probe on 25.3: JWT unsupported. From `ALTER DATABASE ... MODIFY COMMENT` on 25.3: `Unsupported` (the capability check should prevent it). Otherwise a provider bug: `Internal`. Never echoed unredacted ([5.1](#51-sql-safety)). |
| 457 | BAD_QUERY_PARAMETER | any | Provider bug. Error `Internal`. |

**How errors are reported.**

- Errors are wrapped as `fmt.Errorf("%s: %w", Reason, err)`, where `Reason` is one of a fixed set of tokens: `NotOwned`, `Replaced`, `AdoptionRejected`, `ReadOnlyStorage`, `SelfModification`, `NameCollision`, `AmbiguousGrantee`, `PolicyViolation`, `InvalidConfig`, `Unsupported`, `DependencyMissing`, `PermissionDenied`, `AuthenticationFailed`, `AccessStorageNotReplicated`, `ExternalNameConflict`, `DatabaseNotEmpty`, `SecretConflict`, `AlreadyExists`, `InvalidSpec`, `Internal`.
- crossplane-runtime puts them on the MR as `Synced=False` (`ReconcileError`) and emits a Warning event.
- **Only ClickHouse's own error text reaches the MR.** For a `*clickhouse.Exception`, the condition carries its code, name and message (redacted as in [5.1](#51-sql-safety)). For every other error (dial, TLS, protocol, token endpoint), the condition carries the reason, a short category ("dial failed", "TLS handshake failed", "token fetch failed") and at most 256 characters of the error, with any HTTP response body removed. Full details go to debug logs only. This keeps a tenant-chosen endpoint or token URL from reflecting arbitrary response bodies into conditions and events ([9.3](#93-provider-identity-sources-and-tenancy)).
- The provider also emits Normal events: `Adopted`, `CreateRecovered` and `PasswordRotated`. It emits Warning events: `PasswordRegenerated`, `DefaultRoleNotGranted`, `OrphanedByPolicy` (a deleting MR whose namespace or name the policy does not allow) and `Orphaned` (a deleting MR whose object is not its own, whose name cannot be resolved, or that has another UUID on some host of the cluster). Both orphan events mean that the MR went away and the server object was left in place.
- Custom condition types are **OQ-7**. The `Ready` condition is set per [5.10](#510-conditions-and-readiness).

**Silently filtered system tables.** Some system tables are filtered by privileges instead of failing:

- `system.databases` hides databases the identity has no privilege on. `EXISTS DATABASE` instead fails with 497 [V 25.3, 26.6, 26.8].
- `system.tables` hides tables the identity cannot `SHOW` [V].

In contrast, `system.users`, `system.roles`, `system.role_grants`, `system.grants` and `system.settings_profile_elements` fail with 497 without the required grant [V]. **`system.grants` and `system.role_grants` are not filtered by grantee:** with only `SELECT` on them, every user's and role's rows are visible [V 25.3, 26.6, 26.8]. `SHOW USERS` and `SHOW ROLES` are needed only for `system.users`, `system.roles` and `SHOW CREATE USER`.

### 5.3 Database

**Observe**

0. Resolve the effective name and run the name check ([5.8](#58-policy-evaluation)), also while the MR is being deleted. While deleting, an unresolvable name or a name outside the policy orphans the MR (Warning event `Orphaned` or `OrphanedByPolicy`, `ResourceExists=false`) before any SQL runs.
1. ``EXISTS DATABASE <QuoteIdent(name)>``.
   - 497: error `PermissionDenied`. The identity needs at least `SHOW DATABASES` on the database, which any privilege on it implies.
   - `0`: `ResourceExists=false`.
   - `1`: continue.
2. Read the details:

   ```sql
   SELECT toString(uuid), engine, engine_full, comment
   FROM system.databases WHERE name = {name:String}
   ```

   Zero rows here, after `EXISTS` said 1, is error `PermissionDenied` (filtered), never "not found".
3. Ownership is checked as in [4.2](#42-ownership), using `uuid`. While deleting, anything but Owned or Adopted orphans the MR (Warning event `Orphaned`). With `cluster`, while deleting, the per-host check below runs next, before any claim is written: another UUID on any host orphans the MR, because `DROP ... ON CLUSTER` would drop that database there.
4. **Engine.**
   - If `spec.engine` is nil, it is late-initialized from `engine`. For `Replicated`, the arguments are taken verbatim from `engine_full`.
   - If it is set and `engine` differs, the result is error `InvalidSpec: engine is immutable (observed X, desired Y); re-create the database`. Update is not attempted, because no SQL can fix it.
   - For `Replicated`, the three arguments are compared verbatim with `engine_full`. The server keeps macros unexpanded there [V], so `'/clickhouse/databases/{uuid}'` compares equal to itself.
5. **Comment.**
   - If it is nil, it is late-initialized.
   - If it is set and differs, the result is `ResourceUpToDate=false`. If the server lacks `ALTER MODIFY DATABASE COMMENT` (upstream 25.3, [section 7](#7-server-capability-detection)), the result is instead error `Unsupported: changing a database comment needs ClickHouse 25.8 or later`.

**Create**

```sql
CREATE DATABASE `name` UUID '<owned-id>' [ON CLUSTER `c`]
  [ENGINE = Atomic | ENGINE = Replicated('<zk path>', '<shard>', '<replica>')]
  [COMMENT '<comment>']
```

- The clause order (name, UUID, ON CLUSTER, ENGINE, COMMENT) parses on every version [V all: `formatQuery`]. Running it with both UUID and ON CLUSTER is [T].
- **The UUID is always sent** (D-8):
  - Ownership is known before the CREATE, so a lost response is harmless ([4.2](#42-ownership)).
  - The default Replicated path `'/clickhouse/databases/{uuid}'` works without `cluster`. Otherwise the server rejects `{uuid}` with 36 "Macro 'uuid' in engine arguments is only supported when the UUID is explicitly specified or used within an ON CLUSTER query" [V A26.6 with Keeper].
  - Every ON CLUSTER replica gets the same UUID.
- With `engine` nil, the ENGINE clause is omitted. The server default (Atomic) accepts a UUID [V]. A server whose default engine does not accept a UUID fails the CREATE; the fix is to set `engine` [T].
- Before the statement, the provider checks the escaped-name length rule from [3.4](#34-database), that `owned-id` is a non-zero UUID in canonical form, and, for `Replicated`, the Keeper path policy (D-27).
- With `cluster`, CREATE fails with 82 on a host that already has a database of that name and succeeds on the others. The next Observe sees the MR's own database on the connected host; the per-host check before every ON CLUSTER write (below) keeps the other host's database untouched [V 26.8, two replicas].

**Re-check before every write.** Update and Delete address the database by name, using an ownership decision made earlier in Observe. Immediately before the statement they re-read `SELECT toString(uuid) FROM system.databases WHERE name = {name:String}` (zero rows: `EXISTS DATABASE` decides between "gone" and `PermissionDenied`) and compare it with `owned-id`; a mismatch is error `Replaced` and nothing is written. This narrows the window in which the database could be dropped and re-created by someone else to one round trip. A write without a valid claim is refused with `NotOwned`.

**Per-host ownership with `cluster`.** `ON CLUSTER` statements run by name on every host, but Observe reads only the connected one. Before every `ALTER ... ON CLUSTER` and `DROP ... ON CLUSTER`, and in Observe while deleting, the provider reads

```sql
SELECT h, u FROM clusterAllReplicas('<cluster>',
  view(SELECT hostName() AS h, toString(uuid) AS u FROM system.databases WHERE name = {name:String}))
```

and refuses the write (error `NotOwned` naming the hosts) if any host has the name with another UUID. Hosts without the database return no row: `ALTER` fails there with 81 and `DROP ... IF EXISTS` does nothing. An unreachable host fails the query (`ALL_CONNECTION_TRIES_FAILED`), so the check never passes by omission [V 26.8]. The `view()` form needs only `REMOTE` (`READ ON REMOTE`); reading `clusterAllReplicas('<cluster>', system.databases)` directly also needs `SHOW COLUMNS ON system.databases` [V 26.8]. Without `REMOTE` the result is error `PermissionDenied`. Residual risk: with an interserver secret, remote hosts run the query as the provider identity, so a database of that name that the identity cannot see on a remote host returns no row [V 26.8]. Keep the identity's grants identical on every host (or use a replicated user directory).

**Update** (comment only)

```sql
ALTER DATABASE `name` [ON CLUSTER `c`] MODIFY COMMENT '<comment>'    -- [V]; the privilege also exists on 25.8 and A26.3 [V]; not on 25.3
```

The re-check above runs first. 81 means the database is gone. The Update returns, and the next Observe decides.

**Delete**

1. Unless `forceDrop` is true, check that the database is empty:
   - **Single host** (no `cluster`):
     1. ``CHECK GRANT SHOW TABLES ON `name`.*`` must return 1. Otherwise `system.tables` could silently return 0 [V 25.3, 26.6, 26.8]. If it returns 0, the result is error `DatabaseNotEmpty: cannot verify that <name> is empty (grant SHOW TABLES ON <name>.*)`.
     2. `SELECT count() FROM system.tables WHERE database = {name:String}`. If the count is above zero: error `DatabaseNotEmpty`.
   - **With `cluster`**: emptiness cannot be verified (D-18). `system.tables` on remote hosts is filtered by the privileges of whichever user the remote query runs as (the provider identity with an interserver secret, otherwise the cluster's configured user), and no check can prove that it hides nothing. Delete is error `DatabaseNotEmpty: cannot verify that <name> is empty on every host of cluster <c> ...; set forceDrop` unless `forceDrop` is true.

   The MR stays in deletion until the tables are removed, `forceDrop` is set, or `Delete` is removed from the management policies.
2. Re-check ownership (above; with `cluster` on every host).
3. Run ``DROP DATABASE IF EXISTS `name` [ON CLUSTER `c`]``. `SYNC` is not used: on Atomic it can outlast the reconcile timeout on large databases.

**Server-side backstop, single host only.** `DROP DATABASE` on a non-empty database needs `DROP TABLE` on the tables it contains: the server returns 497 "necessary to have the grant DROP TABLE ON d1.t" [V]. If the provider identity is not granted `DROP TABLE` ([9.4](#94-minimal-grants-for-the-provider-identity)), a single server refuses to drop non-empty databases whatever the provider decides. **This backstop does not work for `ON CLUSTER`.** On a two-shard 26.8 cluster, an identity without `DROP TABLE` dropped a database that held a three-row table on the other shard. That still happened with `distributed_ddl_use_initial_user_and_roles = 1` on both hosts [V 26.8]. In cluster mode Delete therefore always needs `forceDrop`, and the examples leave `Delete` out of cluster Databases ([9.6](#96-recommended-management-policies)).

**Settings with `cluster`.**

- The provider sets `distributed_ddl_task_timeout` to 45 s for DDL, which is below the reconcile timeout.
- The controller uses `managed.WithTimeout(2*time.Minute)`.
- Observe reads only the replica it is connected to (**OQ-10**): `Ready` reflects that replica. Every ON CLUSTER write, and Observe while deleting, checks the UUID on every replica first.
- Grants: `CLUSTER ON *.*` (ON CLUSTER), `SELECT ON system.clusters` (the cluster check in Observe) and `REMOTE ON *.*` (the per-host check, for Update and Delete) [V 26.8].

### 5.4 User

**Observe**

1. Read the user's row:

   ```sql
   SELECT toString(id), storage, auth_type, host_ip, host_names, host_names_regexp, host_names_like,
          default_roles_all, default_roles_list, default_roles_except, default_database
          [, valid_until]                                -- when the column exists (section 7)
   FROM system.users WHERE name = {name:String}
   ```

   No row means `ResourceExists=false`. `system.users` is not filtered: without `SHOW USERS` it fails with 497. Then apply the ownership and storage rules ([4.2](#42-ownership)).
2. **Name collision.** `SELECT count() FROM system.roles WHERE name = {name:String}`. A role with the same name gives error `NameCollision`: the server resolves grantee names user-first, so this user would capture every grant meant for that role [V 25.3, 26.6, 26.8]. The check is skipped while the MR is being deleted.
3. Run ``SHOW CREATE USER `name` `` and parse the `IDENTIFIED WITH` clause (see the parser below) when any of these holds:
   - the desired method is jwt;
   - `auth_type` contains `jwt`;
   - `system.users` has no `valid_until` column (25.3, 25.8), so `VALID UNTIL` is visible only here.

   `system.users.auth_params` is `['{}']` for jwt users [V]. The processor and claims are visible only in `SHOW CREATE USER`.
4. If `settingsProfile` is managed or late-initialized, run:

   ```sql
   SELECT inherit_profile, setting_name
   FROM system.settings_profile_elements WHERE user_name = {name:String}
   ```

5. If `defaultRoles.policy` is `Listed`, read the granted roles: `SELECT granted_role_name FROM system.role_grants WHERE user_name = {name:String}`.
6. If the desired method is password, read the connection Secret (`spec.writeConnectionSecretToRef.name` in the MR namespace) with the **uncached API reader** (`mgr.GetAPIReader()`).
   - The cached client is not used: right after Create publishes the Secret, the reconciler requeues immediately. A stale cache would then report "Secret missing" and trigger a needless new password, breaking clients that already read the first one.
   - The Secret is trusted only when its controller reference UID equals the MR's UID. Otherwise the result is error `SecretConflict`: the password is never regenerated, and the Secret is never treated as the record of the last applied password.
   - Its `password` key holds the password that was last applied. Its `rotationToken` key holds the rotation token that was last applied.
7. Fill `status.atProvider`, late-initialize `settingsProfile`, `defaultRoles` and `defaultDatabase`, and compute whether the user is up to date:

| Aspect | Desired | Observed | Rule |
|---|---|---|---|
| Method | jwt | `auth_type` | Must be exactly `['jwt']`. Several methods (from `ADD IDENTIFIED`) count as drift. |
| Processor | `jwt.processor` | parsed `PROCESSOR` | Equal. A missing processor is drift. |
| Claims | canonical JSON of `jwt.claims` | canonical JSON of the parsed `CLAIMS` | Equal (both may be absent). Canonical form: decode with `UseNumber`, re-encode with sorted keys and no spaces. The CRD guarantees an object. The server also rejects invalid and non-object claims (36) [V]. |
| Method | password | `auth_type` | Must be exactly `['sha256_password']`. `sha256_hash` users report `sha256_password` [V]. |
| Password | `passwordSecretRef` value | connection Secret `password` | Equal. A missing connection Secret counts as drift: the desired password is re-applied and published. |
| Password | `generate` | connection Secret `password`, `rotationToken` | A missing Secret or `password` key: generate a new password (Warning event `PasswordRegenerated`). `generate.rotationToken` set and different from the Secret's `rotationToken`: generate a new password (event `PasswordRotated`). |
| Expiry | none | `valid_until` (an array, one entry per method, on A26.3, A26.6 and 26.8), or `VALID UNTIL` in `SHOW CREATE USER` (25.3, 25.8) | Any entry other than the epoch is drift. Re-applying `IDENTIFIED` clears it [V 25.3, 26.6, 26.8]. For a password user, the password re-applied is the desired one (`passwordSecretRef`) or the one in the connection Secret (`generate`). |
| Hosts | `hostIP`, `hostNames` (empty means ANY) | `host_ip`, `host_names`, `host_names_regexp`, `host_names_like` | Set equality after normalisation (below). Any regexp or like entries count as drift. All four empty means `HOST NONE`, which is drift and never equals ANY [V 25.3, 26.6, 26.8]. |
| Profile | `settingsProfile` (if not nil) | `inherit_profile` rows plus any `setting_name` rows | Exactly one profile equal to desired and no individual settings; for `""`, no rows. |
| Default database | `defaultDatabase` (if not nil) | `default_database` | Equal; `""` means none [V all]. |
| Default roles | `defaultRoles` (if not nil) | `default_roles_*` | All: `all=1, except=[]`. None: `all=0, list=[]`. AllExcept: `all=1`, set-equal except. Listed: `all=0`, list set-equal to **desired ∩ granted** (below). |

**Listed default roles.** The applied list is the desired roles that are currently granted to the user, according to `system.role_grants`.

- Desired roles that are not granted are left out, and the provider emits Warning event `DefaultRoleNotGranted` naming them. It does not loop on 512.
- Once a `RoleGrant` grants such a role, the next Observe sees the larger set and re-applies `DEFAULT ROLE`. A role granted to a `Listed` user does not become default by itself [V 25.3, 26.6, 26.8].
- Server versions differ when a role is revoked:
  - On 26.6 and 26.8, `REVOKE` removes the role from `default_roles_list`, and a later `GRANT` does not restore it.
  - On 25.3 the stale entry stays in the list [V].

  Because the provider compares against desired ∩ granted and re-applies on drift, both behaviours converge.

**Host normalisation.** The server rewrites the host list [V 25.3, 26.6, 26.8]:

- `'10.1.2.3/8'` becomes `'10.0.0.0/8'`;
- `'10.0.0.0/255.0.0.0'` becomes `'10.0.0.0/8'`;
- `'10.1.2.3/32'` becomes `'10.1.2.3'`, and `'2001:db8::1/128'` becomes `'2001:db8::1'`;
- `'fd00:0::1'` becomes `'fd00::1'`;
- `'::ffff:192.168.0.1'` is kept as written;
- order is not preserved;
- a list containing `0.0.0.0/0` collapses to ANY, shown as `host_ip = ['::/0']`, and any host names are dropped;
- host names keep their case.

The comparison mirrors this with `net/netip`:

1. Parse each entry as a prefix, an address, or an address with a dotted mask, which is converted to a prefix length.
2. Apply `Masked()`.
3. Render `/32` and `/128` as a bare address.
4. Collapse to `{"::/0"}` if any entry is `0.0.0.0/0` or `::/0`.

Desired entries that fail to parse are `InvalidSpec`, and so is a full-range entry combined with `hostNames`. Host names are compared exactly.

**`SHOW CREATE USER` parser.**

- **Input.** Read the column through the driver. It returns raw SQL with literals escaped, e.g. ``CREATE USER `a'b` IDENTIFIED WITH jwt PROCESSOR 'p\'1' CLAIMS '{"hd":"it\'s \\\\ ok"}'`` [V].
  - Do not use clickhouse-client output for fixtures: its TSV output escapes again.
- **Tokens.** A small lexer recognises:
  - backtick identifiers, with escapes;
  - single-quoted literals, with `\\`, `\'`, `` \` ``, `\0`, `\t`, `\n`, `\r`, `\b`, `\f` and `\xHH`;
  - bare words and commas.
- **Grammar.**

  ```
  identified := IDENTIFIED WITH list
  list       := method { ',' method }
  method     := name [PROCESSOR lit] [CLAIMS lit] [BY lit] [SALT lit] [VALID UNTIL lit]
  ```

  `PROCESSOR` is always printed before `CLAIMS` [V]. `VALID UNTIL` is a suffix of each method and can be followed by a comma and another method. For example, `IDENTIFIED WITH sha256_password VALID UNTIL '2031-01-01 00:00:00', plaintext_password HOST ...` [V 25.3, 26.6, 26.8].
- **Stop.** The method list ends at a top-level `HOST`, `DEFAULT`, `SETTINGS`, `GRANTEES`, `IN` or end of input, **not** at `VALID`.
- **Ignored.** `BY`/`SALT` literals appear only when the server is configured to display secrets.
- **Unknown method.** Reported as is, and it counts as drift.

**Create**

```sql
-- jwt
CREATE USER `name` IDENTIFIED WITH jwt PROCESSOR '<p>' [CLAIMS '<canonical json>']
  HOST {ANY | IP '<ip>'[, '<ip>'...][, NAME '<n>'[, '<n>'...]] | NAME '<n>'[, ...]}
  DEFAULT ROLE {ALL | NONE | ALL EXCEPT `r1`[, `r2`...]}
  [DEFAULT DATABASE `db` | DEFAULT DATABASE NONE]
  [SETTINGS PROFILE '<profile>' | SETTINGS NONE]

-- password
CREATE USER `name` IDENTIFIED WITH sha256_hash BY '<hash>' SALT '<salt>'
  HOST ... DEFAULT ROLE ... [DEFAULT DATABASE ...] [SETTINGS ...]
```

- This clause order is accepted on every version [V all].
- `DEFAULT ROLE` at creation:
  - `All` (or `defaultRoles` nil): `ALL`.
  - `None`: `NONE`.
  - `Listed`: `NONE`. The granted subset is applied by the next Update.
  - `AllExcept`: `ALL EXCEPT ...` directly. A role that does not exist gives 511, `DependencyMissing`.

  A `Listed` or `AllExcept` user is therefore never left on the implicit `DEFAULT ROLE ALL`, where a role granted in the meantime, even an excluded one, would become active.
- Policy ([5.8](#58-policy-evaluation)), the name collision check and the self-modification guard run before the statement.
- After success:
  - read back `id` and set the `owned-id` annotation ([4.2](#42-ownership));
  - return `ConnectionDetails` ([9.5](#95-connection-secrets)).

**Update.** Only the aspects that drifted are sent, in at most two statements:

```sql
ALTER USER `name` [IDENTIFIED WITH ...] [HOST ...] [DEFAULT DATABASE ...] [SETTINGS PROFILE '<p>' | SETTINGS NONE]
ALTER USER `name` DEFAULT ROLE {ALL | NONE | `r1`, `r2` | ALL EXCEPT `r1`}
```

- `ALTER USER ... IDENTIFIED WITH` replaces **all** authentication methods. That also removes extra methods added with `ADD IDENTIFIED` and clears `VALID UNTIL` [V].
- `HOST` and `SETTINGS` replace the old values. `SETTINGS NONE` works [V 25.3, 26.6, 26.8].
- `DEFAULT ROLE` is kept separate so that an error in it does not block the other changes.
- If the password changed, Update returns the new `password` (and, for `generate`, the new `rotationToken`) in `ConnectionDetails`. crossplane-runtime publishes them after Update returns. If the ALTER succeeds and publishing fails, the next Observe still sees the old `rotationToken` (or a mismatched password) in the Secret and rotates again. Server and Secret converge after the first successful publish.
- **Switching from password to jwt.** Update returns `password: nil` and `rotationToken: nil` in `ConnectionDetails`. The local Secret publisher applies a JSON merge patch, and a null value removes the key [C]. An integration test confirms it [T].

**Delete:** ``DROP USER IF EXISTS `name` ``. This is only reached for owned users: [4.2](#42-ownership) turns every non-owned case into `ResourceExists=false` while the MR is being deleted.

**Connection details from Observe.** Observe returns the same full set as Create and Update. For password users it copies the current `password` and `rotationToken` from the existing connection Secret. The publisher's equality check then sees no change and does not patch the Secret on every poll.

### 5.5 Role

```sql
-- Observe
SELECT toString(id), storage FROM system.roles WHERE name = {name:String}
SELECT count() FROM system.users WHERE name = {name:String}   -- name collision
-- Create (policy: roleNamePatterns)
CREATE ROLE `name`
-- Delete (owned only)
DROP ROLE IF EXISTS `name`
```

- Ownership and the storage rules are the same as for User.
- **Name collision.** If a user with the same name exists, the result is error `NameCollision`, both before Create and in Observe. ClickHouse lets a user and a role share a name, even across storages. GRANT and REVOKE then resolve the grantee user-first, so grants meant for the role would land on the user [V 25.3, 26.6, 26.8]. The check is skipped while the MR is being deleted.
- `DROP ROLE` removes the role from every grantee and from every `default_roles_list` [V 25.3, 26.6, 26.8]. This is the expected cascade.
- v1 has nothing to update.

### 5.6 Grant

**Privilege catalogue.** Per client, the provider caches `SELECT privilege, aliases, level, parent_group FROM system.privileges`. It is re-read when the server version changes.

- The table is readable without extra grants [V].
- It has 206 rows on 25.3, 211 on 25.8, 233 on A26.3, 246 on A26.6 and 248 on 26.8 [V].
- From it the provider builds a tree using `parent_group`. Leaves are privileges that are no one's parent.
- Leaves with a NULL `level` are treated as GLOBAL. On every version tested that is only `SHOW FILESYSTEM CACHES`, plus the pseudo-privilege `NONE`, which is rejected as input [V all].

**Unmanaged privileges: source privileges** (D-9). Source privileges cannot be modelled consistently across versions:

- On 25.3, `S3`, `URL`, `REMOTE` and the rest are GLOBAL leaves under `SOURCES`, and `GRANT S3 ON *.*` is stored as an `S3` row [V 25.3].
- From 25.8 on, they are GLOBAL leaves directly under `ALL`, and `READ`/`WRITE` exist at level `SOURCE` (aliases `SOURCE READ`/`SOURCE WRITE`). `GRANT S3, URL ON *.*` is stored as `READ`/`WRITE` rows with `access_object` `S3` and `URL`, while `SHOW GRANTS` still prints `GRANT S3 ON *.*` [V 25.8, A26.3, A26.6, 26.8].
- New source privileges appear in new releases: `YTSAURUS` and `ARROW FLIGHT` on A26.6 and 26.8, and `BIGQUERY` and `DISK` on 26.8 [V].

Because the 25.8+ catalogue cannot tell a source privilege from any other GLOBAL leaf, the provider keeps an **allow-list of known non-source GLOBAL leaves directly under `ALL`**: `KILL QUERY`, `KILL TRANSACTION`, `MOVE PARTITION BETWEEN SHARDS`, `SHOW HANDLERS`, `displaySecretsInShowAndSelect`, `CLUSTER` [V all]. The following are **unmanaged**:

- `SOURCES`;
- every privilege of level `SOURCE` (`READ`, `WRITE`);
- every child of `SOURCES`;
- every GLOBAL leaf whose parent is `ALL` and that is not on the allow-list.

A new leaf of that shape in a future version is treated as unmanaged until the allow-list is updated. The rule fails closed.

**Normalisation.**

1. **Canonicalise.** Upper-case each input, collapse spaces, and match it case-insensitively against `privilege` or any `aliases` entry. The result is the canonical name; camel-case names such as `dictGet` are kept. An unknown name, `NONE` and any unmanaged privilege are `InvalidSpec`.
2. **Target level.** `*.*` is GLOBAL, `db.*` is DATABASE, `db.t` is TABLE and `db.t(cols)` is COLUMN.
3. **Expand.** `leaves(p)` is every leaf under `p` (or `p` itself if it is a leaf). Keep only the leaves applicable at the target level:
   - GLOBAL: leaf levels GLOBAL, DATABASE, TABLE, VIEW, DICTIONARY, COLUMN;
   - DATABASE: DATABASE, TABLE, VIEW, DICTIONARY, COLUMN;
   - TABLE: TABLE, VIEW, DICTIONARY, COLUMN;
   - COLUMN: COLUMN only.

   Unmanaged leaves are always dropped. A desired privilege with no applicable leaf is `InvalidSpec` (e.g. `CREATE DATABASE` on a table, which the server would reject with 509).
4. **Unsupported levels.** Leaves of the levels NAMED_COLLECTION, USER_NAME, TABLE_ENGINE, DEFINER and SOURCE are rejected in v1. `CREATE USER`, `ALTER USER`, `DROP USER` and `CREATE ROLE` are USER_NAME-level privileges [V 25.3, 26.6, 26.8]. `ALL` on `*.*` is therefore rejected. It would fail anyway for an identity bootstrapped with `access_management`, which has no named-collection rights.
5. **The model was checked against the server.** Expanding `ALL`, `ALTER`, `SHOW`, `CREATE`, `DROP`, `SYSTEM`, `INTROSPECTION`, `SOURCES` and `dictGet` with these rules matches the server exactly at DATABASE and TABLE level on 25.3, 26.6 and 26.8. At GLOBAL level the only differences are unsupported-level and unmanaged leaves and `SHOW FILESYSTEM CACHES` [V]. The server stores grants in a **version-dependent** group-compressed form. For example, `GRANT ALTER ON app.*` is stored as one `ALTER` row on 25.3 and 26.6, and as `ALTER TABLE`, `ALTER DATABASE`, `ALTER VIEW` on 26.8 [V]. The provider always compares expanded leaves, never the stored form. Recorded `system.privileges` fixtures for every version in the CI matrix keep the model honest (**OQ-14**).

**Observe**

1. **Grantee checks.**
   - For a **role** grantee, `SELECT count() FROM system.users WHERE name = {g:String}` must be 0. Otherwise the result is error `AmbiguousGrantee`: the server resolves GRANT and REVOKE grantees user-first, so every statement would hit the user [V 25.3, 26.6, 26.8]. No SQL is sent.
   - The grantee's storage comes from `system.users` or `system.roles`. A `users_xml` grantee is error `ReadOnlyStorage` when writes are allowed. The server would return 495 [V].
   - A grantee equal to `currentUser()` is error `SelfModification` when writes are allowed.
2. **Read the rows:**

   ```sql
   SELECT access_type, database, table, column, is_partial_revoke, grant_option
          [, access_object] [, is_wildcard]          -- when the columns exist (section 7)
   FROM system.grants
   WHERE user_name = {grantee:String}                -- or: role_name = {grantee:String}
   ```

3. **Wildcard check on servers without `is_wildcard`** (25.3, 25.8, A26.3, A26.6). A wildcard grant (`ON team_*.*`, `ON app.ev*`) is stored with the prefix as a plain name (`database = 'team_'`, `table = 'ev'`), identical to a literal grant on a database or table with that name [V all]. The provider then runs ``SHOW GRANTS FOR <QuoteIdent(grantee)>`` and scans each `ON` object, aware of backtick quoting, for an unquoted `*` that directly follows an identifier character or a closing backtick. `SHOW GRANTS` prints wildcards unquoted (`team_*.*`, `` `team-a`*.* ``, `app.ev*`) and literal names with `*` quoted (`` `odd*name`.* ``) [V all]. If any wildcard is found, the result is error `Unsupported: grantee <g> has wildcard grants; managing its grants needs system.grants.is_wildcard (ClickHouse 26.8 or later)`. No SQL is sent.

**Rows that are ignored.** These rows are never counted and never revoked:

- `access_type` is unmanaged, or its level is unsupported (USER_NAME, NAMED_COLLECTION, TABLE_ENGINE, DEFINER, SOURCE). On 25.3, which has no `access_object`, a prefix-scoped `GRANT CREATE USER ON svc_*` looks like a global row [V 25.3]. Only the level rule keeps it out.
- `access_object` is non-empty, where the column exists.

**Classifying the remaining rows** relative to the Grant's target T:

- **Grant rows** have `is_partial_revoke = 0`. **Revoke rows** have `is_partial_revoke = 1`. A revoke row with `grant_option = 1` revokes only the grant option. One with `grant_option = 0` revokes the privilege.
- A **wildcard row** (`is_wildcard = 1`, 26.8) with prefix P covers every database, or every table in its database, whose name starts with P, including P itself.
- **Covering rows** are grant rows at T or above it: global; database = db or a wildcard prefix of db; table = t or a wildcard prefix of t; for a COLUMN target, column ∈ cols.
- **Exact rows** are non-wildcard grant rows at exactly T. For a COLUMN target that means `column ∈ cols`.

Expand each row's `access_type` into applicable leaves. Then:

- `desired` = the expanded desired leaves.
- `effective` = leaves L for which some covering row R grants L, and no privilege-revoke row for L lies on the path below R down to and including T. For COLUMN targets this holds per column, and L counts only if it is effective for every listed column.
- `effectiveGO` = leaves L for which some covering row with `grant_option = 1` grants L, and no revoke row of either kind for L lies on that path.
- `exact` = leaves from exact rows. `exactGO` = those whose row has `grant_option = 1`.
- Revoke rows strictly **below** T do not affect T. They are another target's business.

Example: after `GRANT SELECT ON app.*` and `REVOKE SELECT ON app.t`, the rows are `(SELECT, app, -, p=0)` and `(SELECT, app.t, p=1)`. A Grant of `SELECT ON app.t` therefore sees SELECT as **not** effective. The grantee really cannot read `app.t` [V 25.3, 26.6, 26.8].

**ResourceExists**

- Normally: `exact ≠ ∅` **or** `desired ⊆ effective`.
- **While the MR is being deleted** (`meta.WasDeleted`): `(desired ∩ exact) ≠ ∅`. Coverage and foreign privileges are ignored. Otherwise a Grant whose privileges are covered by a broader grant, or one with a foreign privilege at the same target, would report "exists" forever while Delete revokes nothing, and the finalizer would never be released.

**ResourceUpToDate** = all of:

- `desired ⊆ effective`;
- if `withGrantOption`: `desired ⊆ effectiveGO`;
- `exact ⊆ desired`;
- if not `withGrantOption`: `exactGO ∩ desired = ∅`.

`status.atProvider.privileges` lists the `access_type` values of the exact rows. It is informational only.

**Semantics: a Grant owns its exact target for its grantee.**

- Privileges that someone else adds at the *same* target are revoked (drift correction, as provider-sql does for MySQL).
- Privileges at *other* targets are not touched, broader or narrower.
- Two `Grant`s with the same grantee and the same exact target fight each other. That is unsupported and documented (**OQ-1**).

**The server merges grants.** After `GRANT SELECT(a) ON app.t` and then `GRANT SELECT ON app.*`, only the database row remains. `REVOKE SELECT ON app.*` then removes both. Likewise, after `GRANT SELECT ON team_*.*`, a `GRANT SELECT ON team_a.*` adds no row [V 25.3, 26.6, 26.8].

- A narrow Grant whose row was absorbed stays effective, so it stays up to date.
- When such a Grant is deleted, `desired ∩ exact` is empty, so the MR goes away at once and nothing is revoked. The privilege remains through the broader grant.
- If the broad Grant is deleted instead, the narrow Grant sees the privilege missing on its next poll and grants it again. That is a short gap of at most one poll interval, which is acceptable and documented.

**Never create partial revokes.**

- A REVOKE at a narrow level under a broader grant creates a partial revoke. It removes part of someone else's grant [V].
- So the provider only REVOKEs leaves that are present in exact rows **and** not covered by a broader row.
- A leaf shadowed by a broader grant is left alone. The effective state is the same.
- The one exception is grant option: `REVOKE GRANT OPTION FOR` on an exact row whose leaf is covered above without grant option removes the narrow row cleanly [V 25.3, 26.6, 26.8]. With no narrow row and a broader row that has grant option, the same statement creates a partial revoke of the grant option, so it is not issued then.
- **Side effect to know:** any GRANT the provider issues at T erases third-party partial revokes at T and below T. Re-running `GRANT SELECT ON app.*` clears every partial revoke under `app.*` [V 25.3, 26.6, 26.8]. This is how a partial revoke at T is repaired, and it is documented.

**Create and Update** (idempotent statements, in this order):

```sql
-- 1. missing privileges and missing grant option
GRANT <toGrant> ON <target> TO `grantee` [WITH GRANT OPTION]
-- 2. foreign privileges at the target that nobody covers above
REVOKE <(exact − desired) not covered above> ON <target> FROM `grantee`
-- 3. foreign privileges at the target that are covered above without grant option,
--    but hold grant option at the target
REVOKE GRANT OPTION FOR <those leaves> ON <target> FROM `grantee`
-- 4. only if withGrantOption = false: desired exact leaves holding grant option,
--    when no broader row holds grant option for them
REVOKE GRANT OPTION FOR <those leaves> ON <target> FROM `grantee`
```

- `toGrant = (desired − effective) ∪ (withGrantOption ? desired − effectiveGO : ∅)`. With `withGrantOption` the statement carries `WITH GRANT OPTION`. A leaf already effective without grant option must be granted again with it; a separate narrow row with grant option is then stored [V 25.3, 26.6, 26.8].
- The target is written as `*.*`, `` `db`.* ``, `` `db`.`t` ``, or column form ``SELECT(`a`, `b`), INSERT(`a`, `b`) ON `db`.`t` ``.
- A REVOKE of a privilege that is not held is a no-op [V].

**Delete.** For each desired leaf in an exact row:

- not covered above: `REVOKE <leaf> ON <target> FROM grantee`;
- covered above without grant option, while the exact row has grant option: `REVOKE GRANT OPTION FOR <leaf> ON <target> FROM grantee`, which removes the narrow row.

192 and 511 count as already gone. The next Observe uses the deletion rule for ResourceExists.

**Policy** ([5.8](#58-policy-evaluation)): the grantee matches the user or role patterns; the database matches `databaseNamePatterns`, or is `"*"` with `allowGlobalGrants`; `withGrantOption` needs `allowGrantOption`.

The **privilege ceiling** comes from the server. The identity can only grant what it holds `WITH GRANT OPTION`; otherwise the server returns 497 naming the missing grant [V]. On a shared config this ceiling belongs to the single provider identity, not to each tenant. That is why `databaseNamePatterns` exists ([9.3](#93-provider-identity-sources-and-tenancy)).

### 5.7 RoleGrant

**Observe**

1. Grantee checks as for Grant: `AmbiguousGrantee` for a role grantee that shares its name with a user, the storage of the grantee from `system.users` or `system.roles`, and `SelfModification` for `currentUser()`.
2. Read the row:

   ```sql
   SELECT with_admin_option, granted_role_is_default
   FROM system.role_grants
   WHERE granted_role_name = {memberOf:String}
     AND user_name = {grantee:String}                -- or: role_name = {grantee:String}
   ```

   A row means it exists. It is up to date when `with_admin_option == adminOption`.

**Grantee storage rule.** The server rejects `GRANT <role> TO <users_xml user>` with 495 [V], so a `users_xml` grantee gives `ReadOnlyStorage`. Separately, a users.d user whose `grants:` configuration references an SQL role loses it at startup, and the server logs "Role ... is not defined and will be ignored" [V A26.6]. Granting a role from users.d to an **SQL** user works and survives restarts [V]. That is the intended way to combine roles managed in configuration with users managed by the provider. A role from users.d granted to an SQL user also becomes one of its default roles automatically, unless the user's `defaultRoles` is `Listed` or `None`.

**Create:** ``GRANT `memberOf` TO `grantee` [WITH ADMIN OPTION]``. 511 (missing role or grantee) is reported as `DependencyMissing`.

**Update:**

- to add the admin option: ``GRANT `memberOf` TO `grantee` WITH ADMIN OPTION``;
- to remove it: ``REVOKE ADMIN OPTION FOR `memberOf` FROM `grantee` ``.

**Delete:** ``REVOKE `memberOf` FROM `grantee` ``. Revoking a role the grantee does not hold is a no-op [V]. 192 and 511 count as already gone. On 26.x the revoke also removes the role from the grantee's `default_roles_list` ([5.4](#54-user)).

**Policy** ([5.8](#58-policy-evaluation)): `memberOf` matches `grantableRoles`; the grantee matches the user or role patterns; `adminOption` needs `allowAdminOption`.

### 5.8 Policy evaluation

The policy of the `ClusterProviderConfig` is evaluated in Observe, after the effective name is known and before the object's details are read. Namespaced `ProviderConfig`s have no policy. For them, only the checks marked "always" apply.

| Check | When | Kinds |
|---|---|---|
| Namespace in `allowedNamespaces` | in Connect, for every MR ([5.9](#59-shared-controller-wiring)) | all |
| **Name checks**: effective name, grantee, target database and `memberOf` match their patterns | **every management policy, including observe-only, and also while the MR is being deleted**. While deleting, a failed check orphans the MR (Warning event `OrphanedByPolicy`, `ResourceExists=false`, no SQL) instead of returning an error | all |
| **Write checks**: authentication method in `allowedAuthentication`; processor in `allowedJWTProcessors`; `defaultRoles.roles` in `grantableRoles`; database `"*"` needs `allowGlobalGrants`; `withGrantOption` needs `allowGrantOption`; `adminOption` needs `allowAdminOption`; a Replicated Database's `zooKeeperPath` is the default or matches `zooKeeperPathPatterns` (checked in Create, before the CREATE) | when the management policies allow Create or Update, not while deleting | Database, User, Grant, RoleGrant |
| **Always** (any config kind): users_xml storage, `SelfModification`, `NameCollision`, `AmbiguousGrantee`, jwt capability | before any write; collision checks also in Observe; not while deleting | per kind |

- Name checks apply to observe-only MRs as well. Otherwise an observe-only MR on a shared config could read any user's hosts, processor, claims, default roles and profile, or any grantee's privileges, through `status` and late-initialization.
- **Deletion is gated by ownership and by the name checks.** The ownership claim is only annotations, which the tenant can edit (forging `owner-uid` and `owned-id`, or for User and Role the create annotations that row 5 reads), so the name policy is the boundary between tenants and must hold for the most destructive operation too. A name outside the policy never blocks deletion: the MR is orphaned (Warning event `OrphanedByPolicy`) and the object is left in place. Write, capability and collision checks are skipped while deleting, so tightening a policy never leaves finalizers stuck. User and Role follow the same rule.
- A failed check returns `PolicyViolation` (or the listed reason) before any read of the object's details and before any write.
- With `unrestricted: true` every policy check passes. Adoption still needs `allowAdoption`.

| Kind | Name checks | Write checks |
|---|---|---|
| Database | name ∈ `databaseNamePatterns` | Replicated `zooKeeperPath` is `/clickhouse/databases/{uuid}` or ∈ `zooKeeperPathPatterns` |
| User | name ∈ `userNamePatterns` | method, processor, `defaultRoles.roles` |
| Role | name ∈ `roleNamePatterns` | — |
| Grant | grantee ∈ `userNamePatterns` or `roleNamePatterns` (by grantee kind); database ∈ `databaseNamePatterns` unless `"*"` | `"*"` needs `allowGlobalGrants`; `withGrantOption` needs `allowGrantOption` |
| RoleGrant | grantee ∈ user or role patterns; `memberOf` ∈ `grantableRoles` | `adminOption` needs `allowAdminOption` |

### 5.9 Shared controller wiring

- `managed.NewReconciler(mgr, resource.ManagedKind(gvk), ...)` is configured with:
  - `WithTypedExternalConnector[*v1alpha1.X]`;
  - `WithInitializers(...)`: the external-name and ownership initializer for Database, User and Role; none for Grant and RoleGrant;
  - `WithDeterministicExternalName(true)` for every kind ([4.1](#41-external-name));
  - `WithReferenceResolver(...)`: the generated resolver wrapped with the namespace check ([3.1](#31-conventions));
  - `WithPollInterval(o.PollInterval)`, default 1 m;
  - `WithManagementPolicies()`, **always**, whatever the feature flags say (D-26). crossplane-runtime honours management policies only when the controller gets this option; without it every action is allowed, and an observe-only MR would be updated and deleted;
  - `WithLogger`;
  - `WithRecorder(event.NewAPIRecorder(...))`;
  - `WithMetricRecorder`.
- Controllers are wrapped with `ratelimiter.NewReconciler` and `WithEventFilter(resource.DesiredStateChanged())`. They are registered through `SetupGated` with `o.Gate.Register(fn, gvk)`, so `crossplane.yaml` declares `capabilities: [safe-start]`.
- **Connector flow:**
  1. Resolve the config by `mg.GetProviderConfigReference().Kind`:
     - **`ClusterProviderConfig`.** Get it. Validate its policy patterns (`InvalidConfig`). Check `allowedNamespaces` (skipped with `unrestricted`). If the namespace is not allowed:
       - while the MR is being deleted, return an external client whose Observe reports `ResourceExists=false` and emits Warning event `OrphanedByPolicy`. The server object is left in place, and namespace deletion does not hang (D-20);
       - otherwise, error `PolicyViolation`.

       Secrets are read from the namespace in each selector.
     - **`ProviderConfig`.** Refused with `InvalidConfig` if `--enable-namespaced-provider-configs=false`. Get it from `mg.GetNamespace()`. Check `host:port` against `--namespaced-endpoint-pattern`. Its Secrets come from the same namespace.
  2. **Then** track usage with `resource.NewProviderConfigUsageTracker(c, &v1alpha1.ProviderConfigUsage{}).Track(ctx, mg)`. Reading a config is not using it, so this keeps Crossplane's "track before use" rule. A namespace the config denies therefore never creates a usage that would block deleting the config.
  3. Check that referenced MRs use the same ProviderConfig ([3.1](#31-conventions)).
  4. Get a client from the cache ([6.3](#63-client-cache)).
  5. Return an external client holding the client, the effective policy, the capabilities, `currentUser()`, the kube client and the uncached API reader.

  `Disconnect` releases the client reference.
- **Config controllers.** There are two `providerconfig.NewReconciler` instances, one per config kind. Both watch `ProviderConfigUsage` with `resource.EnqueueRequestForProviderConfig{Kind: ...}`. The usage tracker labels each usage with the config kind, so the two controllers do not count each other's usages [C].

### 5.10 Conditions and readiness

crossplane-runtime sets only `Creating`, `Deleting` and the `Synced` condition. Observe sets `Ready` (D-19):

| Situation | Ready |
|---|---|
| The object exists and is owned, adopted or recovered (rows 2, 3 and 5 of [4.2](#42-ownership)), whether or not it has drifted | `True` (`xpv2.Available()`). Drift is repaired by Update in the same reconcile. A failure there shows as `Synced=False`. |
| Observe-only or import mode, and the object exists | `True` |
| Grant: `desired ⊆ effective`. RoleGrant: the row exists | `True` |
| `NotOwned`, `Replaced`, `AdoptionRejected`, `ReadOnlyStorage` (when writes are allowed), `PolicyViolation`, `NameCollision`, `AmbiguousGrantee`, `SelfModification`, `AccessStorageNotReplicated`, `Unsupported`, `SecretConflict` | `False` (`xpv2.Unavailable()`, with the reason as message) |
| The object does not exist | left to crossplane-runtime (`Creating`) |

---

## 6. Authentication and clients

### 6.1 Token sources

```go
// internal/clients/clickhouse/auth
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate() // drop any cached token; the next Token() fetches a new one
}
```

| Source | Implementation | Refresh |
|---|---|---|
| `GoogleIDToken` | `oauth2.ReuseTokenSource(nil, idtoken.NewTokenSource(context.Background(), audience, opts...))`. Options: `option.WithAuthCredentialsJSON(option.ServiceAccount, keyJSON)` when a credentials Secret is given; otherwise Application Default Credentials. The deprecated `WithCredentialsJSON` and `WithCredentialsFile` are not used. | Cached until shortly before expiry (about 1 h tokens). `Invalidate` builds a new `ReuseTokenSource`. |
| `TokenFile` | `os.ReadFile(path)` on every call, `TrimSpace`. Empty files and files over 64 KiB are rejected. Paths under `/var/run/secrets/kubernetes.io/serviceaccount/` are refused. | Each new connection picks up the rotated file, e.g. a kubelet-rotated projected token. |
| `TokenSecret` | Reads the Secret key on every call through the **uncached** API reader. Dials are rare (pooled connections), and a stale cache would defeat the 516 retry. | Rotation by any external process takes effect on the next dial. |
| `PasswordSecret` | Not a token source. The password is put in `Options.Auth` at client build time. | The client cache key contains the Secret's `resourceVersion`. |

**Checks before a token is sent** (`TokenFile` and `TokenSecret`). ClickHouse sees the token before it validates anything, so the provider checks it first:

1. It must be a JWT: three base64url segments. Decode the payload **without** verifying the signature.
2. `aud` must contain the configured `audience`.
3. `aud` must not contain any audience of the provider's own Kubernetes API token. That token is read once at start from the automounted service account token, if present. This catches a projected token minted for the API server instead of for ClickHouse.
4. `exp`, if present, must not be in the past (60 s leeway).

A failed check is `InvalidConfig` and nothing is sent. This matters because Crossplane's RBAC manager gives the provider's service account full access (`*`) to Secrets, ConfigMaps, Events and Leases cluster-wide ([9.3](#93-provider-identity-sources-and-tenancy)). That credential must never reach a ClickHouse endpoint.

**Service account key validation** (`GoogleIDToken` with a credentials Secret), before the key is used:

- `type` must be `service_account`;
- `token_uri` must be empty or `https://oauth2.googleapis.com/token`;
- `universe_domain` must be empty or `googleapis.com`.

The Google library honours `token_uri` from the key and puts token-endpoint response bodies into its errors [C cloud.google.com/go/auth]. On a namespaced ProviderConfig, an unchecked key would let a tenant make the provider POST a signed assertion to any URL and read the reply back in a condition. `WithAuthCredentialsJSON(option.ServiceAccount, …)` also rejects `external_account` files ("credential type mismatch") [C]. A unit test pins this.

**Google token notes.**

- `idtoken.NewTokenSource` gets `context.Background()`, never the reconcile context. The key and impersonation paths keep that context for later refreshes, and a cancelled reconcile context would break every refresh after it.
- On GKE the metadata path requests `format=full`, so the token carries the `email` claim. The server processor should use `username_claim: email`.
- Workload Identity needs the Kubernetes service account bound to a Google service account (`iam.gke.io/gcp-service-account`). The provider runs under a service account with a fixed name, set through a `DeploymentRuntimeConfig` (`examples/runtime/`, [Appendix B](#appendix-b-server-side-configuration-examples)). Whether direct-principal Workload Identity Federation, without a Google service account, can mint ID tokens is unverified (**OQ-22**).

**Server side.** ClickHouse validates the token with a token processor ([Appendix B](#appendix-b-server-side-configuration-examples)). On the native protocol the token is checked only at handshake. Existing connections survive token expiry [V/C], which is why `pool.connMaxLifetime` defaults to 30 m.

### 6.2 Transport

v1alpha1 uses the native protocol only (D-1).

```go
func buildOptions(cfg resolvedConfig, ts auth.TokenSource) *clickhouse.Options {
	opt := &clickhouse.Options{
		Protocol:        clickhouse.Native,
		Addr:            []string{net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))},
		TLS:             cfg.tls, // *tls.Config (MinVersion TLS 1.2, RootCAs, ServerName) or nil
		Auth:            clickhouse.Auth{Database: cfg.defaultDatabase},
		DialTimeout:     5 * time.Second,
		ReadTimeout:     60 * time.Second,
		MaxOpenConns:    cfg.maxOpenConns,
		MaxIdleConns:    max(1, cfg.maxOpenConns/2),
		ConnMaxLifetime: cfg.connMaxLifetime,
		// ClientInfo: product "provider-clickhouse/<version>"
	}
	if cfg.password != nil {
		opt.Auth.Username, opt.Auth.Password = cfg.password.username, cfg.password.password
	} else {
		// Sent as user " JWT AUTHENTICATION " with the token as password,
		// called once per new connection.
		opt.GetJWT = ts.Token
	}
	return opt
}
```

**Clear text.** The native protocol sends the user name and the password, or the token in the password field, in the Hello packet. clickhouse-go applies no TLS condition to `GetJWT` [C conn.go]. Without TLS, the provider's credential is on the wire, and it can take over SQL users. TLS is therefore required by CEL ([3.3](#33-providerconfig-and-clusterproviderconfig)) unless `allowInsecureTransport: true` is set. The resolver enforces the same rule in Go (D-2).

**Rejected configurations.** The resolver refuses:

- an empty username for `PasswordSecret`;
- a token source with an empty token;
- TLS disabled without `allowInsecureTransport`;
- `insecureSkipVerify` with a token source.

### 6.3 Client cache

- **One entry per ProviderConfig identity.** The identity is the kind plus namespace/name.
- **Validity.** The entry is valid while its key matches. The key is built from:
  - the config's UID and `metadata.generation`;
  - the `resourceVersion` of every Secret that went into the options: the password Secret, the CA Secret and the Google credentials Secret.

  `TokenFile` and `TokenSecret` are read on each dial, so they are not part of the key.
- **Key change.** The old entry is marked stale and new connects get a new entry.
  - The old pool is closed explicitly (`conn.Close()`) once its reference count reaches zero. `Connect` increments the count and `Disconnect` decrements it.
  - A deleted config's entry is closed after 15 minutes without use. A sweeper goroutine runs every minute.
- **Contents.** Each entry holds:
  - the pool and the token source;
  - the [capabilities](#7-server-capability-detection) and the privilege catalogue;
  - `currentUser()`;
  - the set of server UUIDs seen recently ([section 8](#8-topology-replicas-and-clusters)).
- **Pool size.** Pools are per config, so `maxOpenConns` bounds the load on each server. Per-config token sources also keep metadata-server traffic low.

### 6.4 516 retry

If a statement fails with 516 and the auth is token-based:

1. Call `ts.Invalidate()`.
2. Replace the pool, depending on the identity's current cache entry:
   - it is still the failed pool: retire it and build a fresh one from the same configuration. The new pool's probe is **not** retried on 516 again, so one operation invalidates the token once and dials at most once more;
   - another reconcile already replaced it with a pool of the same key version: use that pool;
   - the key version changed meanwhile (new generation or Secret), or the pool was retired: do not rebuild. A pool built from the old configuration (old password, token source or endpoint) would retire the newer pool and make pools flap. The original 516 is returned as `AuthenticationFailed`, and the next reconcile resolves the new configuration.
3. Retry the statement **once**.

This is safe for every statement, including DDL: 516 is returned during authentication, before any statement runs. A second 516 is returned as `AuthenticationFailed`. Typical causes are a wrong audience, a wrong processor or clock skew beyond the 60 s server leeway. A new pool's first probe (in `Acquire`, not in a renewal) also gets one invalidate-and-retry.

Password auth is not retried. A changed password Secret already produces a new key.

### 6.5 Timeouts, concurrency and leader election

- **Timeouts.**
  - Dial: 5 s. Read: 60 s. Both are internal.
  - Per reconcile: crossplane-runtime's default of 1 m, and 2 m for `Database` with `cluster`.
  - SELECTs carry `max_execution_time = 30`.
- **Concurrency.** `--max-reconcile-rate` (default 10) bounds concurrent reconciles per controller. Pool limits bound concurrent server connections.
- **Leader election.** `--leader-election` uses a Lease with the template defaults (60 s lease, 50 s renew).
  - For HA, run 2 replicas with leader election through a `DeploymentRuntimeConfig`.
  - Client caches and token sources are per process, and only the leader reconciles.
- **Startup flags.** The provider-template flags are kept: `--debug`, `--sync`, `--poll`, `--max-reconcile-rate`, `--enable-changelogs` and `--enable-secret-cache`. `--enable-management-policies` is kept only as a hidden flag: management policies are always on (D-26), and `false` (or `ENABLE_MANAGEMENT_POLICIES=false`) stops the provider at startup with an error. New flags:
  - `--enable-namespaced-provider-configs` (default `true`);
  - `--namespaced-endpoint-pattern` (RE2 on `host:port`, default `.*`).

### 6.6 Deferred: HTTP transport

HTTP is not in v1alpha1 (**OQ-18**). The client layer implements it to these requirements, found during review, and the integration tests run the Database lifecycle over HTTP and HTTPS with a bearer token; the CRD does not offer it yet:

- **Never use `GetJWT` over HTTP.** Without TLS, clickhouse-go silently ignores it and authenticates as user `default` [V].
- **Bearer transport.** A `TransportFunc` wraps the base transport. `RoundTrip`:
  1. clones the request;
  2. clears `URL.User`;
  3. deletes every credential header the server reads (`X-ClickHouse-User`, `X-ClickHouse-Key`, `X-ClickHouse-SSL-Certificate-Auth`) and the `user`/`password` URL parameters. With TLS, clickhouse-go fills in user `default` and, because the password is empty, sends `X-ClickHouse-SSL-Certificate-Auth: on`; the server then rejects every request ("Got an empty user name from X-ClickHouse HTTP headers", 516) [V A26.6 over HTTPS];
  4. sets `Authorization: Bearer <token>` **only if `req.URL.Scheme` and `req.URL.Host` equal the configured endpoint**, and returns an error otherwise;
  5. turns any 3xx response into an error.

  clickhouse-go builds its `http.Client` with the default redirect policy. Go strips sensitive headers on cross-host redirects only at the client level, so a transport that always sets the header resends the token to a redirect target. A test endpoint that answered 307 to a second host leaked the token to that host [V].
- **POST only.** An HTTP GET forces `readonly=2`, which rejects DDL. clickhouse-go always POSTs [V + C].
- **Error bodies.** clickhouse-go returns non-200 bodies as `response body: "..."` [C]. Only ClickHouse exceptions are put on MRs. For other responses, only the status code is ([5.2](#52-errors-and-not-found)).
- **TLS.** The same `allowInsecureTransport` rule applies, because HTTP Basic and Bearer credentials are clear text without TLS.

---

## 7. Server capability detection

Capabilities are detected per client entry and refreshed every 10 minutes, which picks up server upgrades. **Probes that only some kinds need run lazily**, when the first MR that needs them is reconciled. A failing probe is reported on that MR only and never fails `Connect` for other kinds.

| Capability | Probe | When | Notes |
|---|---|---|---|
| Version | `SELECT version()` | Connect | Shown in events and errors. `altinityantalya` in the string identifies Antalya, for information only. |
| Current user | `SELECT currentUser()` | Connect | Self-modification guard ([4.2](#42-ownership)). No grant needed. |
| Server UUID | `SELECT serverUUID()` | Connect, refresh, before access-entity writes | Multi-server detection ([section 8](#8-topology-replicas-and-clusters)). No grant needed [V 25.3, 26.8]. |
| JWT users | `EXPLAIN AST CREATE USER capability_probe IDENTIFIED WITH jwt` | first jwt User | No side effects. AST: supported (A26.3, A26.6). Code 36 "CREATE USER is not supported for JWT" (25.8, 26.8) or 62 `SYNTAX_ERROR` (25.3): unsupported [V all]. Any other error goes on that MR only. Ran with the minimal identity on 26.6.4 [V]; on the other versions only as an administrator (**OQ-17**). |
| Access storage | `SELECT name, type, precedence FROM system.user_directories ORDER BY precedence` | first User, Role, Grant or RoleGrant | The first writable directory is where new entities go: `local_directory` or `replicated`. |
| Cluster hosts | `SELECT count() FROM system.clusters WHERE cluster = {c:String}` | when `spec.cluster` is set | 0 means `InvalidConfig: unknown cluster`. |
| `system.grants` columns | `SELECT name FROM system.columns WHERE database = 'system' AND table = 'grants'` | first Grant | `access_object` and `is_wildcard` ([5.6](#56-grant)). |
| `system.users` columns | the same for `table = 'users'` | first User | `valid_until` ([5.4](#54-user)). |
| Database comment | `ALTER MODIFY DATABASE COMMENT` present in the cached `system.privileges` | first Database with a comment change | Missing on 25.3. |
| Privilege catalogue | `SELECT privilege, aliases, level, parent_group FROM system.privileges` | first Grant | [5.6](#56-grant). |

**Version matrix** [V all]:

| | 25.3.14 | 25.8.33 | 26.8.23 | Antalya 26.3.13 | Antalya 26.6.4 |
|---|---|---|---|---|---|
| jwt users (probe result) | no (62) | no (36) | no (36) | yes | yes |
| `system.grants.access_object` | no | yes | yes | yes | yes |
| `system.grants.is_wildcard` | no | no | yes | no | no |
| `system.users.valid_until` | no | no | `Array(DateTime64(0))` | `Array(DateTime)` | `Array(DateTime)` |
| `ALTER MODIFY DATABASE COMMENT` privilege | no | yes | yes | yes | yes |
| Source privileges | children of `SOURCES` | `READ`/`WRITE` + `access_object` | same | same | same |
| `system.privileges` rows | 206 | 211 | 248 | 233 | 246 |

**Supported versions** (D-17). Upstream 25.3 and later; Antalya 26.3 and later. Older servers are untested and not supported. 25.3 stays in the list on purpose: it is still widely deployed, including early patch releases, so CI tests the early patch `25.3.3.42` rather than the latest 25.3 patch. On 25.3, database comments cannot be changed after creation and wildcard coverage is not available. The README carries this matrix.

**Reporting.**

- A missing capability is reported on the MR that needs it, not on the ProviderConfig:
  - for example error `Unsupported: server <version> does not support JWT users (requires Altinity Antalya)` (Synced=False, Ready=False, a Warning event);
  - no SQL is sent.
- A failing Connect-time probe (`version()`, `currentUser()`, `serverUUID()`) fails `Connect` with the underlying error. These need no grants, so a failure means the connection or the authentication is broken.
- Putting capabilities in `ProviderConfig.status` would need a dedicated health controller that writes config status (**OQ-7**).

---

## 8. Topology: replicas and clusters

- **SQL access entities are stored per server unless the user directory is replicated.**
  - With `local_directory` on several replicas or shards, each server has its own users, with different UUIDs.
  - A load-balanced endpoint would then create users on a random replica.
  - Replicas added later would have no users at all.
- **Multi-host clusters must use a `replicated` user directory**, backed by Keeper or ZooKeeper ([Appendix B](#appendix-b-server-side-configuration-examples)).
  - `user_directories` is read only at server start.
  - Replacing `local_directory` drops the SQL entities stored there. MR-managed ones are re-created by the provider: they are "not found", so Create runs and the new UUIDs are recorded. Entities restored by other means keep or change their UUID depending on the method [T]; if it changes, adopt by UUID.
- **The provider never uses `ON CLUSTER` for users, roles or grants.**
  - With replicated storage, `ON CLUSTER` would run the same DDL on every host against shared storage. All but one host would then fail (`ignore_on_cluster_for_replicated_access_entities_queries` decides how this is handled).
  - With `local_directory`, it would give divergent UUIDs per host.
- **Enforcement with `spec.cluster`.** When `spec.cluster` is set and the cluster has more than one host, the provider refuses to write users, roles or grants unless the first writable user directory is `replicated`. The error is `AccessStorageNotReplicated`.
  - Auto-detecting clusters from `system.clusters` is not reliable. Operators create helper clusters such as `all-replicated` that list every host as a replica even when the cluster is sharded.
- **Enforcement without `spec.cluster`: multi-server detection.** The most common mistake is a Kubernetes Service in front of several replicas that use `local_directory`. The provider detects it on a best-effort basis:
  - it runs `SELECT serverUUID()` at Connect, on each capability refresh, and before each batch of access-entity writes;
  - the cache entry keeps the set of server UUIDs seen within the last `connMaxLifetime`;
  - if that set has more than one member and the first writable user directory is `local_directory`, every access-entity write fails with `AccessStorageNotReplicated: endpoint <host> reaches multiple servers (<uuid1>, <uuid2>) using local_directory; use a replicated user directory or point the endpoint at one replica`.

  A pool can run on one backend for a while before a second one is seen, so detection is not immediate [T]. A deterministic per-connection check is **OQ-20**.
- **Databases** may use `ON CLUSTER <spec.cluster>`. Alternatively, a `Replicated` database engine can be created through `ON CLUSTER` once.
  - `ON CLUSTER` needs the `CLUSTER` privilege. Without it the server returns 497 "necessary to have the grant CLUSTER ON *.*" [V].
  - The explicit UUID gives every replica the same database UUID ([5.3](#53-database)).
  - `ON CLUSTER` statements address the database by name on every host. Before every `ALTER ... ON CLUSTER` and `DROP ... ON CLUSTER` the provider checks the UUID on every host and refuses the write if any host has the name with another UUID ([5.3](#53-database), D-18).
  - **Remote hosts do not re-check the initiator's `DROP TABLE` privilege.** With the default `distributed_ddl_use_initial_user_and_roles = 0` they run the DDL without re-checking [C]. Even with the setting at 1 on both hosts, `DROP DATABASE ... ON CLUSTER` by an identity without `DROP TABLE` dropped a non-empty database on the other shard [V 26.8]. Hence the cluster-wide emptiness check in [5.3](#53-database).
- **Shared Keeper paths.** Several ClickHouse clusters must not share a replicated user directory path. That would merge their users. This is a documentation note.

---

## 9. Security model

### 9.1 Trust boundaries

| Actor | Can | Trusted for |
|---|---|---|
| Cluster administrator | Install the provider and set its flags; create `ClusterProviderConfig`s; configure ClickHouse `users.d`, `token_processors` and `user_directories`. | Everything. |
| Namespace tenant | Create MRs in their namespace, and namespaced `ProviderConfig`s if `--enable-namespaced-provider-configs` allows it. Reference any `ClusterProviderConfig`: Crossplane v2 has no native restriction, so `allowedNamespaces` provides it. | Nothing beyond what the `ClusterProviderConfig` policy, the provider flags and Kubernetes RBAC allow. |
| Provider identity on ClickHouse | Whatever its grants allow ([9.4](#94-minimal-grants-for-the-provider-identity)). | Bounded by those grants and by provider-side checks. |

### 9.2 Blast radius

**With global grants** (`CREATE USER, ALTER USER, DROP USER ON *.*`, `ROLE ADMIN`), the identity that manages users and role membership can [V A26.6]:

- create users with any authentication method (including password and `no_password`) from any host;
- grant **any** role, including as `DEFAULT ROLE`;
- **take over any existing SQL user** through ``ALTER USER `x` IDENTIFIED WITH ...`` and then act with that user's privileges.

**The server can narrow this** (D-23):

- ``GRANT CREATE USER, ALTER USER, DROP USER ON `<prefix>`*`` limits user DDL to names that start with the prefix. With the prefix `team-a@`, the identity created, altered and dropped `team-a@example.com`, while `ALTER USER` on `team-b@example.com` and `CREATE USER` of `team-b@example.org` failed with 497 [V all]. The grant is stored with `access_object` = the prefix.
- ``GRANT CREATE ROLE, DROP ROLE ON `<prefix>`*`` does the same for roles [V all].
- Only **prefix** wildcards work. A fully quoted ``ON `team-a-*` `` or a suffix pattern ``ON `*@example.com` `` is stored literally and matches nothing [V 26.8, A26.6]. Email-named users therefore need a prefix convention.
- `SHOW USERS` and `SHOW ROLES` cannot be scoped: ``GRANT SHOW USERS ON `svc_`*`` fails with 509 "cannot be granted on the table level" [V all]. The identity can still read the metadata of all access entities.
- `GRANT <role> TO <identity> WITH ADMIN OPTION`, per role, can replace `ROLE ADMIN`. The server then enforces which roles may be handed out: granting `super_role` fails with 497 [V 26.8, A26.6].
  - The role must be active for the identity (`DEFAULT ROLE`). With `DEFAULT ROLE NONE` the grant fails with 497 [V].
  - Trade-off: the identity also holds that role's privileges.
  - Without `ROLE ADMIN` the identity cannot grant roles it created itself. A creator gets no admin option automatically [V]. `ROLE ADMIN` therefore remains necessary when `RoleGrant` must grant provider-created roles.

**What it cannot do:**

- change users or roles from `users.xml`/`users.d` (read-only, 495), including its own definition;
- grant privileges directly (that needs `WITH GRANT OPTION`);
- create roles (unless granted);
- read data.

**Provider-side checks remain mandatory**, because tenants on one identity share its server-side ceiling:

- ownership claims ([4.2](#42-ownership)): no ALTER or DROP of objects it did not create unless adopted by UUID;
- `users_xml` objects are never written;
- the identity never modifies `currentUser()`;
- the closed-by-default policy: namespaces; user, role and database patterns; `grantableRoles`; authentication methods; processors; global grants; grant and admin options; adoption;
- name-collision and ambiguous-grantee checks;
- no `no_password` users, and jwt users always pin a processor.

**Hardening recommendations:**

- Define the provider identity in `users.d`, so the provider cannot modify itself or the break-glass admin.
- Use `grants:` (explicit privileges), not `access_management: 1`. The two cannot be combined (BAD_ARGUMENTS) [C], and `access_management` lacks named-collection rights.
- Prefer prefix-scoped user and role DDL, and per-role `WITH ADMIN OPTION` for roles from users.d.
- Do not put "super roles" in `users.d` that the provider could hand out. Set `grantableRoles`.
- For hard isolation between tenants, give each tenant its own identity and its own `ClusterProviderConfig`, with server-side grants scoped to the tenant's prefix.

### 9.3 Provider identity sources and tenancy

**Ambient identities are allowed only on `ClusterProviderConfig`.** These are `TokenFile` and `GoogleIDToken` without a key.

- A namespaced `ProviderConfig` is tenant-owned, including its endpoint.
- If ambient identity were allowed there, a tenant could point the endpoint at their own server and receive the provider's projected Kubernetes token, or a Google ID token for the provider's service account with an audience they choose. Such a token can be valid for other services that trust Google ID tokens.
- CEL forbids `TokenFile` on `ProviderConfig`, and the Go type makes `credentialsSecretRef` mandatory for `GoogleIDToken` there.

**`TokenFile` can never send the provider's Kubernetes credential.** Crossplane's RBAC manager grants a provider's service account full access (`*`) to Secrets, ConfigMaps, Events and Leases across the cluster [C crossplane v2.0.2 `internal/controller/rbac/provider/roles/roles.go`], not just get/list/watch. The well-known automount path is refused, and the audience checks in [6.1](#61-token-sources) reject tokens minted for the API server. The docs show a projected volume with `audience: clickhouse` and `expirationSeconds: 3600`.

**Namespaced `ProviderConfig`s are controlled by provider flags** (D-3):

- `--enable-namespaced-provider-configs=false` turns them off entirely.
- `--namespaced-endpoint-pattern` restricts the `host:port` they may target, for example `clickhouse-[a-z0-9-]+\.clickhouse\.svc\.cluster\.local:9440`.
- Without a restriction, a tenant can make the provider pod open native-protocol connections to addresses the tenant's own pods may not reach. Two measures limit what comes back: error text is sanitised ([5.2](#52-errors-and-not-found)), and Google keys are validated ([6.1](#61-token-sources)). What remains is port probing, through the difference between "connection refused" and a timeout. Operators of shared clusters should set the pattern.
- Namespaced `ProviderConfig`s have no policy. The tenant owns their credentials anyway.

**On shared `ClusterProviderConfig`s, the policy is the security boundary.** It is closed by default. Set at least:

- `allowedNamespaces`;
- `userNamePatterns`, `roleNamePatterns` and `databaseNamePatterns`, all prefixed with `{namespace}` followed by a separator. Without `databaseNamePatterns`, tenant A could grant its role `SELECT` on tenant B's database whenever the shared identity holds that privilege `WITH GRANT OPTION` on a wildcard;
- `grantableRoles`;
- `allowedAuthentication` and `allowedJWTProcessors`.

Keep `allowGlobalGrants`, `allowGrantOption`, `allowAdminOption` and `allowAdoption` false. A tenant user that holds `ADMIN OPTION` on a shared role, or `GRANT OPTION` on privileges, can act outside the provider. On 26.8, such a user revoked a shared role from another tenant's user, and created a partial revoke on that user's `SELECT` [V 26.8].

Real isolation between tenants needs both name patterns and server-side scoping. Preferably each tenant gets its own identity and `ClusterProviderConfig`, with prefix-scoped grants ([9.2](#92-blast-radius)).

**Other tenancy rules:**

- References cannot leave the MR's namespace ([3.1](#31-conventions)).
- Observe-only MRs are subject to the name checks ([5.8](#58-policy-evaluation)).
- Annotations, including the ownership claims, are writable by anyone who can edit the MR ([4.2](#42-ownership)). Claims bound to the MR's UID prevent accidents, not malice.

**Kubernetes RBAC and admission.**

- Restrict who may create `Grant`, `Role`, `Database` and namespaced `ProviderConfig` in shared namespaces.
- A `ValidatingAdmissionPolicy` can enforce rules such as "RoleGrant.memberOf must start with the namespace label `team`". Examples go in `docs/security.md`.

**Credentials in Secrets** (passwords, service account keys, tokens) follow standard Secret RBAC.

### 9.4 Minimal grants for the provider identity

In `users.d` the same lines go under `grants: {query: [...]}`. That the prefix syntax works there is [T]. Each row was verified with an SQL identity unless marked.

| Feature | Grants | Why |
|---|---|---|
| Always | none for `version()`, `currentUser()`, `serverUUID()`, the `EXPLAIN AST` probe, `system.privileges` and `CHECK GRANT` | Connect and capability probes |
| User, Role, Grant, RoleGrant | `GRANT SELECT ON system.user_directories` | storage detection |
| `spec.cluster` | `GRANT SELECT ON system.clusters` | topology check; without it every cluster MR fails with 497 [V 26.8] |
| User | ``GRANT CREATE USER, ALTER USER, DROP USER ON `<prefix>`*`` (or `ON *.*`); `GRANT SHOW USERS, SHOW ROLES ON *.*` | DDL [V all for the prefix form]. `SHOW USERS` opens `system.users` and `SHOW CREATE USER`. `SHOW ROLES` is for the name-collision check. |
| User with `settingsProfile` | `GRANT SELECT ON system.settings_profile_elements` | profile drift (fails with 497 without it [V]) |
| User with `Listed` default roles, RoleGrant | `GRANT SELECT ON system.role_grants`. For each grantable role from users.d: `GRANT <role> TO <id> WITH ADMIN OPTION`, with the role among the identity's default roles. `ROLE ADMIN ON *.*` only if RoleGrant must grant roles the provider creates. | membership reads; server-enforced grantable set [V 26.8, A26.6] |
| Role | ``GRANT CREATE ROLE, DROP ROLE ON `<prefix>`*`` (or `ON *.*`); `GRANT SHOW ROLES, SHOW USERS ON *.*` | DDL [V all for the prefix form]; name-collision check (`ALTER ROLE` once role settings exist) |
| Grant | `GRANT SELECT ON system.grants`; `GRANT SHOW USERS, SHOW ROLES ON *.*`; plus every delegable privilege `WITH GRANT OPTION` at the widest scope it may delegate, e.g. `GRANT SELECT, INSERT, SHOW TABLES ON team_*.* TO <id> WITH GRANT OPTION` | server-enforced ceiling; grantee checks; `SHOW GRANTS FOR` on servers without `is_wildcard` |
| Database | `GRANT CREATE DATABASE, DROP DATABASE, SHOW TABLES ON <prefix>*.*`, e.g. `ON team_*.*` (wildcard grants work [V]); on 25.8 and later also `ALTER MODIFY DATABASE COMMENT` on the same scope (the privilege does not exist on 25.3, and granting it fails with 62) | DDL plus the emptiness check. Do **not** grant `DROP TABLE`: a single server then refuses to drop non-empty databases [V]. |
| Database with `spec.cluster` | `GRANT CLUSTER ON *.*` [V]; `GRANT SELECT ON system.clusters` (above); for Update and Delete also `GRANT REMOTE ON *.*` (stored as `READ ON REMOTE` and `WRITE ON REMOTE` on 25.8 and later) | `ON CLUSTER`; the per-host ownership check through `clusterAllReplicas(..., view(...))`, which needs nothing else [V 25.3, 25.8, 26.8]. `REMOTE` also allows the `remote()` and `cluster()` table functions toward arbitrary hosts. Without it, cluster Databases can be created and observed, but every comment change and Delete fails with `PermissionDenied` ([9.6](#96-recommended-management-policies)). |

**Upstream with password auth.** A provider identity in `users.xml` with `access_management: 1` cannot `GRANT ALL ON *.*`, because it lacks named-collection rights. Use explicit privileges [C]. A complete users.d example for this case is in [Appendix B](#appendix-b-server-side-configuration-examples).

### 9.5 Connection Secrets

These are written by crossplane-runtime's local publisher to the MR's namespace (type `connection.crossplane.io/v1alpha1`, controller ownerRef to the MR).

| User kind | Keys |
|---|---|
| password | `username`, `password`, `endpoint`, `port`, `protocol`, `tls`, plus `rotationToken` when `generate.rotationToken` is set |
| jwt | `username`, `endpoint`, `port`, `protocol`, `tls`, `jwtProcessor`. No secret material: the workload brings its own token. |

- `endpoint` and `port` are taken from the ProviderConfig endpoint. A separate published endpoint is **OQ-8**.
- `protocol` uses the API casing: `Native` (D-22). `tls` is `"true"` or `"false"`.
- Key names are exported constants in `apis/v1alpha1`, so compositions can rely on them. The `xpv2` constants are used where they exist.
- For password users the Secret is also the provider's record of the last applied password and rotation token. It is read with the uncached API reader, trusted only when controlled by the MR ([5.4](#54-user)), and its name is immutable.

### 9.6 Recommended management policies

- **Database:** `managementPolicies: ["Observe", "Create", "Update", "LateInitialize"]`, which is in crossplane-runtime's supported set. Deleting the MR then never drops data. This is the default in `examples/`, and strongly recommended with `cluster` ([5.3](#53-database)), where Delete always needs `forceDrop`. If `Delete` is kept on a single host, the emptiness check and the missing `DROP TABLE` grant still protect non-empty databases.
- **Import an existing object:**
  1. Start with `["Observe", "LateInitialize"]` (no ownership needed; name policy applies).
  2. Read the UUID from `status.atProvider.id`.
  3. Set `clickhouse.m.infrabay.io/adopt: "<uuid>"` (policy permitting) and switch to `["*"]`.
- **Import existing grants or memberships:** keep `Delete` out of `Grant` and `RoleGrant`. Deleting them revokes even grants that existed before the MR.
- **Observe-only monitoring:** `["Observe"]`.
- **User, Role, Grant, RoleGrant:** `["*"]` is fine. Deleting a `User` drops it, which is intended for app identities.

---

## 10. Testing strategy

### 10.1 Unit tests (`go test -race ./...`, no network, no Docker)

- **Quoting and parameters** ([5.1](#51-sql-safety)), table-driven with adversarial inputs.
- **Error redaction.** No hash, salt, password or token appears in any returned error, condition message or event. Covers server syntax errors that echo the statement, and transport errors with HTTP bodies.
- **SQL builders.** Golden strings per kind and per drift combination: CREATE (with `UUID`, `DEFAULT ROLE ALL EXCEPT`, `DEFAULT DATABASE`), ALTER, GRANT, REVOKE, `REVOKE GRANT OPTION FOR`, `DEFAULT ROLE`, `ON CLUSTER`, `clusterAllReplicas`.
- **Password hash.** The test vector from [3.5](#35-user). A test also asserts that the salt is appended as hex text, never as decoded bytes.
- **`SHOW CREATE USER` parser.** Fixtures captured through the driver from every version in the CI matrix:
  - escaped claims and processors;
  - multiple methods, with `VALID UNTIL` between methods;
  - `HOST` variants, including `HOST NONE`;
  - `DEFAULT DATABASE`, `DEFAULT ROLE`, `SETTINGS PROFILE`;
  - backtick names with escapes.
- **Claims canonicalisation and host normalisation**, including mask notation, `/32` and `/128`, the full-range rule and case-sensitive names.
- **Privilege model.** Uses recorded `system.privileges` dumps (`testdata/privileges/<version>.tsv`, one per CI version) and recorded `system.grants` rows. Cases:
  - `ALL ON db.*`, `ALL ON db.t`, column grants, version-dependent compression (`ALTER`);
  - merged grants;
  - **revoke-aware coverage**: a partial revoke at T under a broad grant, revokes below T, column revokes;
  - **grant option** (the cases of A17): a plain GRANT keeps an existing option; re-grant with option under a broader row without it; clean removal of a narrow row with option; no partial revoke of an option under a broader row that holds it;
  - **deletion**: a covered Grant, and a foreign privilege at the same target, each reaches `ResourceExists=false`;
  - wildcard rows with `is_wildcard`, and the `SHOW GRANTS` wildcard scanner (backtick-aware; `team_*.*`, `` `team-a`*.* ``, `app.ev*` versus `` `odd*name`.* ``);
  - source rows (`S3` on 25.3; `READ`/`WRITE` + `access_object` on 25.8+) and USER_NAME-level rows are ignored and never revoked;
  - the unmanaged-privilege classification, including the allow-list;
  - partial-revoke avoidance.
- **Ownership decision table.** Every row of [4.2](#42-ownership), crossed with deleting/not deleting and with observe-only vs write policies. Plus:
  - a copied manifest (`owner-uid` mismatch);
  - a restored manifest;
  - import followed by a switch to `["*"]`;
  - recovery after `create-pending` and after `create-outcome: unknown`;
  - an adopt annotation with a wrong UUID, and with `"true"`;
  - a leftover adopt annotation on an owned object.
- **Initializer:** effective-name precedence (`forProvider.name`, then the annotation, then `metadata.name`), conflicts, no-op while deleting, claim creation only with `Create` in the policies.
- **Policy evaluation:**
  - anchoring, `{namespace}` quoting and the placement rule (valid: `{namespace}_.*`, `system:serviceaccount:{namespace}:.*`; invalid: `{namespace}-.*`, `.*_{namespace}_.*`, `[a-z]+{namespace}_x`);
  - closed by default, and `unrestricted`;
  - each kind's name and write checks, crossed with observe-only, and skipped while deleting.
- **References:** a cross-namespace `*Ref` or `*Selector` is rejected before resolution; a referenced MR with a different ProviderConfig is rejected.
- **ProviderConfig resolution:**
  - both kinds;
  - Secret namespace rules;
  - rejection of ambient identity on the namespaced kind;
  - `allowedNamespaces`, including `OrphanedByPolicy` while deleting;
  - Track only after the namespace check;
  - `--enable-namespaced-provider-configs` and `--namespaced-endpoint-pattern`;
  - error paths (missing PC, missing Secret, Track failure, invalid kind), as provider-sql does.
- **Client cache:**
  - key changes on generation and Secret `resourceVersion`;
  - ref-counted close;
  - idle sweep;
  - stale-on-516;
  - the server-UUID set and the `AccessStorageNotReplicated` decision, with a fake pool.

  A fake `Open` func is used.
- **Token sources:**
  - file re-read and rotation;
  - Secret re-read through the API reader;
  - `ReuseTokenSource` invalidation with a fake `oauth2.TokenSource`;
  - pre-send checks: audience present, API-server audience rejected, expired token rejected, automount path refused, non-JWT rejected;
  - service account key validation, and rejection of an `external_account` file.
- **Connection Secret handling:** only a Secret controlled by the MR is trusted; reads use the API reader; the rotation token comparison; `password: nil` on the switch to jwt.
- **External clients per kind.** A mock `DB` interface (in the style of provider-sql's `mockDB`, with `MockExec`/`MockQuery`), `test.MockClient`, `cmp` with `test.EquateErrors()`/`EquateConditions()`. They cover Observe, Create, Update and Delete, including every "never treat as not found" path and the `Ready` table of [5.10](#510-conditions-and-readiness).

### 10.2 Integration tests (build tag `integration`)

- **Harness.** testcontainers-go v0.44.0 with `modules/clickhouse`:
  - containers are named with `testcontainers.WithName("xpch-it-<test>-<rand>")`;
  - the run uses `TESTCONTAINERS_RYUK_DISABLED=true`, because the reaper container is not `xpch-`-prefixed;
  - each test calls `t.Cleanup(func(){ testcontainers.TerminateContainer(c) })`.

  Plain `docker run --name xpch-...` is a fallback.
- **Matrix** (D-17): upstream `25.3` (patch `25.3.3.42`), `25.8` and `26.8`; Antalya `26.3.x` and `26.6.x`. Antalya `26.6.8+` is added when available (**OQ-16**).
- **Upstream images:**
  - `CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1` as the bootstrap admin;
  - a `users.d` provider identity with exactly the [9.4](#94-minimal-grants-for-the-provider-identity) password-mode grants, prefix-scoped, mounted with `testcontainers.WithFiles`.
- **Antalya images:**
  - `config.d/token_processors.yaml` with two `jwt_static_key` HS256 processors (expected issuer and audience set, `username_claim: email` and `preferred_username`). The keys are random per run and written to a temp dir, never committed.
  - A `users.d` provider identity with `jwt: {processor: test}` and the minimal grants.
  - Tokens are minted in Go with `golang-jwt/jwt/v5` into a temp file used as `TokenFile`. The test rotates the file mid-run to prove it is re-read.
  - Clock skew: tokens get `iat = now - 10s`.
- **Scenarios** (all images unless noted):
  1. Full lifecycle per kind through the real external clients: Observe → Create → Observe up to date → drift injected with admin SQL → Update → Delete.
  2. jwt User with processor and claims (object form) round-trip (Antalya). Cross-processor login fails once the processor is pinned. On upstream, the probe reports unsupported (36 or 62) and no SQL is sent.
  3. Password user (`sha256_hash`): log in with the published password; rotate it with `rotationToken`; delete the Secret and see `PasswordRegenerated`; switch to jwt (Antalya) and check that the `password` key is removed [T].
  4. Ownership:
     - collision with a pre-existing user gives `NotOwned`;
     - adopt by UUID with and without policy, and with a wrong UUID;
     - DROP and re-create by admin gives `Replaced`;
     - a copied manifest gives `NotOwned`;
     - a context cancelled right after the CREATE is sent yields `CreateRecovered`;
     - a `users.d` user gives `ReadOnlyStorage` and no writes;
     - deleting a non-owned MR leaves the server object.
  5. Minimal grants: the identity with exactly [9.4](#94-minimal-grants-for-the-provider-identity), prefix-scoped, passes. Removing each grant in turn gives `PermissionDenied`, never "not found". A name outside the prefix gives 497.
  6. Grants:
     - the expansion model for `ALL` matches the server;
     - merged grants, and deleting a covered Grant;
     - partial revokes at T are repaired, and none are created;
     - grant option add/remove (the cases of A17);
     - wildcard grants (26.8: coverage; others: `Unsupported`);
     - source grants left untouched;
     - a role grantee shadowed by a same-named user gives `AmbiguousGrantee`.
  7. Default roles: `Listed` created as `NONE` and filled as RoleGrants appear; `AllExcept` at creation; revoke behaviour per version.
  8. User drift: `VALID UNTIL` set out of band is cleared; `HOST NONE` is repaired; `DEFAULT DATABASE`.
  9. Database:
     - creation with an explicit UUID;
     - a forged claim on a name outside the tenant's policy, with `forceDrop`, is released on deletion without a DROP;
     - `Replicated` with the default `{uuid}` path and no `cluster` (Antalya with embedded Keeper);
     - out-of-band drop and re-create with the same UUID;
     - `EXISTS DATABASE` with and without privileges;
     - the emptiness check with `CHECK GRANT`, and `forceDrop`;
     - refusal by the server without `DROP TABLE`;
     - comment change on 25.3 gives `Unsupported`.
  10. Cluster (every upstream image; `TestClusterDatabase`: two replicas of one shard, interserver secret, embedded Keeper on the first):
      - `ON CLUSTER` create, comment change and drop reach both hosts;
      - a same-named database with another UUID on one host is never altered or dropped: the comment change fails with `NotOwned`, and deleting the MR (with `forceDrop`) orphans it;
      - `Replicated` with the default `{uuid}` path registers one replica per host; a Keeper path of another database is refused with `PolicyViolation` and registers nothing;
      - Delete refuses without `forceDrop`;
      - adversarial names through the `clusterAllReplicas(..., view(...))` query.
      Still to add: two shards, a replicated user directory, and the grant matrix (without `REMOTE`, comment changes and Delete fail with `PermissionDenied`).
  11. Multi-server detection: two containers behind a round-robin TCP proxy with `local_directory` give `AccessStorageNotReplicated`.
  12. 516: an expired token followed by a valid file triggers exactly one retry and succeeds.
  13. Names: `@`, `.`, quote, backslash, TAB, backtick, `*` and Unicode through every kind.
  14. Redaction: a forced syntax error on an `IDENTIFIED` statement leaves no hash or salt in conditions or events.

### 10.3 API tests (mandatory)

- **envtest** (`sigs.k8s.io/controller-runtime/tools/setup-envtest`). Every CRD in `package/crds` is installed into a real kube-apiserver. CEL rules (`exists_one`, transition rules on optional fields, list equality, `__namespace__`) fail their compile or cost checks only there. Each rule gets positive and negative cases.
- **Examples.** Every file in `examples/` is applied with server-side dry-run against the envtest API server.
- **`crossplane beta validate package/crds examples/`** runs in CI.

### 10.4 End-to-end

1. Create a kind cluster `xpch-e2e`.
2. Install Crossplane with the `crossplane-stable/crossplane` Helm chart. The CI matrix runs the oldest supported 2.x release (2.0.x) and the latest one. That keeps the `crossplane.yaml` constraint honest.
3. `kubectl apply -f package/crds`.
4. Run the provider out of cluster (`go run ./cmd/provider --debug`) against a ClickHouse container.
5. Assert with `kubectl wait` or chainsaw, including an MRAP that activates `*.clickhouse.m.infrabay.io`.

Installing the real xpkg into kind needs a local registry, so it is deferred.

---

## 11. Repository, build, packaging, CI and versioning

### 11.1 Layout

```
.
├── apis/
│   ├── generate.go                 # go:generate controller-gen + angryjet
│   ├── clickhouse.go               # AddToScheme
│   ├── unreleased-crds/            # generated CRDs of kinds without a controller (envtest only, never packaged)
│   └── v1alpha1/
│       ├── doc.go, register.go
│       ├── providerconfig_types.go # ProviderConfig, ClusterProviderConfig, ProviderConfigUsage, shared types
│       ├── database_types.go  user_types.go  role_types.go  grant_types.go  rolegrant_types.go
│       ├── connection.go           # connection Secret key constants
│       └── zz_generated.*.go       # deepcopy, managed, managedlist, pc, pcu, pculist, resolvers
├── cmd/provider/main.go
├── internal/
│   ├── clients/clickhouse/         # DB interface, options, cache, errors, redaction, quote, params, capabilities
│   │   ├── auth/                   # token sources, pre-send checks, key validation
│   │   └── sqlx/                   # builders, SHOW CREATE parser, SHOW GRANTS wildcard scanner, hosts, claims, privileges
│   ├── controller/
│   │   ├── register.go             # SetupGated
│   │   ├── config/                 # ProviderConfig + ClusterProviderConfig controllers
│   │   ├── providerconfig/         # Resolve(), policy, flags
│   │   ├── ownership/              # initializer, decision table, annotation patching
│   │   └── database/ user/ role/ grant/ rolegrant/
│   └── version/
├── package/
│   ├── crossplane.yaml
│   └── crds/                       # generated; only kinds with a controller (internal/controller.ManagedKinds)
├── cluster/images/provider-clickhouse/Dockerfile
├── examples/
│   ├── quickstart-upstream-password/   # users.d identity, Secret, ClusterProviderConfig, Database, Role, Grant, User, RoleGrant
│   ├── quickstart-antalya-jwt/         # token processor, users.d identity, ClusterProviderConfig, jwt User
│   ├── runtime/                        # DeploymentRuntimeConfig: GKE Workload Identity; projected token
│   ├── mrap.yaml                       # ManagedResourceActivationPolicy for *.clickhouse.m.infrabay.io
│   └── <kind>.yaml                     # one file per kind; cluster Databases without Delete
├── test/integration/               # build tag "integration"; testdata/{antalya,upstream,cluster}/{config.d,users.d}
├── test/envtest/                   # CRD validation and example dry-run
├── hack/boilerplate.go.txt         # SPDX header
├── docs/                           # design.md, api.md (generated), auth.md, security.md, operations.md, upgrade.md
├── .github/workflows/{test.yml,release.yml}
├── Makefile  .golangci.yml  renovate.json  .gitleaksignore
└── LICENSE  NOTICE  README.md  CONTRIBUTING.md  DEVELOPMENT.md  SECURITY.md
```

**Headers and attribution.**

- New files carry `// SPDX-License-Identifier: Apache-2.0` and `// Copyright <year> The provider-clickhouse Authors`.
- Files adapted from crossplane/provider-template or crossplane-contrib/provider-sql keep the "Copyright 2020 The Crossplane Authors" notice in their header, name the upstream file, and state that they were modified, as Apache-2.0 §4(b) and §4(c) require. Today these are `cmd/provider/main.go`, `internal/controller/config/config.go`, `apis/clickhouse.go` and `apis/v1alpha1/register.go`.
- `NOTICE` lists exactly the files that contain adapted portions. Candidates for future adaptation: the `DB` interface, the error helpers, the password drift pattern, the grant diff helpers and the test scaffolding; each is added to `NOTICE` when it is adapted, not before.
- **Packaging.** `make generate` writes every CRD into `package/crds` and then moves the CRDs of kinds without a controller to `apis/unreleased-crds`. A unit test (`internal/controller`) fails when `package/crds` and `ManagedKinds` disagree, so a kind can only be packaged together with its controller.
- provider-sql's `Setup` dereferences `o.MetricOptions` without a nil check; our version must check.

### 11.2 Dependencies and versions

Stack (A) is the lowest-risk option. It matches crossplane-runtime v2.4.2's own `go.mod` and was verified to build, test and regenerate cleanly.

| Component | Version |
|---|---|
| Go | `go 1.26.0`, `toolchain go1.27.2` |
| crossplane-runtime | `github.com/crossplane/crossplane-runtime/v2 v2.4.2` |
| Crossplane shared APIs | `github.com/crossplane/crossplane/apis/v2 v2.4.2` (`xpv2 ".../core/v2"`) |
| Kubernetes | `k8s.io/* v0.35.3`, `sigs.k8s.io/controller-runtime v0.23.1` |
| Code generation (`tool` directives in go.mod) | `sigs.k8s.io/controller-tools/cmd/controller-gen v0.20.0`; `github.com/crossplane/crossplane-tools/cmd/angryjet v0.0.0-20260715161912-60e57f817ad1` (newer crossplane-tools raise k8s to 0.36) |
| ClickHouse client | `github.com/ClickHouse/clickhouse-go/v2 v2.49.0` |
| Google | `google.golang.org/api v0.301.0`, `golang.org/x/oauth2 v0.37.0` |
| CLI flags | `github.com/alecthomas/kingpin/v2 v2.4.0` |
| Tests | `github.com/google/go-cmp v0.7.0`, `github.com/testcontainers/testcontainers-go{,/modules/clickhouse} v0.44.0`, `github.com/golang-jwt/jwt/v5 v5.3.1`, `sigs.k8s.io/controller-runtime/tools/setup-envtest` |
| Tooling (CI env, pinned with renovate comments) | golangci-lint v2.14.0, govulncheck v1.8.0, Crossplane CLI v2.5.0 (downloaded from `cli.crossplane.io` with sha256 check; not a go.mod tool, because its dependency graph is too large). Also `crd-ref-docs`, cosign, syft; their versions are pinned when the workflows are written. |

Renovate:

- extends `local>infrabay/renovate`;
- `ignorePaths: ["testdata/**"]`;
- a lockstep rule that disables separate updates for `k8s.io/**` and `sigs.k8s.io/**`, so they move only together with crossplane-runtime.

### 11.3 Makefile targets

| Target | Does |
|---|---|
| `generate` | `go generate ./apis/...`: deepcopy, CRDs into `package/crds`, angryjet method sets and resolvers; then `crd-ref-docs` into `docs/api.md` |
| `build` | `go build ./...` |
| `test` | `go test -race -count=1 ./...` |
| `envtest` | CRD validation and example dry-run against setup-envtest's kube-apiserver |
| `validate` | `crossplane beta validate package/crds examples/` |
| `lint` | `golangci-lint run` (infrabay `.golangci.yml`: version 2, `std-error-handling` preset, errcheck relaxed in tests) |
| `vuln` | `go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...` |
| `integration` | `TESTCONTAINERS_RYUK_DISABLED=true go test -tags integration -count=1 ./test/integration/...` |
| `reviewable` | `generate` + `lint` + `test` + `envtest` + `validate`, then fail if `generate` changed files |
| `run` | `kubectl apply -f package/crds && go run ./cmd/provider --debug` against the current kubeconfig |
| `crossplane` | Fetch the pinned Crossplane CLI into `bin/` (sha256-verified) |
| `image` | For each `linux/{amd64,arm64}`: `CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build -trimpath -ldflags "-s -w -X .../internal/version.Version=$(VERSION)" -o bin/linux_<arch>/provider ./cmd/provider`, then `docker buildx build --platform linux/<arch> --output type=docker,dest=_output/image-<arch>.tar` |
| `xpkg` | `crossplane xpkg build --package-root=package --examples-root=examples --embed-runtime-image-tarball=_output/image-<arch>.tar -o _output/provider-<arch>.xpkg` |
| `xpkg-push` | `crossplane xpkg push -f _output/provider-amd64.xpkg,_output/provider-arm64.xpkg ghcr.io/infrabay/provider-clickhouse:$(VERSION)` (CI only) |

There is no `build` git submodule.

The Dockerfile only copies the prebuilt binary, so cross-arch builds need no QEMU:

```dockerfile
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG TARGETOS
ARG TARGETARCH
COPY bin/${TARGETOS}_${TARGETARCH}/provider /usr/local/bin/provider
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/provider"]
```

`package/crossplane.yaml`:

```yaml
apiVersion: meta.pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-clickhouse
  annotations:
    meta.crossplane.io/source: github.com/infrabay/provider-clickhouse
    meta.crossplane.io/license: Apache-2.0
    meta.crossplane.io/description: >-
      Self-hosted ClickHouse SQL access management (not ClickHouse Cloud):
      databases, users, roles and grants, with JWT users on Altinity Antalya.
spec:
  crossplane:
    version: ">=v2.0.0"   # must equal the oldest version the e2e job runs (10.4)
  capabilities: [safe-start]
```

### 11.4 CI (GitHub Actions)

**`test.yml`** triggers on `workflow_call`, `pull_request`, and `push` to `main` (paths-ignore `README.md`). Permissions: `contents: read`. Actions are pinned by SHA with renovate comments.

| Job | Steps |
|---|---|
| build | `gofmt -l` must be empty; `go vet`; `go build`; `make generate` with a diff check (CRDs and `docs/api.md`); `go test -race -count=1 ./...`; a "no workstation paths" `git grep` guard |
| envtest | `make envtest validate` (mandatory) |
| lint | golangci-lint-action v9 |
| govulncheck | `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` |
| integration | runs on ubuntu with Docker; `make integration`; a matrix over the five images in [10.2](#102-integration-tests-build-tag-integration), plus the cluster scenario |
| e2e | kind + Crossplane 2.0.x and latest ([10.4](#104-end-to-end)) |
| gitleaks | gitleaks v8 CLI (the org action needs a licence); fake test secrets carry `gitleaks:allow` |

**`release.yml`** runs on `v*` tags.

1. `jobs.test: uses: ./.github/workflows/test.yml`.
2. **Matrix job (amd64, arm64):**
   - build the binary;
   - `docker buildx` the image tarball;
   - `crossplane xpkg build`;
   - upload the artifact.
3. **Push job** (`environment: release`; `permissions: contents: write, packages: write, id-token: write, attestations: write`; `cache: false`):
   - download both artifacts;
   - `docker/login-action` to ghcr with `GITHUB_TOKEN`;
   - `crossplane xpkg push -f amd64.xpkg,arm64.xpkg ghcr.io/infrabay/provider-clickhouse:${GITHUB_REF_NAME}`. This builds the multi-arch index, and the tag must be semver. The image carries the annotation `org.opencontainers.image.source=https://github.com/infrabay/provider-clickhouse`.
   - `cosign sign --yes ghcr.io/infrabay/provider-clickhouse@<digest>` (keyless, through the workflow's OIDC token);
   - `actions/attest-build-provenance` for the package digest;
   - an SBOM generated with syft and attached to the release;
   - `gh release create --generate-notes`, with the `.xpkg` files and the SBOM attached.
4. **One-time step**, documented in `DEVELOPMENT.md`: GHCR packages are private on first push, so the package must be made public once.

goreleaser is not used: it cannot build xpkgs.

### 11.5 Versioning, upgrades and docs

- **Versioning.**
  - Releases are SemVer tags `vX.Y.Z`. The project stays on `v0.x` while the API is `v1alpha1`.
  - **Patch releases are additive only.**
  - A field rename adds the new field, reads both for at least one minor release, and marks the old one deprecated in its description.
  - A field removal needs a release note that names its pruning effect. When a field disappears from the CRD schema, the API server prunes stored values, which silently turns an enforced field into "not managed".
  - Breaking changes are allowed only in minor releases, called out in release notes and `docs/upgrade.md`. No conversion webhooks for alpha. `v1beta1` is planned with a conversion webhook.
  - Dev builds are not published by default. If they are, the tag is `v0.0.0-<yyyymmddHHMMSS>-<sha12>`.
- **Docs.**
  - `README.md`:
    - what it is: self-hosted ClickHouse SQL access management, not ClickHouse Cloud;
    - a quick start;
    - the compatibility matrix: Crossplane core versions, and the ClickHouse versions from [section 7](#7-server-capability-detection);
    - an MRAP example that activates `*.clickhouse.m.infrabay.io`, which safe-start installs need;
    - "How this differs". The section covers JWT users and identities on Antalya, UUID-based ownership and adoption safety, the closed-by-default ClusterProviderConfig policy, and native Crossplane v2. It also lists what lansweeper-oss/provider-clickhousedbops (an Upjet provider for self-hosted SQL objects) offers that v1 does not: settings profiles, row policies and masking policies. And it notes that other packages named `provider-clickhouse` target ClickHouse Cloud.
  - `docs/design.md`: this document.
  - `docs/api.md`: generated with `crd-ref-docs`.
  - `docs/auth.md`:
    - server-side token processor setup, with the three Kubernetes JWKS variants from Appendix B;
    - Google Workload Identity: the KSA must be bound to a GSA for metadata-server ID tokens;
    - projected tokens;
    - a sidecar pattern for non-Google identity providers: a refresher container, added through a DeploymentRuntimeConfig, writes a client-credentials token to an `emptyDir` that `TokenFile` reads (**OQ-19**);
    - a decision guide between provider-managed jwt users and Antalya's `token` user directory. The `token` directory creates ephemeral users from claims and maps groups to roles; with it, manage only `Role` and `Grant`, and Role names must match its `roles_mapping` output.
  - `docs/security.md`: [section 9](#9-security-model) as an operator guide, with VAP examples, prefix-scoped grants and the one-identity-per-tenant pattern. It warns that `.` must be escaped in email patterns.
  - `docs/operations.md`, a runbook:
    - migrating to a new management cluster or restoring from backup: claims become invalid, so adopt by UUID, with `allowAdoption` enabled temporarily if needed;
    - recovering from a lost Create;
    - "Deleting a Grant or RoleGrant revokes even pre-existing grants";
    - rotating passwords;
    - unblocking a stuck deletion.
  - `docs/upgrade.md`.
  - `examples/`.
  - `CONTRIBUTING.md`: commands, the rule of no network in unit tests, PR rules.
  - `DEVELOPMENT.md`: local Docker-based integration, the `xpch-` container naming rule, the GHCR one-time step.
  - `SECURITY.md`: GitHub private vulnerability reporting; the latest minor version is supported.

---

## 12. Decisions

Decisions taken while applying the review. Where the critics disagreed, or offered alternatives, the choice and the reason are recorded here.

| # | Topic | Options raised | Decision | Why |
|---|---|---|---|---|
| D-1 | Transport | Usability review: ship Native only. Security review: fix the Bearer transport's redirect leak. | **Native only in v1alpha1.** `Protocol` keeps a one-value enum. The HTTP design, including the redirect fix, is kept in [6.6](#66-deferred-http-transport) (**OQ-18**). | Native covers in-cluster use on every tested image. Dropping HTTP removes the redirect leak, the readonly-GET trap and HTTP-body reflection. Adding HTTP later is additive. |
| D-2 | TLS | Security review: require TLS for token sources. | TLS required for **all** credential sources unless `allowInsecureTransport: true`. `insecureSkipVerify` is forbidden with tokens. | The native Hello carries a password in clear just like a token, and the provider password is as powerful as its token. |
| D-3 | Policy on namespaced ProviderConfig | Both reviews: remove it (or keep only `allowAdoption`). | **Removed.** Adoption through a namespaced PC is always allowed. Namespaced PCs are governed by `--enable-namespaced-provider-configs` (default `true`) and `--namespaced-endpoint-pattern` (default any). | A tenant-editable policy protects nothing against the tenant. Keeping namespaced PCs enabled by default matches other Crossplane providers. Native-only transport, error sanitising and key validation reduce the residual SSRF to port probing. Shared clusters should set the pattern. |
| D-4 | Policy default | Security review: closed by default, or an `unrestricted` switch. | **Both**: `spec.policy` is required; empty lists allow nothing; `unrestricted: true` opens everything except adoption. | A `ClusterProviderConfig` named `default` without a policy must not hand every namespace the full identity. The switch keeps single-tenant installs short. |
| D-5 | Namespace patterns | Usability review: glob or `namespaceSelector`. Security review: separator characters that cannot occur in namespaces. | RE2 everywhere. The example is fixed (`team-.*`). `{namespace}` must be preceded only by literal text and followed by a non-namespace character. | One syntax for all patterns. A label selector needs RBAC on Namespaces, which providers lack by default (**OQ-25**). The separator rule alone is not enough: a regexp wildcard to the left of `{namespace}` makes names ambiguous again (`.*_{namespace}_.*` matches `a_b_c_d` for both `b` and `c`). |
| D-6 | Where ownership is recorded | Security review: annotations, not status. | Annotations `owner-uid` + `owned-id`. A claim is valid only when `owner-uid` equals the MR's UID. | Status is reset on full-object updates and filled by observe-only MRs. Binding to the UID also stops copied manifests (`owned-id` copied along) from taking over objects. The cost is that restores need re-adoption (**OQ-21**). |
| D-7 | Adoption | Security review: UUID-valued annotation; `"true"` only behind a flag, one-time. | UUID-valued only. `"true"` is not accepted. One-time: removed after use. | Removes the race between a human's check and the adoption. One form is simpler to document and test. |
| D-8 | Crash during Create | Security review: deterministic external names, a UUID for Database, a `create-outcome` annotation. Semantics review: a provider-generated database UUID. | All adopted. Recovery for User/Role also requires a valid owner claim. | The runtime's guard is meant for server-assigned names, and here every name is in the spec. The owner claim keeps recovery from applying to copied manifests. |
| D-9 | Source privileges in Grant | Semantics review: a static deny-list, or modelling as `READ`/`WRITE` + `access_object`. Usability review: exclude from observation, never revoke, declare a non-goal. | Rejected in spec **and** ignored in observation. They are identified by an allow-list of known non-source GLOBAL leaves under `ALL`, which fails closed. | New source privileges appear in each release (`YTSAURUS`, `ARROW FLIGHT`, `BIGQUERY`, `DISK` were seen), so a deny-list would always lag. Modelling sources is left for a later version. |
| D-10 | Wildcard grants | Semantics review. | On 26.8+, wildcard rows cover matching targets. Elsewhere, a grantee with wildcard grants makes its Grants `Unsupported`. Wildcard *targets* remain out of scope (**OQ-3**). | Safe on servers where wildcards cannot be told apart from literal names (**OQ-23**). |
| D-11 | Partial revokes | Semantics review. | Coverage is revoke-aware at and above the target. Revokes below the target are ignored. The side effect of GRANT is documented. **OQ-2** is closed. | Matches verified server behaviour without fighting third-party revokes at other targets. |
| D-12 | Password rotation | Security review: a `rotate-password` annotation. Usability review: `generate.rotationToken` in spec. | `generate.rotationToken`. The applied token is stored in the connection Secret (`rotationToken` key), not in an annotation. | Typed and discoverable, and it suits GitOps. Recording the token in the Secret makes "ALTER, then publish" converge after a partial failure, because Update cannot persist annotations. |
| D-13 | JWT processor | Security review: required by default with an `allowAnyProcessor` opt-out. Usability review: a required field. | **Required field**, no opt-out in v1. | Cross-issuer impersonation was verified. Relaxing later is additive, while tightening later would break users. |
| D-14 | RoleGrant field names | Usability review: `memberOf` plus `user`/`role` as the grantee. | Adopted before the first release. | Same meaning as in Grant and in provider-sql. Renaming after v0.1.0 would be breaking. |
| D-15 | JWT claims type | Usability review: a YAML object. | `*apiextensionsv1.JSON` with `type: object`. Provider-side canonicalisation is kept. | Easier to read and diff, with no escaping. |
| D-16 | `DEFAULT DATABASE` | Usability review: in scope. Semantics review: it can block logins. | In scope as `defaultDatabase` (tri-state, late-initialized). | Cheap, observable, and needed by most application users. |
| D-17 | Supported versions and CI | Semantics review: declare a minimum, test 25.3 or exclude it. Usability review: add the 25.3 and 25.8 LTS lines. | Upstream 25.3+ and Antalya 26.3+. CI covers 25.3 (patch 25.3.3.42), 25.8, 26.8, Antalya 26.3 and 26.6. | All five were probed during this revision. Capability detection makes the differences explicit. 25.3 is kept, not raised to 25.8: early 25.3 patches are still in production use. The Database integration tests pass on 25.3.3.42; facts marked [V 25.3] were probed on 25.3.14.14 and are re-checked on 25.3.3.42 when the User, Role and Grant controllers are built. |
| D-18 | Cluster Database deletion | Usability review; code review: the `clusterAllReplicas(system.tables)` count can be filtered on remote hosts and silently return 0. | **Revised:** in cluster mode Delete always needs `forceDrop`. Before every ON CLUSTER ALTER or DROP the UUID is checked on every host (`REMOTE`), and another UUID on any host refuses the write. Examples omit `Delete` for cluster Databases. | Neither the single-replica check nor the `DROP TABLE` backstop protects data on other shards, and no unfilterable remote emptiness check exists. ON CLUSTER writes go by name to every host, so ownership must hold on every host. |
| D-19 | When `Ready` is true | Security review: Available when the object exists, is owned and is up to date. | Available when the object exists and ownership is fine, **regardless of drift**. | Update runs in the same reconcile. Requiring "up to date" would keep `Ready=False` for a whole poll interval after every repaired drift. Failures still show as `Synced=False`. |
| D-20 | Namespace removed from `allowedNamespaces` while MRs are deleted there | Security review: orphan with an event, or document the hang. | Orphan, with Warning event `OrphanedByPolicy`. | Unblocks namespace deletion and never drops data. |
| D-21 | Pool knobs | Usability review: expose only two. | `maxOpenConns` and `connMaxLifetime`; the rest are internal. | Fewer fields to support. Adding fields later is additive. |
| D-22 | `protocol` key in connection Secrets | Usability review: enum casing, or document the mapping. | Enum casing (`Native`). New keys `tls` and `jwtProcessor`. | No mapping to document. |
| D-23 | Identity scoping | Security review: prefix grants and per-role admin option. | Recommended in [9.2](#92-blast-radius) and [9.4](#94-minimal-grants-for-the-provider-identity). `ROLE ADMIN` is kept only where RoleGrant must grant provider-created roles. | Verified on all five versions (prefix grants). A creator gets no admin option on its own roles. |
| D-24 | `TokenSecret.audience` | Security review: required on ClusterProviderConfig. | Required on both kinds. | One rule. A tenant knows the audience of their own processor. |
| D-25 | Name policy while deleting | Code review: a forged claim plus `forceDrop` dropped another tenant's database, because the name policy was skipped while deleting. | Name checks run while deleting too. Outside the policy the MR is orphaned (`OrphanedByPolicy`, no SQL), so deletion never blocks. Delete re-checks the policy and the UUID before the DROP. | Annotations are tenant-editable; the policy is the tenant boundary and must cover the most destructive operation. |
| D-26 | Management policies | Code review: crossplane-runtime honours them only with `managed.WithManagementPolicies()`, which was tied to `--enable-management-policies`. | Always passed. The flag is hidden and `false` is refused at startup. Second guard: while deleting, Decide reports only Owned, Adopted and Recovered as existing, whatever the policies; Update and Delete refuse without a valid claim. | Ownership depends on policies: an observe-only MR must never alter or drop anything. |
| D-27 | Replicated engine Keeper path | Code review: a tenant registered a replica under another tenant's Replicated database path (verified on 26.8: the victim's DDL failed with 159). | Without `unrestricted`, only `/clickhouse/databases/{uuid}` and paths matching `policy.zooKeeperPathPatterns` are allowed, checked before CREATE. | "Contains `{uuid}`" is not enough: a path nested under another database's node (`.../victim/replicas/{uuid}`) is just as harmful. |
| D-28 | HTTP bearer transport over TLS | Code review. | The transport strips every credential header the server reads (including `X-ClickHouse-SSL-Certificate-Auth`) and the `user`/`password` URL parameters. Covered by an HTTPS integration test. | clickhouse-go asks for certificate authentication with user `default` over TLS when the password is empty; the server rejects that with a bearer token (verified on Antalya 26.6). |

---

## 13. Open questions

| # | Question | Current decision |
|---|---|---|
| OQ-1 | Grant semantics: exclusive per exact target (chosen), or additive with a status-tracked applied set? Should overlapping Grants (same grantee and target) be detected across MRs? | Exclusive per target. Overlaps are documented as unsupported. |
| OQ-3 | Wildcard database or table *targets* (`ON team_*.*`) in `Grant`. | Not in v1. Coverage by existing wildcard rows is handled ([5.6](#56-grant)). |
| OQ-4 | A per-membership `RoleGrant.default`, against the user-level `User.defaultRoles` (chosen). | User-level only. |
| OQ-5 | Support `no_password` users restricted to `HOST LOCAL`, behind a policy flag? | No. |
| OQ-6 | Out-of-band password changes cannot be detected (the hash is hidden). Use `display_secrets_in_show_and_select` where enabled? | No. Documented limitation. |
| OQ-7 | A ProviderConfig health controller (server version, capabilities, auth check, pattern errors in `status`), and custom MR condition types. | Not in v1. Errors use stable reason prefixes; config problems are Warning events. |
| OQ-8 | Publish a different endpoint, port or protocol in connection Secrets than the one the provider uses. | Not in v1. |
| OQ-9 | Google service account impersonation (`impersonate.IDTokenSource`) and `external_account` credential files. | Not in v1. Cluster-scoped only if added. |
| OQ-10 | For `Database` with `cluster`, Observe reads only the connected replica. Verify existence and comment on every replica too? | Connected replica only for Observe (so polling needs no `REMOTE`); every ON CLUSTER write, and Observe while deleting, checks the UUID on every replica ([5.3](#53-database)). |
| OQ-11 | Settings profiles, quotas, row policies, per-user settings, `VALID UNTIL`, `HOST REGEXP/LIKE`, role settings, role-level profiles. | Later versions. |
| OQ-14 | The privilege expansion model must match the server for every supported version. | Verified at DATABASE and TABLE level on 25.3, 26.6 and 26.8. Per-version fixtures and integration tests gate releases. 25.8 and A26.3 fixtures still to be recorded. |
| OQ-16 | Behaviour differences in Antalya 26.6.8+ (OR REPLACE, native Hello marker) and newer upstream releases. | Add to the CI matrix when available. No OR REPLACE is used. |
| OQ-17 | Does the `EXPLAIN AST` jwt probe need privileges on any supported version? | Verified with the minimal identity on 26.6.4 only; integration tests run it with the minimal identity on every image. |
| OQ-18 | HTTP(S) transport. | Deferred; design in [6.6](#66-deferred-http-transport). |
| OQ-19 | An `OIDCClientCredentials` token source (token URL, client ID, client secret Secret, scopes/audience) for non-Google identity providers, cluster-scoped only. | Not in v1; the sidecar pattern in `docs/auth.md` covers it meanwhile. |
| OQ-20 | Deterministic multi-server detection: run `serverUUID()` on every new pooled connection (needs a per-connection hook in clickhouse-go). | Best effort through repeated probes ([section 8](#8-topology-replicas-and-clusters)). |
| OQ-21 | Claims bound to the MR's UID become invalid after a restore. A restore-aware mode (for example a one-time provider flag) instead of adopting each object? | Adopt by UUID; runbook in `docs/operations.md`. |
| OQ-22 | Can direct-principal Workload Identity Federation (no Google service account) mint ID tokens through the metadata server? | Unverified; docs require a KSA bound to a GSA. |
| OQ-23 | Wildcard grants on servers without `is_wildcard`: Grants for such grantees are `Unsupported`. Model `SHOW GRANTS` output fully instead? | Not in v1. |
| OQ-24 | Warn when policy patterns are wider than the server-side scope of the identity (read `system.grants.access_object` of `currentUser()`). | Not in v1. |
| OQ-25 | `allowedNamespaces` as a `namespaceSelector` (label-based). | Not in v1: it needs RBAC on Namespaces. |
| OQ-26 | Is `REMOTE` acceptable for the cluster ownership check, or should the provider connect to each host listed in the config instead? | `REMOTE` for cluster Databases that Update or Delete; Delete also needs `forceDrop`, and examples omit `Delete`. |
| OQ-27 | Warn when `defaultDatabase` names a database that does not exist, which blocks logins? | Not in v1; documented. |

**Closed in revision 2:**

| # | Question | Answer |
|---|---|---|
| OQ-2 | Partial revokes below a Grant's target. | A partial revoke at the target, under a covering row, makes the privilege not effective, and `GRANT` at the target repairs it. Revokes below the target are ignored. Any provider GRANT at T erases third-party revokes at and below T, which is documented [V 25.3, 26.6, 26.8]. |
| OQ-12 | A `databaseNamePatterns` policy. | Added. Server grants alone cannot separate tenants who share one identity ([9.3](#93-provider-identity-sources-and-tenancy)). |
| OQ-13 | Referenced MRs must use the same ProviderConfig. | Yes, for `*Ref` and `*Selector`, and references cannot leave the namespace ([3.1](#31-conventions)). Plain names are not checked. |
| OQ-15 | Is `system.grants` filtered for grantees the identity cannot see? | No. With only `SELECT ON system.grants`, all rows are visible; `system.role_grants` is the same [V 25.3, 26.6, 26.8]. |

---

## Appendix A: verified ClickHouse behaviour

Versions as defined at the top: plain [V] means Antalya 26.6.4 and upstream 26.8.23.6. "Prior tests" are checks from the earlier design rounds.

| # | Behaviour | Status |
|---|---|---|
| A1 | ``CREATE USER `n` IDENTIFIED WITH jwt [PROCESSOR 'p'] [CLAIMS '{json}']`` works. `SHOW CREATE USER` round-trips it with PROCESSOR first and CLAIMS verbatim. `system.users.auth_type = ['jwt']`, `auth_params = ['{}']`. The server rejects invalid or non-object CLAIMS and an empty PROCESSOR with 36. | [V A26.3, A26.6] |
| A2 | A nonexistent PROCESSOR is accepted at DDL time; login then fails. | [V] (prior tests) |
| A3 | Upstream rejects jwt users. 25.8 and 26.8: `CREATE USER is not supported for JWT` (36), including `EXPLAIN AST`. 25.3: `SYNTAX_ERROR` (62) "Expected one of: BY, end of query". | [V 25.3, 25.8, 26.8] |
| A4 | Native login: user `" JWT AUTHENTICATION "` with the token as password (clickhouse-go `GetJWT`, once per new connection). HTTP: `Authorization: Bearer` only. clickhouse-go over plain HTTP ignores `GetJWT` and sends `default`. | [V] (prior tests) + [C] |
| A5 | HTTP GET forces `readonly=2`. clickhouse-go always POSTs. | [V] + [C] |
| A6 | Native tokens are checked only at handshake; 516 on an invalid or expired token; 60 s default leeway. | [V] (prior tests) |
| A7 | users.xml/users.d entities are read-only: ALTER, GRANT, or CREATE with the same name returns 495, even with `IF EXISTS` / `IF NOT EXISTS`. | [V 25.3, 26.6, 26.8] |
| A8 | A users.d role granted to an SQL user survives a restart. `GRANT <SQL role> TO <users.d user>` returns 495. A users.d user whose `grants:` configuration references an SQL role loses it at startup. | [V] |
| A9 | Plain `CREATE USER` on an existing name returns 493. `CREATE USER IF NOT EXISTS` silently succeeds, changes nothing and keeps the UUID. | [V 25.3, 26.6, 26.8] |
| A10 | `CREATE USER` without `IDENTIFIED` creates a `no_password` user. | [V] |
| A11 | `sha256_hash BY hex(SHA256(pw ‖ salt-as-text)) SALT salt` logs in with `pw`; hashing the hex-decoded salt bytes gives 516. Test vector: `Pa55word` + `00112233445566778899aabbccddeeff` → `6257310c…61848188`. Every build requires a 64-hex-digit hash (36 otherwise). `SHOW CREATE USER` shows `IDENTIFIED WITH sha256_password` only. `query_log` is masked. | [V 25.3, 26.6, 26.8] |
| A12 | `ALTER USER ... IDENTIFIED WITH ...` replaces all methods, including ones added with `ADD IDENTIFIED`, and clears `VALID UNTIL`. `HOST` and `SETTINGS` replace the old values; `SETTINGS NONE` works. A combined `ALTER USER ... IDENTIFIED ... HOST ... SETTINGS NONE DEFAULT ROLE ALL` works. | [V 25.3, 26.6, 26.8] |
| A13 | Host rewrites: CIDR masked, mask notation converted (`/255.0.0.0` → `/8`), `/32` and `/128` shown bare, IPv6 compressed, order changed. `0.0.0.0/0` collapses to ANY (`['::/0']`) and drops host names. Host names keep their case. `HOST NONE` gives four empty arrays. `HOST LOCAL` gives `host_names=['localhost']`. | [V 25.3, 26.6, 26.8] |
| A14 | `DEFAULT ROLE` with a non-granted role returns 512; with an unknown role, 511. `DEFAULT ROLE ALL EXCEPT r` with an existing but not granted role works. An unknown grantee returns 511 in `GRANT r TO x` and in privilege `GRANT`/`REVOKE`. REVOKE of a privilege or role that is not held is a no-op. | [V 25.3, 26.6, 26.8] |
| A15 | New users start with `DEFAULT ROLE ALL`. With `Listed` or `NONE`, a later `GRANT` does not make the role default. REVOKE removes the role from `default_roles_list` on 26.6 and 26.8, and a later GRANT does not restore it; on 25.3 the stale entry stays. `DROP ROLE` removes the role from every list. | [V 25.3, 26.6, 26.8] |
| A16 | Grants are stored expanded to level-applicable privileges in a version-dependent group-compressed form (`GRANT ALTER ON app.*`: `ALTER` on 25.3 and 26.6; `ALTER TABLE`, `ALTER DATABASE`, `ALTER VIEW` on 26.8). A narrow grant is absorbed by a broader one. `REVOKE` at the broader level removes both. A narrow REVOKE under a broad grant creates a partial revoke row. `GRANT` at T removes partial revokes at T and below. Grants on nonexistent databases are accepted. | [V 25.3, 26.6, 26.8] |
| A17 | `REVOKE GRANT OPTION FOR` works. `GRANT ... WITH GRANT OPTION` under a broader row without grant option stores a separate narrow row; `REVOKE GRANT OPTION FOR` then removes it cleanly. With only a broader row holding grant option, `REVOKE GRANT OPTION FOR` at a narrow level creates a partial revoke of the option (`is_partial_revoke = 1, grant_option = 1`). | [V 25.3, 26.6, 26.8] |
| A18 | Wildcard grants `ON team_*.*` and `ON app.ev*` are stored with the prefix as the name (`database = 'team_'`, `table = 'ev'`), identical to a literal grant without `is_wildcard` (only 26.8 has it). `SHOW GRANTS` prints wildcards unquoted and literal `*` names quoted. Wildcards absorb narrower grants; a narrow REVOKE under one creates a partial revoke. | [V all] |
| A19 | Source privileges: on 25.3 `S3`, `URL`, ... are children of `SOURCES` and stored by name. From 25.8 they are GLOBAL leaves under `ALL`, `READ`/`WRITE` exist at level `SOURCE`, and `GRANT S3 ON *.*` is stored as `READ`/`WRITE` with `access_object = 'S3'` while `SHOW GRANTS` prints `GRANT S3 ON *.*`. New sources per version: `YTSAURUS`, `ARROW FLIGHT` (A26.6, 26.8), `BIGQUERY`, `DISK` (26.8). | [V all] |
| A20 | A user and a role may share a name, even across storages. GRANT and REVOKE resolve the grantee user-first. `CREATE ROLE <users.d user name>` succeeds, and a later GRANT to that name returns 495. `CREATE USER <users.d role name>` succeeds, and grants to that name land on the new user. | [V 25.3, 26.6, 26.8] |
| A21 | `system.grants` columns: 25.3 `user_name, role_name, access_type, database, table, column, is_partial_revoke, grant_option`; 25.8, A26.3 and A26.6 add `access_object`; 26.8 also adds `is_wildcard`. `system.users.valid_until`: absent on 25.3 and 25.8, `Array(DateTime)` on A26.3 and A26.6, `Array(DateTime64(0))` on 26.8, one entry per method. `default_database` exists everywhere. `auth_params` is `Array(String)`. | [V all] |
| A22 | `system.databases` and `system.tables` are silently filtered by privileges; `EXISTS DATABASE` fails with 497 instead. `system.users`, `roles`, `role_grants`, `grants` and `settings_profile_elements` fail with 497 without a grant. `system.grants` and `system.role_grants` are not filtered by grantee. `system.privileges` needs no grant. | [V 25.3, 26.6, 26.8] |
| A23 | `DROP DATABASE` on a non-empty database needs `DROP TABLE` on its tables (497 otherwise) on a single server. With `ON CLUSTER`, remote hosts do not enforce it, even with `distributed_ddl_use_initial_user_and_roles = 1`. | [V]; [V 26.8 two shards] |
| A24 | `CREATE DATABASE ... COMMENT` works everywhere and stores the comment unescaped. `ALTER DATABASE ... MODIFY COMMENT` works on A26.6 and 26.8. The privilege `ALTER MODIFY DATABASE COMMENT` exists on 25.8, A26.3, A26.6 and 26.8. On 25.3 the privilege is missing, and both the statement and the GRANT are syntax errors (62). | [V all] (statement: [V 25.3, 26.6, 26.8]) |
| A25 | ``CREATE DATABASE `n` UUID '<uuid>' ENGINE = Atomic`` works. The order name, UUID, ON CLUSTER, ENGINE, COMMENT parses. After `DROP DATABASE` (empty or not), the same UUID can be reused immediately. Reusing a UUID that is in use fails with 57. `Replicated('/clickhouse/databases/{uuid}', ...)` needs an explicit UUID or `ON CLUSTER` (36 otherwise). `engine_full` keeps macros unexpanded. | [V all]; Replicated [V A26.6 with Keeper] |
| A26 | Database names are escaped into file names; a 164-character name containing `@` fails with 458 "File name too long". | [V] |
| A27 | Server-side parameters work over Native and HTTP for names with `@`, `.`, quote, backslash, backtick and Unicode. TAB and newline need escaped-format encoding (`ParamValue`). | [V] both protocols |
| A28 | `SHOW CREATE USER` through the driver returns raw SQL with escaped literals; clickhouse-client TSV output double-escapes. `VALID UNTIL` is a per-method suffix that can precede `,` and another method. | [V] |
| A29 | `CREATE USER ... COMMENT` is a syntax error. There is no server-side ownership marker. | [V 25.3, 26.6, 26.8] |
| A30 | `CREATE OR REPLACE USER` keeps the same id and the existing grants on 25.3, 26.6.4 and 26.8. DROP followed by CREATE changes the UUID. | [V 25.3, 26.6, 26.8] |
| A31 | The minimal identity can create jwt users with HOST and SETTINGS PROFILE, grant and revoke roles, set default roles, and drop users. Without `SELECT ON system.grants`/`system.settings_profile_elements` those reads fail with 497. | [V A26.6] |
| A32 | `ON CLUSTER` with `local_directory` gives different UUIDs per host. With replicated storage, `ON CLUSTER` on access entities fails on all but one host. `ON CLUSTER` needs the `CLUSTER` privilege. | [V] (prior tests) + [C]; CLUSTER [V] |
| A33 | Prefix grants: ``CREATE USER, ALTER USER, DROP USER ON `p`*`` and ``CREATE ROLE, DROP ROLE ON `p`*`` restrict DDL to names starting with `p` (497 otherwise). Only prefixes work. `SHOW USERS` cannot be scoped (509). Per-role `WITH ADMIN OPTION` restricts which roles the identity may grant; the role must be active for it. A role's creator gets no admin option on it. | [V all]; admin option [V 26.8, A26.6] |
| A34 | `CHECK GRANT <privilege> ON <target>` returns 1 or 0 for the current user, including privileges held through roles. | [V 25.3, 26.6, 26.8] |
| A35 | A jwt user without PROCESSOR accepts a token from any processor whose username claim matches (cross-issuer login). CLAIMS arrays match by inclusion. | [V A26.6] |
| A36 | `DEFAULT DATABASE <name>` is accepted for a nonexistent database; `DEFAULT DATABASE NONE` clears it; `SHOW CREATE USER` prints it before `DEFAULT ROLE`. The clause order `HOST ... DEFAULT ROLE ... DEFAULT DATABASE ... SETTINGS ...` is accepted. | [V all] |
| A37 | `serverUUID()` and `currentUser()` need no grant. | [V 25.3, 26.8] |
| A38 | `SYNTAX_ERROR` messages quote the statement from the failure position, including password hashes and salts; 36 and 180 do not. | [V 26.8] |
| A39 | 509 `INVALID_GRANT` for privileges at the wrong level, e.g. "CREATE DATABASE cannot be granted on the table level". | [V 25.3, 26.6, 26.8] |
| A40 | `CREATE USER`, `ALTER USER`, `DROP USER` and `CREATE ROLE` are USER_NAME-level privileges. On 25.3, a prefix-scoped `GRANT CREATE USER ON svc_*` appears in `system.grants` with `database = NULL` and no way to see the prefix. | [V 25.3, 26.6, 26.8] |

---

## Appendix B: server-side configuration examples

These belong to the ClickHouse deployment, not to the provider. Names are examples.

**Token processors (Antalya, `config.d/token_processors.yaml`).** Processor types in Antalya 26.6 include `google`, `openid`, `entra`, `jwt_static_key`, `jwt_static_jwks` and `jwt_dynamic_jwks` [C].

```yaml
token_processors:
  google:                                   # Google ID tokens (GoogleIDToken source, jwt users)
    type: jwt_dynamic_jwks
    jwks_uri: https://www.googleapis.com/oauth2/v3/certs
    expected_issuer: https://accounts.google.com
    expected_audience: https://clickhouse.example.com   # = credentials.googleIDToken.audience; unique per deployment
    username_claim: email
```

**Kubernetes service account tokens** (TokenFile source, and jwt users named `system:serviceaccount:<ns>:<name>`). Copy `issuer` and `jwks_uri` from `<issuer>/.well-known/openid-configuration` (`kubectl get --raw /.well-known/openid-configuration`). Choose one variant:

```yaml
token_processors:
  # (a) Managed cluster with a public issuer (GKE, EKS, AKS): fetch the public JWKS.
  #     The path differs per platform, so always take jwks_uri from the discovery document.
  kubernetes:
    type: jwt_dynamic_jwks
    jwks_uri: <jwks_uri from the discovery document>
    expected_issuer: <issuer from the discovery document>
    expected_audience: clickhouse
    username_claim: sub                     # system:serviceaccount:<ns>:<name>

  # (b) Self-managed cluster: the discovered jwks_uri is the API server, which
  #     needs authentication and uses a private CA. Use a static JWKS file filled with
  #     `kubectl get --raw /openid/v1/jwks`, mounted from a ConfigMap, and refreshed
  #     when the signing keys rotate.
  kubernetes_static:
    type: jwt_static_jwks
    static_jwks_file: /etc/clickhouse-server/jwks/kubernetes.json
    expected_issuer: https://kubernetes.default.svc.cluster.local
    expected_audience: clickhouse
    username_claim: sub
```

(c) Alternatively, on a self-managed cluster, bind the ClusterRole `system:service-account-issuer-discovery` to `system:unauthenticated`, trust the cluster CA in ClickHouse's `openSSL.client` configuration, and use variant (a) against the API server. On a test cluster the discovered `jwks_uri` was the API server's IP. An anonymous fetch returned 401, and the CA was untrusted [V].

**Generic OIDC identity provider** (for jwt users), using discovery [C]:

```yaml
token_processors:
  keycloak:
    type: openid
    configuration_endpoint: https://idp.example.com/realms/main/.well-known/openid-configuration
    expected_issuer: https://idp.example.com/realms/main
    expected_audience: clickhouse
    username_claim: preferred_username
    groups_claim: groups
```

**Points to know:**

- **Failure is server-wide.** A processor that fails to parse disables token authentication for the whole server.
- **JWKS fetch failures.** A failed JWKS fetch is cached for `jwks_cache_lifetime`.
- **URL allow-list.** If `remote_url_allow_hosts` is set, it must include every discovery, JWKS and userinfo host [C].
- **Token type.** Antalya warns at startup when `expected_typ` is empty [C]. Set it where the identity provider issues typed tokens.

Test these in staging first.

**Provider identity on Antalya (`users.d/provider-clickhouse.yaml`).** It covers users, roles and role membership, scoped to the `team-` prefix. Add the rows from [9.4](#94-minimal-grants-for-the-provider-identity) for Grant and Database as needed.

```yaml
users:
  "system:serviceaccount:crossplane-system:provider-clickhouse":
    jwt:
      processor: kubernetes      # pins the processor (Antalya; key not in upstream docs)
    networks: {ip: ["10.0.0.0/8"]}
    profile: default
    grants:
      query:
        - "GRANT CREATE USER, ALTER USER, DROP USER ON `team-`*"
        - "GRANT CREATE USER, ALTER USER, DROP USER ON `system:serviceaccount:team-`*"
        - "GRANT CREATE ROLE, DROP ROLE ON `team-`*"
        - "GRANT SHOW USERS, SHOW ROLES ON *.*"
        - "GRANT analytics_reader TO `system:serviceaccount:crossplane-system:provider-clickhouse` WITH ADMIN OPTION"
        - "GRANT ROLE ADMIN ON *.*"   # only if RoleGrant must grant roles the provider creates
        - "GRANT SELECT ON system.role_grants"
        - "GRANT SELECT ON system.user_directories"
        - "GRANT SELECT ON system.clusters"
```

Whether a users.d `grants:` entry can grant a role to the user it defines is [T]. The fallback is an administrator running the `GRANT ... WITH ADMIN OPTION` once in SQL.

**Provider identity on upstream with a password (`users.d/provider-clickhouse.yaml`).** This is the basis of `examples/quickstart-upstream-password/`. Generate the hash with `printf '%s' "$PASSWORD" | sha256sum`.

```yaml
users:
  provider-clickhouse:
    password_sha256_hex: "<sha256 hex of the password>"
    networks: {ip: ["10.0.0.0/8"]}
    profile: default
    grants:
      query:
        - "GRANT CREATE USER, ALTER USER, DROP USER ON `app_`*"
        - "GRANT CREATE ROLE, DROP ROLE ON `app_`*"
        - "GRANT SHOW USERS, SHOW ROLES ON *.*"
        - "GRANT ROLE ADMIN ON *.*"   # RoleGrant of roles the provider creates
        - "GRANT SELECT ON system.user_directories"
        - "GRANT SELECT ON system.role_grants"
        - "GRANT SELECT ON system.grants"
        - "GRANT SELECT ON system.settings_profile_elements"
        - "GRANT CREATE DATABASE, DROP DATABASE, SHOW TABLES ON app_*.*"
        - "GRANT ALTER MODIFY DATABASE COMMENT ON app_*.*"   # omit on 25.3
        - "GRANT SELECT, INSERT, SHOW TABLES ON app_*.* WITH GRANT OPTION"
```

**Matching provider configurations** (TLS on; `audience` equals the projected volume's audience):

```yaml
spec:
  tls: {enabled: true, caCertSecretRef: {namespace: crossplane-system, name: clickhouse-ca, key: ca.crt}}
  credentials:
    source: TokenFile
    tokenFile: {path: /var/run/secrets/clickhouse/token, audience: clickhouse}
```

**DeploymentRuntimeConfig for a projected token** (`examples/runtime/projected-token.yaml`):

```yaml
apiVersion: pkg.crossplane.io/v1beta1
kind: DeploymentRuntimeConfig
metadata: {name: provider-clickhouse}
spec:
  serviceAccountTemplate:
    metadata: {name: provider-clickhouse}
  deploymentTemplate:
    spec:
      selector: {}
      template:
        spec:
          containers:
            - name: package-runtime
              volumeMounts:
                - {name: clickhouse-token, mountPath: /var/run/secrets/clickhouse, readOnly: true}
          volumes:
            - name: clickhouse-token
              projected:
                sources:
                  - serviceAccountToken: {audience: clickhouse, expirationSeconds: 3600, path: token}
```

**DeploymentRuntimeConfig for GKE Workload Identity** (`examples/runtime/gke-workload-identity.yaml`). The Kubernetes service account must be allowed to impersonate the Google service account (`roles/iam.workloadIdentityUser`).

```yaml
apiVersion: pkg.crossplane.io/v1beta1
kind: DeploymentRuntimeConfig
metadata: {name: provider-clickhouse-gke}
spec:
  serviceAccountTemplate:
    metadata:
      name: provider-clickhouse
      annotations:
        iam.gke.io/gcp-service-account: provider-clickhouse@my-project.iam.gserviceaccount.com
```

**Replicated user directory** (multi-host clusters, `config.d/user_directories.xml`; needs a restart):

```xml
<clickhouse>
  <user_directories replace="replace">
    <users_xml><path>/etc/clickhouse-server/users.xml</path></users_xml>
    <replicated><zookeeper_path>/clickhouse/access/my-cluster</zookeeper_path></replicated>
  </user_directories>
</clickhouse>
```

Keep `users_xml` first. If the order is changed, an SQL entity could shadow the break-glass or provider identity.
