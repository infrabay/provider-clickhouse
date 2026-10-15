#!/usr/bin/env bash
# Copyright 2026 The provider-clickhouse Authors
# SPDX-License-Identifier: Apache-2.0
#
# make e2e: installs the real provider package into a local cluster and runs
# test/e2e against it (design 10.4, DEVELOPMENT.md "End-to-end tests"):
#
#  1. builds the runtime image and the .xpkg for the node architecture
#     (make xpkg), and the test-only fakegcp image (make fakegcp-image);
#  2. starts a throwaway registry inside the cluster and pushes the package
#     there with the pinned Crossplane CLI, and the fakegcp image with the
#     pinned crane;
#  3. installs Crossplane (crossplane-stable chart, CROSSPLANE_VERSION) with no
#     default activation policy;
#  4. starts the fake GCE metadata server and Google JWKS
#     (test/fakegcp/deploy/fakegcp.yaml), upstream ClickHouse (password
#     identity) and Altinity Antalya (JWT identities: the provider's
#     Kubernetes service account, validated against the cluster's service
#     account JWKS, and the fake's Google service account, validated by a
#     production-style google_jwt processor against the fake's JWKS), all
#     provider identities with the minimal grants of the README;
#  5. installs the package (Provider and DeploymentRuntimeConfig from
#     examples/, plus GCE_METADATA_HOST pointing at the fake) and waits until
#     it is healthy;
#  6. runs go test -tags e2e ./test/e2e/..., which activates the managed
#     resource kinds and runs the scenarios.
#
# On failure, diagnostics (Secrets redacted) are written to
# $E2E_WORKDIR/artifacts. Everything is removed afterwards (cleanup.sh) unless
# E2E_KEEP=1.
set -euo pipefail

# shellcheck source=hack/e2e/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# Versions; the Makefile passes its pinned CROSSPLANE_VERSION.
CROSSPLANE_VERSION="${CROSSPLANE_VERSION:?CROSSPLANE_VERSION is not set (run make e2e)}"
CROSSPLANE_CHART_REPO="${CROSSPLANE_CHART_REPO:-https://charts.crossplane.io/stable}"
CROSSPLANE="${CROSSPLANE:-${E2E_ROOT}/bin/crossplane}"
CRANE="${CRANE:?CRANE is not set (run make e2e)}"
E2E_UPSTREAM_IMAGE="${E2E_UPSTREAM_IMAGE:-clickhouse/clickhouse-server:25.3.3.42-alpine}"
E2E_ANTALYA_IMAGE="${E2E_ANTALYA_IMAGE:-altinity/clickhouse-server:26.6.4.20001.altinityantalya}"
E2E_REGISTRY_IMAGE="${E2E_REGISTRY_IMAGE:-registry:3.1.2@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8}"
# The node-local port of the registry (host network; outside the NodePort
# range).
E2E_REGISTRY_PORT="${E2E_REGISTRY_PORT:-35000}"
# The provider's --poll interval; the drift scenarios wait for one interval
# plus a margin.
E2E_POLL_INTERVAL="${E2E_POLL_INTERVAL:-10s}"
E2E_TEST_TIMEOUT="${E2E_TEST_TIMEOUT:-25m}"
E2E_GO_TEST_ARGS="${E2E_GO_TEST_ARGS:-}"
E2E_KEEP="${E2E_KEEP:-0}"
# The google_jwt processor's jwks_cache_lifetime and token_cache_lifetime
# (seconds). Production uses 3600 and 300 (docs/testing-google-tokens.md);
# short values let the GoogleIDToken scenario rotate keys within seconds.
E2E_GOOGLE_CACHE_LIFETIME="${E2E_GOOGLE_CACHE_LIFETIME:-10}"
case "${E2E_GOOGLE_CACHE_LIFETIME}" in
'' | *[!0-9]* | 0) e2e::die "E2E_GOOGLE_CACHE_LIFETIME must be a positive number of seconds" ;;
esac
# The fake's manifest; its Service is the metadata server (GCE_METADATA_HOST)
# and the JWKS URI.
FAKEGCP_MANIFEST="${E2E_ROOT}/test/fakegcp/deploy/fakegcp.yaml"
FAKEGCP_HOST="fakegcp.${E2E_NS_FAKEGCP}.svc.cluster.local"
GO="${GO:-go}"
MAKE="${MAKE:-make}"

