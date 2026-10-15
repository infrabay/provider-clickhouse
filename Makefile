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
# Crossplane for make e2e: the latest release (crossplane-stable chart) and the
# oldest release that package/crossplane.yaml allows (make e2e-oldest).
# renovate: datasource=helm depName=crossplane registryUrl=https://charts.crossplane.io/stable
CROSSPLANE_VERSION ?= 2.4.2
CROSSPLANE_MIN_VERSION ?= 2.0.0
# kind for make e2e-kind-up. The node image is one that the kind release
# lists in its release notes; Renovate does not track it (the digest belongs
# to the kind release), so a kind update must also update KIND_NODE_IMAGE
# from the new release notes.
# renovate: datasource=github-releases depName=kubernetes-sigs/kind
KIND_VERSION ?= v0.33.0
KIND_NODE_IMAGE ?= kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
# kubectl and Helm for make e2e in CI (make e2e-tools puts them into bin/);
# locally the ones on PATH are used unless bin/ comes first.
# renovate: datasource=github-releases depName=kubernetes/kubernetes
KUBECTL_VERSION ?= v1.37.1
# renovate: datasource=github-releases depName=helm/helm
HELM_VERSION ?= v4.3.0

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
KIND ?= $(GO) run sigs.k8s.io/kind@$(KIND_VERSION)
# The kind cluster of make e2e-kind-up and its kubeconfig (a separate file:
# ~/.kube/config is never touched).
E2E_KIND_CLUSTER ?= xpch-e2e
E2E_KIND_KUBECONFIG ?= $(OUTPUT)/e2e/kind.kubeconfig
# The cluster's own Docker network (not kind's shared "kind" network), which
# make e2e-kind-down removes.
E2E_KIND_NETWORK ?= $(E2E_KIND_CLUSTER)
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

.PHONY: e2e
e2e: $(CROSSPLANE) ## End-to-end tests: install the package into the local cluster E2E_KUBE_CONTEXT (required) and run test/e2e.
	CROSSPLANE_VERSION=$(CROSSPLANE_VERSION) CROSSPLANE=$(CROSSPLANE) GO=$(GO) MAKE=$(MAKE) hack/e2e/run.sh

.PHONY: e2e-oldest
e2e-oldest: ## make e2e with the oldest Crossplane release that package/crossplane.yaml allows.
	$(MAKE) e2e CROSSPLANE_VERSION=$(CROSSPLANE_MIN_VERSION)

.PHONY: e2e-cleanup
e2e-cleanup: ## Remove everything make e2e installed from E2E_KUBE_CONTEXT.
	hack/e2e/cleanup.sh

.PHONY: e2e-tools
e2e-tools: $(BIN)/kubectl $(BIN)/helm ## Fetch the pinned kubectl and Helm into bin/ (sha256-verified), for make e2e in CI.

$(BIN)/kubectl:
	@mkdir -p $(BIN)
	curl -fsSL -o $@.download https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/$(HOST_OS)/$(HOST_ARCH)/kubectl
	curl -fsSL -o $@.sha256 https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/$(HOST_OS)/$(HOST_ARCH)/kubectl.sha256
	@echo "$$(cut -d' ' -f1 $@.sha256)  $@.download" | shasum -a 256 -c -
	chmod +x $@.download && mv $@.download $@

$(BIN)/helm:
	@mkdir -p $(BIN)/helm-download
	curl -fsSL -o $(BIN)/helm-download/helm.tar.gz https://get.helm.sh/helm-$(HELM_VERSION)-$(HOST_OS)-$(HOST_ARCH).tar.gz
	curl -fsSL -o $(BIN)/helm-download/helm.tar.gz.sha256sum https://get.helm.sh/helm-$(HELM_VERSION)-$(HOST_OS)-$(HOST_ARCH).tar.gz.sha256sum
	@echo "$$(cut -d' ' -f1 $(BIN)/helm-download/helm.tar.gz.sha256sum)  $(BIN)/helm-download/helm.tar.gz" | shasum -a 256 -c -
	tar -xzf $(BIN)/helm-download/helm.tar.gz -C $(BIN)/helm-download $(HOST_OS)-$(HOST_ARCH)/helm
	mv $(BIN)/helm-download/$(HOST_OS)-$(HOST_ARCH)/helm $@
	rm -rf $(BIN)/helm-download

.PHONY: e2e-kind-up
e2e-kind-up: ## Create the kind cluster E2E_KIND_CLUSTER for make e2e (context kind-<name>, kubeconfig E2E_KIND_KUBECONFIG).
	@case "$(E2E_KIND_CLUSTER)" in xpch-e2e*) ;; *) echo "E2E_KIND_CLUSTER must start with xpch-e2e"; exit 1 ;; esac
	@mkdir -p $(dir $(E2E_KIND_KUBECONFIG))
	KIND_EXPERIMENTAL_DOCKER_NETWORK=$(E2E_KIND_NETWORK) $(KIND) create cluster --name $(E2E_KIND_CLUSTER) --image $(KIND_NODE_IMAGE) --kubeconfig $(E2E_KIND_KUBECONFIG) --wait 5m

.PHONY: e2e-kind-down
e2e-kind-down: ## Delete the kind cluster E2E_KIND_CLUSTER and its Docker network.
	@case "$(E2E_KIND_CLUSTER)" in xpch-e2e*) ;; *) echo "E2E_KIND_CLUSTER must start with xpch-e2e"; exit 1 ;; esac
	$(KIND) delete cluster --name $(E2E_KIND_CLUSTER) --kubeconfig $(E2E_KIND_KUBECONFIG)
	@if docker network inspect $(E2E_KIND_NETWORK) >/dev/null 2>&1; then docker network rm $(E2E_KIND_NETWORK) >/dev/null; fi

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

# The local cluster of make run (required; never the current context).
KUBE_CONTEXT ?=

.PHONY: run
run: ## Install the CRDs into the local cluster KUBE_CONTEXT (required, same allow-list as make e2e) and run the provider out of cluster against it.
	@[ -n "$(KUBE_CONTEXT)" ] || { echo "KUBE_CONTEXT is not set: make run KUBE_CONTEXT=<local cluster context> (the current context is never used)"; exit 1; }
	@E2E_KUBE_CONTEXT="$(KUBE_CONTEXT)" E2E_KUBECONFIG= bash -c 'source hack/e2e/lib.sh && e2e::guard'
	@mkdir -p $(OUTPUT)
	@# A kubeconfig with only that context, so that neither kubectl nor the
	@# provider can use another one.
	@(umask 077 && kubectl config view --minify --flatten --context "$(KUBE_CONTEXT)" >$(OUTPUT)/run.kubeconfig)
	kubectl --kubeconfig $(OUTPUT)/run.kubeconfig apply -f package/crds
	KUBECONFIG=$(OUTPUT)/run.kubeconfig $(GO) run ./cmd/provider --debug

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
