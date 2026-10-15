# shellcheck shell=bash
# Copyright 2026 The provider-clickhouse Authors
# SPDX-License-Identifier: Apache-2.0
#
# Shared helpers of the end-to-end harness (run.sh, cleanup.sh,
# diagnostics.sh). Sourced, not executed. Written for bash 3.2 (the macOS default) and later.
#
# The harness only ever talks to the cluster named by E2E_KUBE_CONTEXT, which
# must be set explicitly and must be a local cluster (see e2e::guard). Every
# kubectl and helm call goes through e2e::kubectl and e2e::helm, which pass the
# context (and E2E_KUBECONFIG, if set) explicitly; the current context of the
# kubeconfig is never used and never changed.

# The constants below are used by the scripts that source this file.
# shellcheck disable=SC2034

E2E_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
E2E_DIR="${E2E_ROOT}/hack/e2e"

# Local cluster contexts the harness may run against. Anything else, including
# every cloud context, is refused.
E2E_ALLOWED_CONTEXTS="orbstack docker-desktop minikube rancher-desktop kind-* k3d-*"

# The label of every namespace the harness creates; cleanup removes exactly
# these namespaces.
E2E_LABEL_KEY="app.kubernetes.io/part-of"
E2E_LABEL_VALUE="xpch-e2e"
E2E_LABEL="${E2E_LABEL_KEY}=${E2E_LABEL_VALUE}"

# Fixed names shared with test/e2e.
E2E_NS_REGISTRY="xpch-e2e-registry"
E2E_NS_CLICKHOUSE="xpch-e2e-clickhouse"
E2E_NS_CROSSPLANE="crossplane-system"
# The fake GCE metadata server and Google JWKS (test/fakegcp/deploy) and the
# tenant namespace of the GoogleIDToken scenario.
E2E_NS_FAKEGCP="xpch-e2e-fakegcp"
E2E_NS_GOOGLE="xpch-e2e-google"
E2E_FAKEGCP_REPO="fakegcp"
E2E_PROVIDER="provider-clickhouse"
E2E_MR_KINDS="databases users roles grants rolegrants"
E2E_GROUP="clickhouse.m.infrabay.io"

E2E_WORKDIR="${E2E_WORKDIR:-${E2E_ROOT}/_output/e2e}"

e2e::log() { printf '==> %s\n' "$*" >&2; }
e2e::warn() { printf 'WARNING: %s\n' "$*" >&2; }
e2e::die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

# e2e::kubectl runs kubectl against E2E_KUBE_CONTEXT only.
e2e::kubectl() {
	e2e::kubectl_argv
	"${E2E_KUBECTL[@]}" "$@"
}

# e2e::kubectl_argv sets the array E2E_KUBECTL to the kubectl command for
# E2E_KUBE_CONTEXT (and E2E_KUBECONFIG). A kubectl that runs in the
# background must be started from it, not through e2e::kubectl: then $! is
# the PID of kubectl itself rather than of a subshell, and killing it stops
# kubectl.
e2e::kubectl_argv() {
	E2E_KUBECTL=(kubectl --context "${E2E_KUBE_CONTEXT}")
	if [ -n "${E2E_KUBECONFIG:-}" ]; then
		E2E_KUBECTL=(kubectl --kubeconfig "${E2E_KUBECONFIG}" --context "${E2E_KUBE_CONTEXT}")
	fi
}

# e2e::helm runs helm against E2E_KUBE_CONTEXT only, with its configuration,
# cache and data kept in the work directory.
e2e::helm() {
	local home="${E2E_WORKDIR}/helm"
	mkdir -p "${home}/cache" "${home}/config" "${home}/data"
	if [ -n "${E2E_KUBECONFIG:-}" ]; then
		HELM_CACHE_HOME="${home}/cache" HELM_CONFIG_HOME="${home}/config" HELM_DATA_HOME="${home}/data" \
			helm --kubeconfig "${E2E_KUBECONFIG}" --kube-context "${E2E_KUBE_CONTEXT}" "$@"
	else
		HELM_CACHE_HOME="${home}/cache" HELM_CONFIG_HOME="${home}/config" HELM_DATA_HOME="${home}/data" \
			helm --kube-context "${E2E_KUBE_CONTEXT}" "$@"
	fi
}

# e2e::context_allowed reports whether $1 matches the local allow-list.
e2e::context_allowed() {
	local ctx="$1" pattern
	for pattern in ${E2E_ALLOWED_CONTEXTS}; do
		# shellcheck disable=SC2254 # The allow-list entries are glob patterns.
		case "${ctx}" in ${pattern}) return 0 ;; esac
	done
	return 1
}

