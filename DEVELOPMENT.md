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

## Running the provider locally

```sh
make run   # kubectl apply -f package/crds && go run ./cmd/provider --debug
```

This uses the current kube context. Use a disposable local cluster (kind, or
the one built into your Docker engine), never a shared one.

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
its image); and gitleaks.

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
