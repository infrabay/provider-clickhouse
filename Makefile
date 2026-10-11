# provider-clickhouse

# Tool versions, kept current by Renovate.
# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.14.0
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION ?= v1.8.0
# renovate: datasource=github-releases depName=crossplane/cli
CROSSPLANE_CLI_VERSION ?= v2.5.0
# setup-envtest branch matching controller-runtime, and the kube-apiserver line.
SETUP_ENVTEST_VERSION ?= release-0.23
ENVTEST_K8S_VERSION ?= 1.35.x

GO ?= go
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo v0.0.0-dev)
PLATFORMS ?= linux/amd64 linux/arm64
XPKG_REPO ?= ghcr.io/infrabay/provider-clickhouse
IMAGE_SOURCE ?= https://github.com/infrabay/provider-clickhouse
LDFLAGS := -s -w -X github.com/infrabay/provider-clickhouse/internal/version.Version=$(VERSION)

BIN := $(CURDIR)/bin
OUTPUT := $(CURDIR)/_output
# Prints the path of the envtest binaries (kube-apiserver, etcd), downloading
# them into bin/envtest on first use.
SETUP_ENVTEST = $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN)/envtest -p path
CROSSPLANE ?= $(BIN)/crossplane
HOST_OS := $(shell $(GO) env GOOS)
HOST_ARCH := $(shell $(GO) env GOARCH)

.DEFAULT_GOAL := build

.PHONY: generate
generate: ## Generate deepcopy, CRDs (package/crds), method sets and resolvers.
	$(GO) generate -tags generate ./apis/...

.PHONY: build
build: ## Build every package and the provider binary.
	$(GO) build ./...
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/provider ./cmd/provider

.PHONY: test
test: ## Unit tests (no network, no Docker).
	$(GO) test -race -count=1 ./...

.PHONY: test-integration integration
test-integration: ## Integration and end-to-end tests against ClickHouse containers (Docker) and envtest.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST))" TESTCONTAINERS_RYUK_DISABLED=true \
		$(GO) test -tags integration -count=1 -timeout 20m ./test/integration/...
integration: test-integration

.PHONY: envtest
envtest: ## CRD validation (CEL) and examples dry-run against a real kube-apiserver.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST))" $(GO) test -tags envtest -count=1 ./test/envtest/...

.PHONY: validate
validate: $(CROSSPLANE) ## Validate examples against the CRDs with the Crossplane CLI.
	$(CROSSPLANE) resource validate package/crds examples

.PHONY: lint
lint: ## golangci-lint (falls back to go run at the pinned version).
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...; fi

.PHONY: vuln
vuln: ## govulncheck.
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: reviewable
reviewable: generate lint test envtest ## Everything a pull request needs; fails if generate changed files.
	@git diff --exit-code -- apis package docs || (echo "generated files are out of date: run make generate"; exit 1)

.PHONY: run
run: ## Install the CRDs into the current kube context and run the provider out of cluster.
	kubectl apply -f package/crds
	$(GO) run ./cmd/provider --debug

$(CROSSPLANE):
	@mkdir -p $(BIN)
	curl -fsSL -o $(CROSSPLANE) https://cli.crossplane.io/stable/$(CROSSPLANE_CLI_VERSION)/bin/$(HOST_OS)_$(HOST_ARCH)/crossplane
	curl -fsSL -o $(CROSSPLANE).sha256 https://cli.crossplane.io/stable/$(CROSSPLANE_CLI_VERSION)/bin/$(HOST_OS)_$(HOST_ARCH)/crossplane.sha256
	@echo "$$(cut -d' ' -f1 $(CROSSPLANE).sha256)  $(CROSSPLANE)" | shasum -a 256 -c -
	chmod +x $(CROSSPLANE)

.PHONY: crossplane
crossplane: $(CROSSPLANE) ## Fetch the pinned Crossplane CLI into bin/ (sha256-verified).

.PHONY: image
image: ## Build the runtime image tarball for each platform into _output/.
	@mkdir -p $(OUTPUT)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "==> $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/$${os}_$${arch}/provider ./cmd/provider || exit 1; \
		docker buildx build --platform $$os/$$arch -f cluster/images/provider-clickhouse/Dockerfile \
			--output type=docker,dest=$(OUTPUT)/image-$$arch.tar . || exit 1; \
	done

.PHONY: xpkg
xpkg: $(CROSSPLANE) image ## Build a Crossplane package (.xpkg) per platform with the runtime image embedded.
	@for p in $(PLATFORMS); do \
		arch=$${p#*/}; \
		$(CROSSPLANE) xpkg build --package-root=package --examples-root=examples \
			--embed-runtime-image-tarball=$(OUTPUT)/image-$$arch.tar -o $(OUTPUT)/provider-$$arch.xpkg || exit 1; \
	done

.PHONY: xpkg-push
xpkg-push: $(CROSSPLANE) ## Push the multi-arch package to $(XPKG_REPO):$(VERSION) (CI; VERSION must be semver).
	$(CROSSPLANE) xpkg push -a org.opencontainers.image.source=$(IMAGE_SOURCE) \
		-f $(shell echo $(foreach p,$(PLATFORMS),$(OUTPUT)/provider-$(lastword $(subst /, ,$(p))).xpkg) | tr ' ' ',') \
		$(XPKG_REPO):$(VERSION)

.PHONY: clean
clean: ## Remove build output.
	@chmod -R u+w $(BIN) 2>/dev/null || true
	rm -rf $(BIN) $(OUTPUT)

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'