ARTIFACTS="${E2E_WORKDIR}/artifacts"
PF_PID=""

e2e::guard

on_exit() {
	local status=$?
	# A second Ctrl-C or SIGTERM stops the cleanup.
	trap - INT TERM
	if [ -n "${PF_PID}" ]; then kill "${PF_PID}" 2>/dev/null || true; fi
	if [ "${status}" -ne 0 ]; then
		e2e::warn "the end-to-end run failed (exit ${status}); writing diagnostics to ${ARTIFACTS}"
		"${E2E_DIR}/diagnostics.sh" "${ARTIFACTS}" || e2e::warn "diagnostics incomplete"
	fi
	if [ "${E2E_KEEP}" = "1" ]; then
		e2e::log "E2E_KEEP=1: leaving everything installed; remove it with make e2e-cleanup E2E_KUBE_CONTEXT=${E2E_KUBE_CONTEXT}"
	else
		"${E2E_DIR}/cleanup.sh" || {
			e2e::warn "cleanup failed; run make e2e-cleanup E2E_KUBE_CONTEXT=${E2E_KUBE_CONTEXT}"
			[ "${status}" -ne 0 ] || status=1
		}
	fi
	exit "${status}"
}

build_package() {
	local arches
	arches="$(e2e::kubectl get nodes -o 'jsonpath={range .items[*]}{.status.nodeInfo.architecture}{"\n"}{end}' | sort -u)"
	[ -n "${arches}" ] || e2e::die "cannot read the node architecture"
	[ "$(printf '%s\n' "${arches}" | wc -l | tr -d ' ')" = "1" ] || e2e::die "nodes of several architectures (${arches}) are not supported"
	ARCH="${arches}"
	TAG="e2e-$(date -u +%Y%m%d%H%M%S)"
	e2e::log "building the runtime image and the package for linux/${ARCH}"
	"${MAKE}" -C "${E2E_ROOT}" xpkg PLATFORMS="linux/${ARCH}"
	XPKG="${E2E_ROOT}/_output/provider-${ARCH}.xpkg"
	[ -s "${XPKG}" ] || e2e::die "${XPKG} was not built"
	e2e::log "building the test-only fakegcp image for linux/${ARCH}"
	"${MAKE}" -C "${E2E_ROOT}" fakegcp-image PLATFORMS="linux/${ARCH}"
	FAKEGCP_TAR="${E2E_ROOT}/_output/fakegcp-${ARCH}.tar"
	[ -s "${FAKEGCP_TAR}" ] || e2e::die "${FAKEGCP_TAR} was not built"
}

deploy_registry() {
	e2e::log "starting the in-cluster registry"
	e2e::ensure_namespace "${E2E_NS_REGISTRY}"
	sed -e "s|@@REGISTRY_IMAGE@@|${E2E_REGISTRY_IMAGE}|" -e "s|@@REGISTRY_PORT@@|${E2E_REGISTRY_PORT}|g" \
		"${E2E_DIR}/manifests/registry.yaml" | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl -n "${E2E_NS_REGISTRY}" rollout status deployment/registry --timeout=300s
	REGISTRY_IP="$(e2e::kubectl -n "${E2E_NS_REGISTRY}" get service registry -o 'jsonpath={.spec.clusterIP}')"
	case "${REGISTRY_IP}" in
	10.* | 192.168.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[0-1].*) ;;
	*) e2e::die "the registry's ClusterIP ${REGISTRY_IP} is not an RFC 1918 IPv4 address; the package manager would use HTTPS" ;;
	esac
}