# e2e::context_name_valid reports whether $1 is a context name the harness
# accepts: letters, digits and . _ @ : - only, so that it is safe in the
# jsonpath filter of e2e::guard.
e2e::context_name_valid() {
	case "$1" in
	'' | *[!A-Za-z0-9._@:-]*) return 1 ;;
	esac
	return 0
}

# e2e::ipv4_local reports whether $1 is a dotted IPv4 address in 127/8, 10/8,
# 172.16/12 or 192.168/16, or 0.0.0.0. The octets are compared as numbers, so
# that a host name such as 10.example.com does not pass.
e2e::ipv4_local() {
	local a b c d o
	printf '%s\n' "$1" | grep -Eq '^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$' || return 1
	IFS=. read -r a b c d <<EOF
$1
EOF
	for o in "${a}" "${b}" "${c}" "${d}"; do
		[ "$((10#${o}))" -le 255 ] || return 1
	done
	a=$((10#${a})) b=$((10#${b})) c=$((10#${c})) d=$((10#${d}))
	[ "${a}" -eq 127 ] || [ "${a}" -eq 10 ] ||
		{ [ "${a}" -eq 172 ] && [ "${b}" -ge 16 ] && [ "${b}" -le 31 ]; } ||
		{ [ "${a}" -eq 192 ] && [ "${b}" -eq 168 ]; } ||
		{ [ "${a}" -eq 0 ] && [ "${b}" -eq 0 ] && [ "${c}" -eq 0 ] && [ "${d}" -eq 0 ]; }
}

# e2e::server_local reports whether the API server URL $1 is on this machine
# or on a private network: a loopback or private IPv4 address (compared as
# numbers), ::1, an IPv6 unique local address, or a host name that local
# cluster tools use (exact names, and *.orb.local). The same rules as
# serverLocal in test/e2e/guard_test.go. A private address alone does not
# make a cluster local (a cloud cluster can have a private endpoint): the
# context name allow-list and e2e::refuse_cloud_nodes come first and last.
e2e::server_local() {
	local hostport host
	hostport="$(printf '%s' "$1" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##; s#/.*$##; s#^[^@]*@##')"
	case "${hostport}" in
	\[*) host="${hostport#[}" && host="${host%%]*}" ;;
	*:*) host="${hostport%:*}" ;;
	*) host="${hostport}" ;;
	esac
	case "${host}" in
	localhost | host.docker.internal | kubernetes.docker.internal | *.orb.local) return 0 ;;
	::1 | :: | [fF][cCdD][0-9a-fA-F][0-9a-fA-F]:*) return 0 ;;
	esac
	e2e::ipv4_local "${host}"
}

# e2e::refuse_cloud_nodes fails when a node of the cluster runs at a cloud
# provider: a provider ID of a cloud (gce://, aws://, azure:// and others) or
# the node labels of GKE, EKS or AKS. Defence in depth behind the context
# name and API server checks.
e2e::refuse_cloud_nodes() {
	local nodes
	nodes="$(e2e::kubectl get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.spec.providerID}{"|"}{.metadata.labels}{"\n"}{end}')" ||
		e2e::die "cannot list the nodes of '${E2E_KUBE_CONTEXT}'"
	[ -n "${nodes}" ] || e2e::die "'${E2E_KUBE_CONTEXT}' has no nodes"
	if printf '%s\n' "${nodes}" | grep -Eq '\|(gce|aws|azure|digitalocean|linode|oci|ibm|openstack|vsphere|hcloud|alicloud|exoscale|scaleway|equinixmetal)://|cloud\.google\.com/gke-|eks\.amazonaws\.com/|kubernetes\.azure\.com/'; then
		e2e::die "context '${E2E_KUBE_CONTEXT}' has nodes at a cloud provider; refusing"
	fi
}

