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
apis/unreleased-crds/          generated CRDs of kinds without a controller (not packaged)
cmd/provider/                  the provider binary
internal/clients/clickhouse/   connection options, client cache, errors, quoting, capabilities
  auth/                        token sources and the checks before a token is sent
  sqlx/                        SQL builders
  fake/                        fakes for unit tests
internal/controller/           controllers: config, database; providerconfig (resolution
                               and policy) and ownership (claims and the decision table)
package/                       crossplane.yaml and the generated CRDs of kinds with a controller
examples/                      example manifests, validated in CI
test/envtest/                  CRD validation against a real kube-apiserver
test/integration/              tests against ClickHouse containers
cluster/images/                the runtime image
docs/design.md                 the design
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

Only kinds with a controller are packaged. `make generate` moves the CRDs of
the other kinds from `package/crds` to `apis/unreleased-crds` (the list is in
`apis/generate.go`); envtest still installs them to check their CEL rules.
When a controller is added, register it in `internal/controller/register.go`,
add its kind to `ManagedKinds` there and remove its CRD from the list in
`apis/generate.go`; a unit test fails if the two disagree.

## Integration tests

`make test-integration` runs `go test -tags integration ./test/integration/...`
with testcontainers. The tests start:

- upstream ClickHouse with a password provider identity defined in `users.d`,
  holding only the prefix-scoped grants that `Database` needs;
- Altinity Antalya with a `jwt_static_key` HS256 token processor and a JWT
  provider identity. Keys are random per run and written to a temporary
  directory; tokens are minted in the test with `golang-jwt` and written to a
  token file that the provider reads (`TokenFile`). The test covers the native
  protocol, the HTTP transport over plain HTTP and HTTPS (a throwaway CA and
  certificate are generated per run), token rotation, the audience check and
  the retry on error 516;
- a two-replica upstream cluster (interserver secret, embedded Keeper on the
  first node, on a temporary Docker network) for `ON CLUSTER` DDL, Replicated
  databases, the per-host ownership check and the Keeper path policy. It uses
  the first image of `XPCH_UPSTREAM_IMAGES`;
- the provider's real controllers against envtest and an upstream container
  (`TestProviderEndToEnd`), including an observe-only resource.

Choose the images with comma-separated lists, or `none` to skip a family:

```sh
XPCH_UPSTREAM_IMAGES=clickhouse/clickhouse-server:25.3,clickhouse/clickhouse-server:26.8.23.6 \
XPCH_ANTALYA_IMAGES=none \
  make test-integration
```

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
26.3 and 26.6); and gitleaks.

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
