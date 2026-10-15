# Developing provider-clickhouse

This page is for contributors. To use the provider, install the package as the
[README](README.md#quick-start) shows.

## Prerequisites

- Go 1.26 or newer. The `go` directive in `go.mod` is `1.26.0`; its
  `toolchain` line is what CI and release builds use, and what a local `go`
  selects automatically.
- Docker (any engine that testcontainers can reach, for example Docker
  Desktop, OrbStack or colima) for the integration tests and image builds.
- Optional: `golangci-lint` (`make lint` falls back to `go run` at the pinned
  version) and `kubectl` with a disposable local cluster for `make run`.

## Layout

```
apis/v1alpha1/                 API types; zz_generated.* files are generated
cmd/provider/                  the provider binary
internal/clients/clickhouse/   connection options, client cache, errors, quoting, capabilities
  auth/                        token sources and the checks before a token is sent
  sqlx/                        SQL builders, the SQL lexer, the SHOW CREATE USER parser,
                               the privilege catalogue and grant model; testdata/ holds
                               recorded server output per version
  fake/                        fakes for unit tests
internal/controller/           controllers: config, database, user, role, grant, rolegrant;
                               access (shared by the access entity kinds), providerconfig
                               (resolution and policy), ownership (claims, decision table,
                               create journal)
package/                       crossplane.yaml and the generated CRDs
examples/                      example manifests, validated in CI
test/envtest/                  CRD validation against a real kube-apiserver
test/integration/              tests against ClickHouse containers
test/e2e/                      end-to-end tests of the installed package (make e2e)
hack/e2e/                      the end-to-end harness: setup, cleanup, diagnostics, manifests
cluster/images/                the runtime image
docs/design.md                 the design
docs/operations.md             the operations runbook
```

## Everyday commands

```sh
make generate          # deepcopy, CRDs, method sets, resolvers (go tool controller-gen / angryjet)
make build             # go build ./... and bin/provider
make test              # unit tests: go test -race ./... (no network, no Docker)
make lint              # golangci-lint
make envtest           # CRD CEL rules and examples against a real kube-apiserver
make validate          # crossplane resource validate package/crds examples
make test-integration  # ClickHouse containers (Docker)
make reviewable        # generate + lint + test + envtest, and fail on uncommitted generated changes
make e2e E2E_KUBE_CONTEXT=<local context>  # the real package on a local cluster (see below)
make help              # every target
```

The code generators are `tool` directives in `go.mod`, so their versions move
with the module. `make generate` must be run after any change in `apis/`, and
its output is committed; CI fails when it is out of date.

Only kinds with a controller may be packaged. `make generate` writes every CRD
into `package/crds`, and a unit test fails when `package/crds` and
`ManagedKinds` in `internal/controller/register.go` disagree. When a kind is
added, register its controller in `SetupGated` and add it to `ManagedKinds`
in the same change.

## Integration tests

`make test-integration` runs `go test -tags integration ./test/integration/...`
with testcontainers. The tests start:

- upstream ClickHouse with a password provider identity defined in `users.d`,
  holding only the prefix-scoped grants that `Database` needs, and for the
  access entity kinds (`TestUser`, `TestRole`, `TestRoleGrant`, `TestGrant`,
  `TestAccess*`) an identity with the prefix-scoped grants of every kind
  (design 9.4). Antalya runs the same tests with a JWT identity and two token
  processors; JWT users are tested there and reported `Unsupported` upstream;
- Altinity Antalya with a `jwt_static_key` HS256 token processor and a JWT
  provider identity. Keys are random per run and written to a temporary
  directory; tokens are minted in the test with `golang-jwt` and written to a
  token file that the provider reads (`TokenFile`). The test covers the native
  protocol, the HTTP transport over plain HTTP and HTTPS (a throwaway CA and
  certificate are generated per run), token rotation, the audience check and
  the retry on error 516;
- every image again with a provider identity defined in SQL that holds
  exactly the grants of every kind (`TestMinimalGrants`, design 10.2
  scenario 5): an administrator revokes them one at a time, every resource
  then fails with `PermissionDenied` (or `DependencyMissing` when a
  dependency could not be created) and is never reported as absent, and
  everything converges once the grant is back;
- a two-replica upstream cluster (interserver secret, embedded Keeper on the
  first node, on a temporary Docker network) for `ON CLUSTER` DDL, Replicated
  databases, the per-host ownership check and the Keeper path policy. It uses
  the first image of `XPCH_UPSTREAM_IMAGES`;
- the provider's real controllers against envtest and an upstream container
  (`TestProviderEndToEnd`), including an observe-only resource, and against
  every selected image (`TestProviderEndToEndAccess`): a Database, Role,
  User (password upstream, JWT on Antalya), Grant and RoleGrant; the user
  logs in and can do exactly what was granted, foreign users and roles stay
  byte-identical, and deleting the resources revokes and drops only what the
  provider created. The end-to-end tests need `KUBEBUILDER_ASSETS`, which
  `make test-integration` sets.

Choose the images with comma-separated lists, or `none` to skip a family:

```sh
XPCH_UPSTREAM_IMAGES=clickhouse/clickhouse-server:25.3.3.42-alpine,clickhouse/clickhouse-server:26.8.23.6 \
XPCH_ANTALYA_IMAGES=none \
  make test-integration
```

One image takes several minutes; CI runs one job per image. For a run over
all five images raise the timeout, for example
`go test -tags integration -count=1 -timeout 90m ./test/integration/...` with
`KUBEBUILDER_ASSETS` and `TESTCONTAINERS_RYUK_DISABLED=true` set as the
Makefile does.

### Recorded server output

Some unit tests use output recorded from every image of the CI matrix. The
integration tests compare it with the servers under test and fail on any
difference (or skip for a version without a recording); re-record it when an
image of the matrix changes:

- `SHOW CREATE USER` fixtures of the parser,
  `internal/clients/clickhouse/sqlx/testdata/user/<server version>.txt`:

  ```sh
  XPCH_UPDATE_FIXTURES=1 go test -tags integration -run 'TestUser$/.*/.*/ShowCreateUserFixtures' ./test/integration/
  ```

- the privilege catalogues of the grant model,
  `internal/clients/clickhouse/sqlx/testdata/privileges/<server version>.tsv`:

  ```sh
  XPCH_RECORD_PRIVILEGES=1 go test -tags integration -run TestGrantPrivilegeCatalogue ./test/integration/
  ```

Both read the images from `XPCH_UPSTREAM_IMAGES` and `XPCH_ANTALYA_IMAGES`.

### Container naming rule

Every container a test starts is named `xpch-it-<test>-<random>` and removed
when the test ends, also when it fails. Testcontainers' reaper container is
disabled (`TESTCONTAINERS_RYUK_DISABLED=true`, set by the Makefile and by the
test binary) because its name does not follow the rule. If a run is killed,
remove leftovers with:

```sh
docker ps -a --filter name=xpch- --format '{{.Names}}' | xargs -r docker rm -f
```

Use the same `xpch-` prefix for any container you start by hand while
developing (for example `docker run --name xpch-dev-clickhouse ...`).

## End-to-end tests

`make e2e` installs the real package into a local Kubernetes cluster and runs
`test/e2e` (build tag `e2e`) against it. `hack/e2e/run.sh` does the setup:

1. builds the runtime image and the `.xpkg` for the node architecture
   (`make xpkg PLATFORMS=linux/<arch>`);
2. starts a throwaway registry inside the cluster (namespace
   `xpch-e2e-registry`) and pushes the package there with the pinned Crossplane
   CLI, through a port-forward. The Crossplane package manager pulls it
   through the registry's ClusterIP (go-containerregistry speaks plain HTTP to
   RFC 1918 addresses). The provider's runtime image is the same package; the
   node's container runtime pulls it from `localhost:35000`, because the
   registry runs on the host network and container runtimes speak plain HTTP
   to localhost registries without configuration. The
   `DeploymentRuntimeConfig` sets that image. No public registry is involved;