# e2e::guard refuses to continue unless E2E_KUBE_CONTEXT is set explicitly,
# matches the local allow-list, exists in the kubeconfig, points at a local
# API server and has no cloud nodes. It never falls back to the current
# context.
e2e::guard() {
	if [ -z "${E2E_KUBE_CONTEXT:-}" ]; then
		e2e::die "E2E_KUBE_CONTEXT is not set. Name the local cluster explicitly, e.g. E2E_KUBE_CONTEXT=orbstack or E2E_KUBE_CONTEXT=kind-xpch-e2e (the current context is never used)."
	fi
	if ! e2e::context_name_valid "${E2E_KUBE_CONTEXT}"; then
		e2e::die "context name '${E2E_KUBE_CONTEXT}' has characters other than letters, digits and . _ @ : -"
	fi
	if ! e2e::context_allowed "${E2E_KUBE_CONTEXT}"; then
		e2e::die "context '${E2E_KUBE_CONTEXT}' is not a local cluster context (allowed: ${E2E_ALLOWED_CONTEXTS})"
	fi
	local kc=()
	if [ -n "${E2E_KUBECONFIG:-}" ]; then
		[ -f "${E2E_KUBECONFIG}" ] || e2e::die "E2E_KUBECONFIG ${E2E_KUBECONFIG} does not exist"
		kc=(--kubeconfig "${E2E_KUBECONFIG}")
	fi
	local cluster server
	# An empty kubeconfig has no contexts list, which the filter rejects.
	cluster="$(kubectl "${kc[@]+"${kc[@]}"}" config view -o "jsonpath={.contexts[?(@.name==\"${E2E_KUBE_CONTEXT}\")].context.cluster}" 2>/dev/null || true)"
	[ -n "${cluster}" ] || e2e::die "context '${E2E_KUBE_CONTEXT}' does not exist in the kubeconfig"
	case "${cluster}" in *[\"\'{}\]\[]*) e2e::die "context '${E2E_KUBE_CONTEXT}' names the cluster '${cluster}', which has characters the harness does not accept" ;; esac
	server="$(kubectl "${kc[@]+"${kc[@]}"}" config view -o "jsonpath={.clusters[?(@.name==\"${cluster}\")].cluster.server}")"
	if ! e2e::server_local "${server}"; then
		e2e::die "context '${E2E_KUBE_CONTEXT}' points at API server '${server}', which is not local; refusing"
	fi
	export E2E_KUBE_CONTEXT
	if [ -n "${E2E_KUBECONFIG:-}" ]; then export E2E_KUBECONFIG; fi
	e2e::refuse_cloud_nodes
}

# e2e::crossplane_namespace sets E2E_CROSSPLANE_NAMESPACE to "ours" when the
# crossplane-system namespace exists with the harness label and to "absent"
# when it does not exist. It dies when the namespace exists without the
# label, or cannot be read. Call it directly, not in $(...), so that it can
# stop the script.
e2e::crossplane_namespace() {
	local out
	out="$(e2e::kubectl get namespace "${E2E_NS_CROSSPLANE}" --ignore-not-found -o "jsonpath={.metadata.name}|{.metadata.labels.app\.kubernetes\.io/part-of}" 2>&1)" ||
		e2e::die "cannot read namespace ${E2E_NS_CROSSPLANE}: ${out}"
	case "${out}" in
	"") E2E_CROSSPLANE_NAMESPACE=absent ;;
	"${E2E_NS_CROSSPLANE}|${E2E_LABEL_VALUE}") E2E_CROSSPLANE_NAMESPACE=ours ;;
	*) e2e::die "namespace ${E2E_NS_CROSSPLANE} exists and was not created by the e2e harness (no label ${E2E_LABEL}); refusing to touch this Crossplane installation" ;;
	esac
}

# e2e::foreign_crossplane_deployments prints the Crossplane Deployments
# outside crossplane-system (label app, app.kubernetes.io/name or
# app.kubernetes.io/part-of set to crossplane or crossplane-rbac-manager), for
# example a Crossplane or UXP installed into another namespace. It fails if
# kubectl does.
e2e::foreign_crossplane_deployments() {
	local out
	out="$(e2e::kubectl get deployments -A -o 'jsonpath={range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"|"}{.metadata.labels.app}{"|"}{.metadata.labels.app\.kubernetes\.io/name}{"|"}{.metadata.labels.app\.kubernetes\.io/part-of}{"\n"}{end}')" || return 1
	printf '%s\n' "${out}" | grep -E '\|(crossplane|crossplane-rbac-manager)(\||$)' | grep -v "^${E2E_NS_CROSSPLANE}/" | sed -E 's#^([^|]*)\|.*#deployment/\1#' || true
}

# e2e::crossplane_cluster_objects prints the cluster-scoped objects that
# cleanup.sh removes as part of a Crossplane installation the harness made:
# *.crossplane.io CRDs, and ClusterRoles, ClusterRoleBindings and webhook
# configurations whose names start with "crossplane". It fails if kubectl
# does.
e2e::crossplane_cluster_objects() {
	local out type
	out="$(e2e::kubectl get crd -o name)" || return 1
	printf '%s\n' "${out}" | grep -E '\.crossplane\.io$' || true
	for type in clusterroles clusterrolebindings validatingwebhookconfigurations mutatingwebhookconfigurations; do
		out="$(e2e::kubectl get "${type}" -o name)" || return 1
		printf '%s\n' "${out}" | grep -E '/crossplane' || true
	done
}

# e2e::foreign_crossplane fails, before the harness installs anything, when
# the cluster has Crossplane objects that the harness did not create: it
# would install a second Crossplane next to them, and its cleanup would
# remove them. With the harness's own crossplane-system (left by E2E_KEEP=1
# or an interrupted run), Crossplane Deployments in other namespaces count;
# without it, any Crossplane Deployment and any object of
# e2e::crossplane_cluster_objects does, including the CRDs and RBAC that an
# uninstalled Crossplane left behind.
e2e::foreign_crossplane() {
	local found more
	e2e::crossplane_namespace
	found="$(e2e::foreign_crossplane_deployments)" || e2e::die "cannot list the Deployments of '${E2E_KUBE_CONTEXT}'"
	if [ "${E2E_CROSSPLANE_NAMESPACE}" = absent ]; then
		more="$(e2e::crossplane_cluster_objects)" || e2e::die "cannot list the CRDs, ClusterRoles, ClusterRoleBindings and webhook configurations of '${E2E_KUBE_CONTEXT}'"
		found="$(printf '%s\n%s\n' "${found}" "${more}" | grep . || true)"
	fi
	if [ -n "${found}" ]; then
		e2e::die "Crossplane objects that the e2e harness did not create exist in '${E2E_KUBE_CONTEXT}'; refusing to install a second Crossplane and to remove them afterwards. Use a cluster without Crossplane, or remove them first:
$(printf '%s\n' "${found}" | sed 's/^/  /')"
	fi
}

# e2e::ensure_namespace creates a namespace with the harness label.
e2e::ensure_namespace() {
	e2e::kubectl create namespace "$1" --dry-run=client -o yaml |
		e2e::kubectl label --local -f - "${E2E_LABEL}" -o yaml |
		e2e::kubectl apply -f - >/dev/null
}

# e2e::random_hex prints n random bytes as hex.
e2e::random_hex() { openssl rand -hex "$1"; }

# e2e::sha256_hex prints the SHA-256 of the content of file $1 as hex.
e2e::sha256_hex() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# e2e::secret_file writes a secret value to a 0600 file in the work
# directory (never printed) and prints its path.
e2e::secret_file() {
	local path="${E2E_WORKDIR}/secrets/$1"
	mkdir -p "${E2E_WORKDIR}/secrets"
	chmod 700 "${E2E_WORKDIR}/secrets"
	if [ ! -s "${path}" ]; then
		(umask 077 && e2e::random_hex 16 | tr -d '\n' >"${path}")
	fi
	printf '%s' "${path}"
}

# e2e::wait_until runs "$@" every 2 s until it succeeds or $1 seconds pass.
e2e::wait_until() {
	local timeout="$1" what="$2"
	shift 2
	local deadline=$(($(date +%s) + timeout))
	until "$@" >/dev/null 2>&1; do
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			e2e::warn "timed out after ${timeout}s waiting for ${what}"
			return 1
		fi
		sleep 2
	done
}

# e2e::redact replaces every secret value the harness generated with
# [REDACTED] in the files given as arguments (in place).
e2e::redact() {
	local f s value
	[ -d "${E2E_WORKDIR}/secrets" ] || return 0
	for f in "$@"; do
		[ -f "${f}" ] || continue
		for s in "${E2E_WORKDIR}"/secrets/*; do
			[ -s "${s}" ] || continue
			value="$(cat "${s}")"
			# The values are hex, so they are safe in a sed expression.
			sed -i.bak "s/${value}/[REDACTED]/g" "${f}" && rm -f "${f}.bak"
		done
	done
}

# e2e::registry_digest prints the digest of the manifest that the registry
# at 127.0.0.1:$1 serves for repository $2 and tag $3, or fails.
e2e::registry_digest() {
	local digest
	digest="$(curl -fsS -o /dev/null -D - \
		-H 'Accept: application/vnd.oci.image.index.v1+json' -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
		-H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
		"http://127.0.0.1:$1/v2/$2/manifests/$3" | tr -d '\r' |
		sed -n -E 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: (sha256:[0-9a-f]{64})$/\1/p')"
	[ -n "${digest}" ] || return 1
	printf '%s' "${digest}"
}

# e2e::readme_grants prints the grants of the provider identity from the README
# quick start (the users.d example), one per line, without comments. With
# "no-comment" the line marked "omit on 25.3" is left out. The end-to-end
# tests thereby prove that the documented grants are enough.
e2e::readme_grants() {
	local grants
	grants="$(sed -n '/users.d\/provider-clickhouse.yaml/,/^```/p' "${E2E_ROOT}/README.md" |
		sed -n -E 's/^[[:space:]]+- "(GRANT [^"]+)".*$/\1|&/p')"
	if [ "$1" = "no-comment" ]; then
		grants="$(printf '%s\n' "${grants}" | grep -v 'omit on 25.3')"
	fi
	printf '%s\n' "${grants}" | cut -d'|' -f1
}

# e2e::grants_yaml prints grants (one per line on stdin) as a YAML list indented
# for users.d.
e2e::grants_yaml() {
	while IFS= read -r g; do
		if [ -n "${g}" ]; then printf '        - "%s"\n' "${g}"; fi
	done
}