push_package() {
	local log="${E2E_WORKDIR}/port-forward.log" port="" deadline
	# With ":5000" kubectl picks a free local port, so that nothing else that
	# listens on this machine can receive the push.
	e2e::kubectl_argv
	"${E2E_KUBECTL[@]}" -n "${E2E_NS_REGISTRY}" port-forward --address 127.0.0.1 service/registry :5000 >"${log}" 2>&1 &
	PF_PID=$!
	deadline=$(($(date +%s) + 30))
	while :; do
		# "Forwarding from 127.0.0.1:<local port> -> <pod port>"
		port="$(sed -n -E 's/^Forwarding from 127\.0\.0\.1:([0-9]+) -> [0-9]+$/\1/p' "${log}" | head -n 1)"
		[ -z "${port}" ] || break
		kill -0 "${PF_PID}" 2>/dev/null || e2e::die "the registry port-forward exited: $(cat "${log}")"
		[ "$(date +%s)" -lt "${deadline}" ] || e2e::die "the registry port-forward did not come up: $(cat "${log}")"
		sleep 1
	done
	e2e::log "pushing the package through a port-forward (127.0.0.1:${port})"
	e2e::wait_until 30 "the registry behind the port-forward" curl -fsS "http://127.0.0.1:${port}/v2/" ||
		e2e::die "the registry does not answer through the port-forward: $(cat "${log}")"
	kill -0 "${PF_PID}" 2>/dev/null || e2e::die "the registry port-forward exited: $(cat "${log}")"
	# go-containerregistry speaks plain HTTP to loopback registries.
	"${CROSSPLANE}" xpkg push -f "${XPKG}" "127.0.0.1:${port}/${E2E_PROVIDER}:${TAG}"
	# The fakegcp image the same way, with crane (go-containerregistry, which
	# also speaks plain HTTP to loopback registries); the node pulls it from
	# the registry's node-local address like the runtime image.
	e2e::log "pushing the fakegcp image"
	"${CRANE}" push "${FAKEGCP_TAR}" "127.0.0.1:${port}/${E2E_FAKEGCP_REPO}:${TAG}" >/dev/null
	# The registry accepts pushes without authentication, and on OrbStack and
	# Docker Desktop its node port is reachable from the host: the package,
	# the runtime image and the fakegcp image are referenced by the digests
	# of these pushes, so that a later push to the tag cannot change what the
	# cluster runs.
	local digest fakegcp_digest
	digest="$(e2e::registry_digest "${port}" "${E2E_PROVIDER}" "${TAG}")" || e2e::die "cannot read the digest of the pushed package"
	fakegcp_digest="$(e2e::registry_digest "${port}" "${E2E_FAKEGCP_REPO}" "${TAG}")" || e2e::die "cannot read the digest of the pushed fakegcp image"
	kill "${PF_PID}" 2>/dev/null || true
	wait "${PF_PID}" 2>/dev/null || true
	PF_PID=""
	PACKAGE="${REGISTRY_IP}:5000/${E2E_PROVIDER}@${digest}"
	RUNTIME_IMAGE="localhost:${E2E_REGISTRY_PORT}/${E2E_PROVIDER}@${digest}"
	FAKEGCP_IMAGE="localhost:${E2E_REGISTRY_PORT}/${E2E_FAKEGCP_REPO}@${fakegcp_digest}"
}

install_crossplane() {
	e2e::log "installing Crossplane ${CROSSPLANE_VERSION}"
	e2e::ensure_namespace "${E2E_NS_CROSSPLANE}"
	# No default ManagedResourceActivationPolicy: the tests activate the
	# provider's kinds themselves (safe-start).
	e2e::helm upgrade --install crossplane crossplane --repo "${CROSSPLANE_CHART_REPO}" --version "${CROSSPLANE_VERSION}" \
		--namespace "${E2E_NS_CROSSPLANE}" --set-json 'provider.defaultActivations=[]' --wait --timeout 10m
	e2e::kubectl wait --for=condition=Established --timeout=180s \
		crd/providers.pkg.crossplane.io crd/deploymentruntimeconfigs.pkg.crossplane.io \
		crd/managedresourceactivationpolicies.apiextensions.crossplane.io crd/managedresourcedefinitions.apiextensions.crossplane.io
	if e2e::kubectl get managedresourceactivationpolicies.apiextensions.crossplane.io -o name | grep -q .; then
		e2e::die "Crossplane created a ManagedResourceActivationPolicy although provider.defaultActivations is empty"
	fi
}