3. installs Crossplane (`CROSSPLANE_VERSION`, crossplane-stable chart) with
   `provider.defaultActivations=[]`, so no activation policy exists;
4. starts upstream ClickHouse 25.3.3.42 and Altinity Antalya 26.6.4 (namespace
   `xpch-e2e-clickhouse`). Upstream has password identities for the provider
   and for a tenant; Antalya has JWT identities for the provider's service
   account and a tenant's service account. They are validated by a
   `jwt_static_jwks` token processor that holds the cluster's service account
   keys, read from the API server at setup. Every identity holds exactly the
   grants of the README quick start, read from `README.md` at run time (on
   Antalya plus user DDL for `system:serviceaccount:xpch-e2e-*` users). The
   administrators are only used by the tests' assertions;
5. installs the package with `examples/provider.yaml` and
   `examples/runtime/projected-token.yaml` (the projected token for the
   `TokenFile` identity), with `--poll=10s` and `--debug` added, and waits
   until it is healthy.

The tests then activate the managed resource kinds (one kind first, then
`examples/mrap.yaml`; Crossplane 2.0.0 and 2.0.1 activate the ProviderConfig
kinds the same way, after the provider has run for longer than
controller-runtime's 2-minute cache sync timeout without them) and run,
against both servers: the lifecycle of every kind through a
`ClusterProviderConfig` with a closed policy and a namespaced `ProviderConfig`,
each MR running as the identity of its own config (`system.query_log`); a
login with the connection Secret (with a service account token on Antalya)
that can do exactly what was granted; out-of-band drift (`REVOKE`, `ALTER
USER`, comments) repaired within the poll interval, and a second login with
the Secret afterwards; foreign objects with the same names refused and
byte-identical; policy violations (database, user and role names, namespace,
authentication method, `grantableRoles`); observe-only resources that never
write; a provider pod deleted while 40 roles per server are being created (the
test checks that the old pod was still creating after its deletion and left
roles to the new pod); and deletion, which drops and revokes only what the
provider created.

```sh
make e2e E2E_KUBE_CONTEXT=orbstack          # latest Crossplane (CROSSPLANE_VERSION)
make e2e-oldest E2E_KUBE_CONTEXT=orbstack   # oldest allowed (CROSSPLANE_MIN_VERSION)

make e2e-kind-up                            # kind cluster xpch-e2e (own Docker network), kubeconfig _output/e2e/kind.kubeconfig
make e2e E2E_KUBE_CONTEXT=kind-xpch-e2e E2E_KUBECONFIG=$PWD/_output/e2e/kind.kubeconfig
make e2e-kind-down                          # deletes the cluster and its network

make e2e-cleanup E2E_KUBE_CONTEXT=orbstack  # after E2E_KEEP=1 or an interrupted run
```

`E2E_KUBE_CONTEXT` is required, and must be a local cluster context
(`orbstack`, `docker-desktop`, `minikube`, `rancher-desktop`, `kind-*`,
`k3d-*`; letters, digits and `._@:-` only) whose API server is on this
machine or a private network (loopback or RFC 1918 addresses, compared as
numbers, `::1`, IPv6 unique local addresses, `localhost`,
`host.docker.internal`, `kubernetes.docker.internal`, `*.orb.local`), and whose
nodes have no cloud provider ID (`gce://`, `aws://`, `azure://` and others) or
GKE, EKS or AKS node labels. A private address alone does not make a cluster
local; the name allow-list is the main protection. The harness and the tests
never use or change the kubeconfig's current context; every `kubectl` and
`helm` call names the context. The run needs Docker (for the image build),
`kubectl`, `helm`, `openssl` and `curl` (`make e2e-tools` puts the pinned
kubectl and Helm into `bin/`; CI uses those), a single-architecture cluster,
and about 4 GB of memory for the two servers.

The cluster must not have a Crossplane that the harness did not install:
`make e2e` refuses to start if a Crossplane Deployment runs in any namespace,
or if `crossplane-system` is absent and any `*.crossplane.io` CRD or any
ClusterRole, ClusterRoleBinding or webhook configuration named `crossplane*`
exists (for example left behind by an uninstalled Crossplane).

| Variable | Default | |
|---|---|---|
| `E2E_KUBECONFIG` | the default kubeconfig | a separate kubeconfig, for example kind's |
| `CROSSPLANE_VERSION` | the Makefile pin | the Crossplane chart version |
| `CROSSPLANE_CHART_REPO` | `https://charts.crossplane.io/stable` | the Helm repository of the Crossplane chart |
| `E2E_KEEP` | `0` | `1` leaves everything installed |
| `E2E_WORKDIR` | `_output/e2e` | generated files, secrets (mode 0600) and `artifacts/` |
| `E2E_POLL_INTERVAL` | `10s` | the provider's `--poll` |
| `E2E_TEST_TIMEOUT` | `25m` | `go test -timeout` |
| `E2E_GO_TEST_ARGS` | | more `go test` flags, for example `-run` |
| `E2E_UPSTREAM_IMAGE`, `E2E_ANTALYA_IMAGE` | 25.3.3.42-alpine, 26.6.4 Antalya | the servers |
| `E2E_REGISTRY_IMAGE` | `registry:3.1.2` (by digest) | the in-cluster registry |
| `E2E_REGISTRY_PORT` | `35000` | the registry's port on the node |
| `E2E_KIND_CLUSTER`, `E2E_KIND_KUBECONFIG` | `xpch-e2e`, `_output/e2e/kind.kubeconfig` | the cluster of `make e2e-kind-up` and `make e2e-kind-down` |

On failure the run writes diagnostics to `$E2E_WORKDIR/artifacts`: the
Provider and its revisions, activation policies and definitions, the
provider's and Crossplane's logs, every managed resource and ProviderConfig
with its conditions, events, pods, the servers' error logs and the test
output. Secrets are listed by name, type and keys only, and every secret
value the harness generated is replaced with `[REDACTED]`.

The run also writes diagnostics when it is interrupted (Ctrl-C, SIGTERM, a
cancelled CI job). Afterwards (unless `E2E_KEEP=1`) `make e2e-cleanup` removes
everything: the resources of the test namespaces, the Provider, the
activation policies the tests created, Crossplane with its CRDs, ClusterRoles,
ClusterRoleBindings and webhook configurations, the provider's CRDs, every
namespace labelled `app.kubernetes.io/part-of=xpch-e2e` (`crossplane-system`
last), and events about the removed objects in `default`. It treats
Crossplane as its own only while `crossplane-system` carries that label and
no Crossplane Deployment runs in another namespace; otherwise it leaves
Crossplane, its CRDs and RBAC and the provider's CRDs alone. A
`crossplane-system` without the label stops it. It fails if anything is left.
On OrbStack it also removes the runtime images the node pulled from the
registry; the third-party images the node pulled (Crossplane, both ClickHouse
servers, the registry, several GB) stay in the Docker engine for later runs,
and can be removed with `docker image rm`.

Known limits of the end-to-end tests:

- Database spec and comment changes are only exercised on Antalya: 25.3
  cannot change a database comment.
- The provider restart scenario cannot force SIGTERM to land between a
  `CREATE` and the write of its claim; it creates 40 roles per server at once
  and fails if the shutdown did not interrupt the creates. With the graceful
  shutdown fix reverted it failed in 2 of 4 runs. The deterministic regression
  test of the graceful shutdown is `TestGracefulShutdownDuringCreate`
  (integration).
- The registry listens on port 35000 of the node's network (host network)
  and accepts pushes without authentication. On OrbStack the node shares the
  network of the Docker VM, and OrbStack forwards that port on all of the
  host's interfaces while the run lasts (Docker Desktop may do the same). The
  Provider and the runtime image reference the package by the digest of the
  push, so a push by someone else cannot change what the cluster runs. If the
  port is taken, the registry does not become ready and the run stops after 5
  minutes. Set `E2E_REGISTRY_PORT` to move it.

## Running the provider locally

```sh
make run KUBE_CONTEXT=orbstack   # kubectl apply -f package/crds && go run ./cmd/provider --debug
```

`KUBE_CONTEXT` is required and goes through the same checks as
`E2E_KUBE_CONTEXT` (a local cluster only); the current context is never used.
`make run` writes a kubeconfig with only that context to
`_output/run.kubeconfig` and runs `kubectl` and the provider with it. Set
`KUBECONFIG` to use a separate kubeconfig, for example kind's.

## Images and packages

```sh
make image                     # bin/linux_<arch>/provider + _output/image-<arch>.tar
make xpkg                      # _output/provider-<arch>.xpkg (runtime image embedded)
make xpkg PLATFORMS=linux/arm64
```

`make crossplane` downloads the pinned Crossplane CLI into `bin/` and checks
its sha256. The Dockerfile only copies the cross-compiled binary into
`gcr.io/distroless/static:nonroot`, so cross-architecture builds need no
emulation.

## Continuous integration

`.github/workflows/test.yml` runs on pull requests and pushes to `main`:
gofmt, vet, build, the generated-files check, unit tests and a guard against
workstation paths; envtest and `crossplane resource validate`; golangci-lint;
govulncheck; the integration matrix (upstream 25.3, 25.8 and 26.8, Antalya
26.3 and 26.6; each job runs every integration and end-to-end test against
its image); the end-to-end tests on kind with the oldest Crossplane release
that `package/crossplane.yaml` allows and with the latest one
(`make e2e-oldest` and `make e2e`, with the kubectl and Helm of
`make e2e-tools`; diagnostics are uploaded when the job fails or is
cancelled); and gitleaks.

## Releasing

Push a semantic-version tag (`vX.Y.Z`). `.github/workflows/release.yml` runs
the test workflow against the tag, builds the image and `.xpkg` for amd64 and
arm64, pushes the multi-platform package to
`ghcr.io/infrabay/provider-clickhouse:<tag>`, signs it with cosign (keyless),
attests its build provenance, generates an SBOM and creates the GitHub release
with the packages attached. The publish job runs in the `release`
environment.

One-time step: GHCR creates the package as private on its first push. Make it
public once in the package settings (and link it to the repository) so that
Crossplane can pull it without credentials.