# deploy_fakegcp starts the fake GCE metadata server and Google JWKS from
# test/fakegcp/deploy/fakegcp.yaml with the image pushed above, and reads the
# provider's Google service account (its --email flag).
deploy_fakegcp() {
	e2e::log "starting the fake GCE metadata server and Google JWKS (${FAKEGCP_IMAGE})"
	grep -q '^          image: fakegcp:dev$' "${FAKEGCP_MANIFEST}" || e2e::die "${FAKEGCP_MANIFEST} has no 'image: fakegcp:dev' to replace"
	grep -q "^  namespace: ${E2E_NS_FAKEGCP}$" "${FAKEGCP_MANIFEST}" || e2e::die "${FAKEGCP_MANIFEST} does not use the namespace ${E2E_NS_FAKEGCP}"
	GOOGLE_EMAIL="$(sed -n -E 's/^ +- --email=([a-z0-9-]+@[a-z0-9-]+\.iam\.gserviceaccount\.com)$/\1/p' "${FAKEGCP_MANIFEST}")"
	[ "$(printf '%s\n' "${GOOGLE_EMAIL}" | grep -c .)" = "1" ] || e2e::die "cannot read the service account (--email) from ${FAKEGCP_MANIFEST}"
	sed -e "s|^          image: fakegcp:dev$|          image: ${FAKEGCP_IMAGE}|" "${FAKEGCP_MANIFEST}" | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl -n "${E2E_NS_FAKEGCP}" rollout status deployment/fakegcp --timeout=300s
}

deploy_clickhouse() {
	e2e::log "starting ClickHouse: ${E2E_UPSTREAM_IMAGE} and ${E2E_ANTALYA_IMAGE}"
	e2e::ensure_namespace "${E2E_NS_CLICKHOUSE}"
	local dir="${E2E_WORKDIR}/clickhouse" upstream_grants antalya_grants comment_mode="with-comment"
	mkdir -p "${dir}"
	case "${E2E_UPSTREAM_IMAGE}" in *:25.3*) comment_mode="no-comment" ;; esac
	upstream_grants="$(e2e::readme_grants "${comment_mode}")"
	antalya_grants="$(e2e::readme_grants with-comment)"
	[ "$(printf '%s\n' "${upstream_grants}" | grep -c '^GRANT')" -ge 10 ] || e2e::die "cannot read the provider grants from the README quick start"

	# Upstream: a password identity for the provider (ClusterProviderConfig)
	# and one for the tenant (namespaced ProviderConfig), with the README
	# grants.
	{
		printf 'users:\n'
		local who
		for who in provider-clickhouse tenant-admin; do
			printf '  %s:\n    password_sha256_hex: "%s"\n    networks: {ip: ["::/0"]}\n    profile: default\n    quota: default\n    grants:\n      query:\n' \
				"${who}" "$(e2e::sha256_hex "$(e2e::secret_file "upstream-${who}")")"
			printf '%s\n' "${upstream_grants}" | e2e::grants_yaml
		done
	} >"${dir}/upstream-users.yaml"

	# Antalya: JWT identities for the provider's service account (TokenFile,
	# ClusterProviderConfig) and a tenant service account (TokenSecret,
	# namespaced ProviderConfig), pinned to the "kubernetes" processor, with
	# the README grants and the user DDL for service account users of the
	# test namespaces (design Appendix B); and for the provider's Google
	# service account (GoogleIDToken through the fake metadata server),
	# pinned to the "google_jwt" processor, with the README grants and the
	# user DDL for users named after workload service accounts (app-...).
	{
		printf 'users:\n'
		local who
		for who in "system:serviceaccount:${E2E_NS_CROSSPLANE}:${E2E_PROVIDER}" "system:serviceaccount:xpch-e2e-antalya:tenant-admin"; do
			printf '  "%s":\n    jwt: {processor: kubernetes}\n    networks: {ip: ["::/0"]}\n    profile: default\n    quota: default\n    grants:\n      query:\n' "${who}"
			{
				printf '%s\n' "${antalya_grants}"
				# shellcheck disable=SC2016 # Backticks quote the ClickHouse name.
				printf '%s\n' 'GRANT CREATE USER, ALTER USER, DROP USER ON `system:serviceaccount:xpch-e2e-`*'
			} | e2e::grants_yaml
		done
		printf '  "%s":\n    jwt: {processor: google_jwt}\n    networks: {ip: ["::/0"]}\n    profile: default\n    quota: default\n    grants:\n      query:\n' "${GOOGLE_EMAIL}"
		{
			printf '%s\n' "${antalya_grants}"
			# shellcheck disable=SC2016 # Backticks quote the ClickHouse name.
			printf '%s\n' 'GRANT CREATE USER, ALTER USER, DROP USER ON `app-`*'
		} | e2e::grants_yaml
	} >"${dir}/antalya-users.yaml"

	# The token processor validates projected service account tokens with the
	# cluster's signing keys, read from the API server now (design Appendix
	# B, variant b).
	e2e::kubectl get --raw /openid/v1/jwks >"${dir}/kubernetes.json"
	local issuer
	issuer="$(e2e::kubectl get --raw /.well-known/openid-configuration | sed -E 's/.*"issuer":"([^"]+)".*/\1/')"
	case "${issuer}" in http*) ;; *) e2e::die "cannot read the service account issuer" ;; esac
	# Next to it, the production google_jwt processor (as
	# fakegcp.GoogleProcessor.YAML renders it; the tests compare) with
	# jwks_uri pointing at the fake, this run's audience and short cache
	# lifetimes.
	cat >"${dir}/token_processors.yaml" <<EOF
token_processors:
  kubernetes:
    type: jwt_static_jwks
    static_jwks_file: /etc/clickhouse-server/jwks/kubernetes.json
    expected_issuer: ${issuer}
    expected_audience: clickhouse
    username_claim: sub
  google_jwt:
    type: jwt_dynamic_jwks
    jwks_uri: "http://${FAKEGCP_HOST}/oauth2/v3/certs"
    expected_issuer: "https://accounts.google.com"
    expected_audience: "${E2E_GOOGLE_AUDIENCE}"
    expected_typ: JWT
    claims: '{"email_verified": true}'
    username_claim: email
    jwks_cache_lifetime: ${E2E_GOOGLE_CACHE_LIFETIME}
    token_cache_lifetime: ${E2E_GOOGLE_CACHE_LIFETIME}
EOF

	local ns=(-n "${E2E_NS_CLICKHOUSE}")
	e2e::kubectl "${ns[@]}" create secret generic upstream-admin --from-file=password="$(e2e::secret_file upstream-admin)" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl "${ns[@]}" create secret generic antalya-admin --from-file=password="$(e2e::secret_file antalya-admin)" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl "${ns[@]}" create secret generic upstream-users-d --from-file=provider-clickhouse.yaml="${dir}/upstream-users.yaml" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl "${ns[@]}" create secret generic antalya-users-d --from-file=provider-clickhouse.yaml="${dir}/antalya-users.yaml" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl "${ns[@]}" create configmap antalya-config --from-file=token_processors.yaml="${dir}/token_processors.yaml" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl "${ns[@]}" create configmap antalya-jwks --from-file=kubernetes.json="${dir}/kubernetes.json" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	sed -e "s|@@UPSTREAM_IMAGE@@|${E2E_UPSTREAM_IMAGE}|" -e "s|@@ANTALYA_IMAGE@@|${E2E_ANTALYA_IMAGE}|" \
		"${E2E_DIR}/manifests/clickhouse.yaml" | e2e::kubectl apply -f - >/dev/null
}

wait_clickhouse() {
	e2e::kubectl -n "${E2E_NS_CLICKHOUSE}" rollout status deployment/upstream --timeout=600s
	e2e::kubectl -n "${E2E_NS_CLICKHOUSE}" rollout status deployment/antalya --timeout=600s
}

create_tenants() {
	e2e::log "creating the tenant namespaces and the provider credentials"
	local ns
	for ns in xpch-e2e-upstream xpch-e2e-antalya xpch-e2e-outsider "${E2E_NS_GOOGLE}"; do e2e::ensure_namespace "${ns}"; done
	# The ClusterProviderConfig of the upstream server reads the provider's
	# password from crossplane-system; the namespaced ProviderConfigs read
	# the tenants' credentials from their own namespaces.
	e2e::kubectl -n "${E2E_NS_CROSSPLANE}" create secret generic clickhouse-upstream \
		--from-file=password="$(e2e::secret_file upstream-provider-clickhouse)" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl -n xpch-e2e-upstream create secret generic clickhouse-tenant \
		--from-file=password="$(e2e::secret_file upstream-tenant-admin)" --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	e2e::kubectl -n xpch-e2e-antalya create serviceaccount tenant-admin --dry-run=client -o yaml | e2e::kubectl apply -f - >/dev/null
	# A token of the tenant's service account for the TokenSecret source. It
	# goes straight from the TokenRequest into the Secret.
	e2e::kubectl -n xpch-e2e-antalya create token tenant-admin --audience clickhouse --duration 4h |
		e2e::kubectl -n xpch-e2e-antalya create secret generic clickhouse-tenant --from-file=token=/dev/stdin --dry-run=client -o yaml |
		e2e::kubectl apply -f - >/dev/null
}

install_provider() {
	e2e::log "installing ${PACKAGE}"
	# The DeploymentRuntimeConfig of examples/runtime/projected-token.yaml
	# (projected token with the audience "clickhouse" for TokenFile), plus a
	# short poll interval for the drift scenarios, debug logs, the runtime
	# image (the same package, pulled by the node from the registry's
	# node-local address) and GCE_METADATA_HOST, which sends the Google
	# client libraries' metadata requests (Application Default Credentials of
	# the GoogleIDToken source) to the fake, as
	# test/fakegcp/deploy/provider-runtime.yaml documents. Nothing else reads
	# it: the password, TokenFile and TokenSecret sources are unaffected.
	e2e::kubectl apply -f "${E2E_ROOT}/examples/runtime/projected-token.yaml" >/dev/null
	local container=/spec/deploymentTemplate/spec/template/spec/containers/0 env_op
	env_op="{\"op\": \"add\", \"path\": \"${container}/env\", \"value\": [{\"name\": \"GCE_METADATA_HOST\", \"value\": \"${FAKEGCP_HOST}\"}]}"
	if [ -n "$(e2e::kubectl get deploymentruntimeconfigs.pkg.crossplane.io "${E2E_PROVIDER}" -o "jsonpath={.spec.deploymentTemplate.spec.template.spec.containers[0].env}")" ]; then
		# Keep the variables the example sets.
		env_op="{\"op\": \"add\", \"path\": \"${container}/env/-\", \"value\": {\"name\": \"GCE_METADATA_HOST\", \"value\": \"${FAKEGCP_HOST}\"}}"
	fi
	e2e::kubectl patch deploymentruntimeconfigs.pkg.crossplane.io "${E2E_PROVIDER}" --type=json -p "[
		{\"op\": \"add\", \"path\": \"${container}/image\", \"value\": \"${RUNTIME_IMAGE}\"},
		{\"op\": \"add\", \"path\": \"${container}/args\", \"value\": [\"--debug\", \"--poll=${E2E_POLL_INTERVAL}\"]},
		${env_op}
	]" >/dev/null
	# examples/provider.yaml with this run's package and its runtimeConfigRef
	# enabled.
	local provider="${E2E_WORKDIR}/provider.yaml"
	sed -E -e "s|^(  package: ).*$|\1${PACKAGE}|" -e 's|^  # runtimeConfigRef:|  runtimeConfigRef:|' \
		-e "s|^  #   name: ${E2E_PROVIDER}.*$|    name: ${E2E_PROVIDER}|" "${E2E_ROOT}/examples/provider.yaml" >"${provider}"
	if ! grep -q "^  package: ${PACKAGE}$" "${provider}" || ! grep -q "^    name: ${E2E_PROVIDER}$" "${provider}"; then
		e2e::die "cannot derive the Provider from examples/provider.yaml"
	fi
	e2e::kubectl apply -f "${provider}" >/dev/null
	e2e::kubectl wait "providers.pkg.crossplane.io/${E2E_PROVIDER}" --for=condition=Installed --timeout=300s
	e2e::kubectl wait "providers.pkg.crossplane.io/${E2E_PROVIDER}" --for=condition=Healthy --timeout=300s
}

run_tests() {
	e2e::log "running test/e2e"
	(
		cd "${E2E_ROOT}"
		# shellcheck disable=SC2086 # E2E_GO_TEST_ARGS is a list of flags.
		E2E_POLL_INTERVAL="${E2E_POLL_INTERVAL}" E2E_CROSSPLANE_VERSION="${CROSSPLANE_VERSION}" E2E_GOOGLE_AUDIENCE="${E2E_GOOGLE_AUDIENCE}" \
			"${GO}" test -tags e2e -count=1 -timeout "${E2E_TEST_TIMEOUT}" -v ./test/e2e/... ${E2E_GO_TEST_ARGS}
	) 2>&1 | tee "${ARTIFACTS}/go-test.log"
}

main() {
	command -v kubectl >/dev/null || e2e::die "kubectl is required"
	command -v helm >/dev/null || e2e::die "helm is required"
	command -v openssl >/dev/null || e2e::die "openssl is required"
	[ -x "${CROSSPLANE}" ] || e2e::die "the Crossplane CLI ${CROSSPLANE} is missing (make crossplane)"
	[ -x "${CRANE}" ] || e2e::die "crane ${CRANE} is missing (run make e2e)"
	e2e::foreign_crossplane
	mkdir -p "${E2E_WORKDIR}"
	rm -rf "${ARTIFACTS}" "${E2E_WORKDIR}/secrets"
	mkdir -p "${ARTIFACTS}"
	# Leftovers of an E2E_KEEP=1 run would mix with this one.
	if e2e::kubectl get namespaces -l "${E2E_LABEL}" -o name | grep -q .; then
		e2e::log "removing the leftovers of an earlier run"
		"${E2E_DIR}/cleanup.sh"
	fi
	trap on_exit EXIT
	# Without these, bash runs the EXIT trap of a run stopped by a signal (Ctrl-C,
	# a cancelled CI job) with status 0, and on_exit would skip the
	# diagnostics.
	trap 'exit 130' INT
	trap 'exit 143' TERM
	e2e::log "context ${E2E_KUBE_CONTEXT}, Crossplane ${CROSSPLANE_VERSION}, work directory ${E2E_WORKDIR}"
	e2e::log "$(kubectl version --client 2>/dev/null | head -n 1), helm $(helm version --short 2>/dev/null)"

	# The audience of this run's GoogleIDToken config and google_jwt
	# processor: unique per deployment, as in production.
	E2E_GOOGLE_AUDIENCE="https://clickhouse-e2e-$(e2e::random_hex 4).example.com"

	build_package
	deploy_registry
	push_package
	install_crossplane
	deploy_fakegcp
	deploy_clickhouse
	create_tenants
	install_provider
	wait_clickhouse
	run_tests
	e2e::log "the end-to-end tests passed (Crossplane ${CROSSPLANE_VERSION}, context ${E2E_KUBE_CONTEXT})"
}

main "$@"
