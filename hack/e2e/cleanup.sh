#!/usr/bin/env bash
# Copyright 2026 The provider-clickhouse Authors
# SPDX-License-Identifier: Apache-2.0
#
# make e2e-cleanup: removes everything make e2e installed from
# E2E_KUBE_CONTEXT and checks that nothing is left:
#
# - the managed resources, ProviderConfigs and ClusterProviderConfigs of the
#   test namespaces (while the provider still runs, so it can remove its
#   finalizers; finalizers that are left after a timeout are removed);
# - only if the harness installed Crossplane: the Provider, its revisions and
#   DeploymentRuntimeConfig, the activation policies the tests created,
#   Crossplane itself (Helm release), the provider's CRDs (unless managed
#   resources outside the test namespaces use them), Crossplane's CRDs, and
#   the ClusterRoles, ClusterRoleBindings and webhook configurations whose
#   names start with "crossplane";
# - every namespace with the label app.kubernetes.io/part-of=xpch-e2e (the
#   registry, the fake GCE metadata server, the ClickHouse servers, the
#   tenants, crossplane-system), which takes their Secrets, Services and
#   volumes with them;
# - events in the default namespace about the removed objects;
# - the runtime and fakegcp images the node pulled from the registry, on
#   OrbStack (the host's Docker engine) and kind (each node's containerd).
#
# The harness installed Crossplane when crossplane-system carries the harness
# label and no Crossplane Deployment runs in another namespace: run.sh
# refuses to start on a cluster with any Crossplane object it did not create
# (e2e::foreign_crossplane), so every Crossplane object is then from the run.
# crossplane-system is removed last, after the cluster-scoped objects, so an
# interrupted cleanup still knows this when it runs again. Without the
# labelled namespace, Crossplane and the provider's CRDs are left alone. A
# crossplane-system without the label stops the cleanup.
#
# It is idempotent and safe to run when nothing is installed.
set -euo pipefail

# shellcheck source=hack/e2e/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

E2E_REGISTRY_PORT="${E2E_REGISTRY_PORT:-35000}"

e2e::guard

k() { e2e::kubectl "$@"; }

# crd_exists reports whether a CRD is installed.
crd_exists() { k get crd "$1" >/dev/null 2>&1; }

# strip_finalizers removes the finalizers of every object of a resource type,
# in the given scope (kubectl get arguments: -n ns, a label selector, or
# nothing for every cluster-scoped object).
strip_finalizers() {
	local type="$1" obj
	shift
	for obj in $(k get "${type}" "$@" -o name 2>/dev/null); do
		k patch "${obj}" "$@" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
	done
}

# delete_and_wait deletes objects (kubectl delete arguments), waits up to $1
# seconds for them to disappear, and removes their finalizers if they do not.
# kubectl get has no --all flag (without a name it lists every object), so
# --all is passed to kubectl delete only.
delete_and_wait() {
	local timeout="$1" type="$2" a out
	shift 2
	local get_args=()
	for a in "$@"; do
		[ "${a}" = --all ] || get_args+=("${a}")
	done
	k delete "${type}" "$@" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	local deadline=$(($(date +%s) + timeout))
	while :; do
		if ! out="$(k get "${type}" "${get_args[@]+"${get_args[@]}"}" --ignore-not-found -o name 2>&1)"; then
			e2e::warn "cannot list ${type} ${get_args[*]+"${get_args[*]}"}: ${out}"
			return 0
		fi
		[ -n "${out}" ] || return 0
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			e2e::warn "${type} ${get_args[*]+"${get_args[*]}"} still exists after ${timeout}s; removing finalizers"
			strip_finalizers "${type}" "${get_args[@]+"${get_args[@]}"}"
			k delete "${type}" "$@" --ignore-not-found --wait=false >/dev/null 2>&1 || true
			return 0
		fi
		sleep 2
	done
}

# Whether the harness installed Crossplane (see the header). The namespace
# check dies if crossplane-system exists without the harness label.
e2e::crossplane_namespace
crossplane_ours=false
if [ "${E2E_CROSSPLANE_NAMESPACE}" = ours ]; then
	foreign="$(e2e::foreign_crossplane_deployments)" || e2e::die "cannot list the Deployments of ${E2E_KUBE_CONTEXT}"
	if [ -n "${foreign}" ]; then
		e2e::warn "Crossplane Deployments outside ${E2E_NS_CROSSPLANE} exist ($(printf '%s' "${foreign}" | tr '\n' ' ')); leaving the Provider, Crossplane with its CRDs and RBAC, and the provider's CRDs alone"
	else
		crossplane_ours=true
	fi
fi

test_namespaces="$(k get namespaces -l "${E2E_LABEL}" -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}')"

e2e::log "removing the managed resources and ProviderConfigs of the test namespaces"
for ns in ${test_namespaces}; do
	for kind in ${E2E_MR_KINDS}; do
		crd_exists "${kind}.${E2E_GROUP}" && k delete "${kind}.${E2E_GROUP}" -n "${ns}" --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
	done
done
for ns in ${test_namespaces}; do
	for kind in ${E2E_MR_KINDS}; do
		crd_exists "${kind}.${E2E_GROUP}" && delete_and_wait 120 "${kind}.${E2E_GROUP}" -n "${ns}" --all
	done
	for kind in providerconfigs providerconfigusages; do
		crd_exists "${kind}.${E2E_GROUP}" && delete_and_wait 30 "${kind}.${E2E_GROUP}" -n "${ns}" --all
	done
done
if crd_exists "clusterproviderconfigs.${E2E_GROUP}"; then
	delete_and_wait 60 "clusterproviderconfigs.${E2E_GROUP}" -l "${E2E_LABEL}"
fi

if [ "${crossplane_ours}" = true ]; then
	e2e::log "removing the provider package and Crossplane"
	if crd_exists providers.pkg.crossplane.io; then
		delete_and_wait 120 providers.pkg.crossplane.io "${E2E_PROVIDER}"
		delete_and_wait 60 providerrevisions.pkg.crossplane.io -l "pkg.crossplane.io/package=${E2E_PROVIDER}"
	fi
	crd_exists deploymentruntimeconfigs.pkg.crossplane.io && delete_and_wait 30 deploymentruntimeconfigs.pkg.crossplane.io "${E2E_PROVIDER}"
	# The tests label the activation policies they create.
	crd_exists managedresourceactivationpolicies.apiextensions.crossplane.io &&
		delete_and_wait 30 managedresourceactivationpolicies.apiextensions.crossplane.io -l "${E2E_LABEL}"
	if e2e::helm status crossplane -n "${E2E_NS_CROSSPLANE}" >/dev/null 2>&1; then
		e2e::helm uninstall crossplane -n "${E2E_NS_CROSSPLANE}" --wait --timeout 5m >/dev/null
	fi

	# CRDs: the provider's (unless resources outside the test namespaces use
	# them) and Crossplane's.
	crds=""
	for crd in $(k get crd -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}'); do
		case "${crd}" in
		*."${E2E_GROUP}")
			outside="$(k get "${crd}" -A -o 'jsonpath={range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u | grep -v -x -F "${test_namespaces:-none}" | grep . || true)"
			if [ -n "${outside}" ] && [ "${crd}" != "clusterproviderconfigs.${E2E_GROUP}" ]; then
				e2e::warn "keeping CRD ${crd}: it is used outside the test namespaces (${outside})"
				continue
			fi
			crds="${crds} ${crd}"
			;;
		*.crossplane.io) crds="${crds} ${crd}" ;;
		esac
	done
	if [ -n "${crds}" ]; then
		e2e::log "removing CRDs:${crds}"
		for crd in ${crds}; do
			# Instances with finalizers block the CRD's deletion once no
			# controller runs any more.
			# A CRD can disappear meanwhile: deleting the activation
			# definitions removes the CRDs they own.
			if [ "$(k get crd "${crd}" -o 'jsonpath={.spec.scope}' 2>/dev/null)" = "Namespaced" ]; then
				for obj in $(k get "${crd}" -A -o 'jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null); do
					k patch "${crd}" -n "${obj%%/*}" "${obj#*/}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
				done
			else
				strip_finalizers "${crd}"
			fi
			k delete crd "${crd}" --ignore-not-found --wait=false >/dev/null
		done
		for crd in ${crds}; do
			k wait --for=delete "crd/${crd}" --timeout=120s >/dev/null 2>&1 || e2e::warn "CRD ${crd} is still being deleted"
		done
	fi

	# Crossplane's RBAC manager and Helm chart create these; the chart's are
	# removed by helm uninstall, the RBAC manager's are not. Nothing
	# recreates them once Crossplane is uninstalled.
	for type in clusterroles clusterrolebindings validatingwebhookconfigurations mutatingwebhookconfigurations; do
		for obj in $(k get "${type}" -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' | grep '^crossplane' || true); do
			k delete "${type}" "${obj}" --ignore-not-found >/dev/null
		done
	done
fi

if [ -n "${test_namespaces}" ]; then
	# crossplane-system last: it records that the harness installed
	# Crossplane (see the header).
	others="$(printf '%s\n' "${test_namespaces}" | grep -v -x -F "${E2E_NS_CROSSPLANE}" || true)"
	e2e::log "removing the namespaces: $(printf '%s\n' "${test_namespaces}" | tr '\n' ' ')"
	for batch in "${others}" "$(printf '%s\n' "${test_namespaces}" | grep -x -F "${E2E_NS_CROSSPLANE}" || true)"; do
		[ -n "${batch}" ] || continue
		# shellcheck disable=SC2086 # One namespace per word.
		k delete namespace ${batch} --ignore-not-found --wait=false >/dev/null
		for ns in ${batch}; do
			k wait --for=delete "namespace/${ns}" --timeout=300s >/dev/null 2>&1 || e2e::warn "namespace ${ns} is still terminating"
		done
	done
fi

# Events about cluster-scoped objects (Providers, revisions, activation
# policies and definitions, CRDs, ClusterProviderConfigs) are recorded in
# "default". If the harness installed Crossplane, those about objects of
# Crossplane's and the provider's APIs, about provider-clickhouse* and about
# CRDs of those APIs are from the run; in any case those about objects named
# xpch-e2e* (ClusterProviderConfigs, activation policies, namespaces).
events="$(k get events -n default -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.involvedObject.apiVersion}{"|"}{.involvedObject.name}{"\n"}{end}')"
for ev in $(printf '%s\n' "${events}" | awk -F'|' -v ours="${crossplane_ours}" '
	$3 ~ /^xpch-e2e/ ||
	(ours == "true" && ($2 ~ /(crossplane|infrabay)\.io\// || $3 ~ /^provider-clickhouse/ || $3 ~ /\.(crossplane|infrabay)\.io$/)) { print $1 }'); do
	k delete event -n default "${ev}" --ignore-not-found >/dev/null 2>&1 || true
done
# Some controllers record events about objects of the test namespaces
# without a namespace, so they land in "default" too (for example the
# endpoint controller about a Service of a namespace being deleted); only
# their message names the namespace.
for ev in $(k get events -n default -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.message}{"\n"}{end}' |
	grep -E '^[^|]*\|.*xpch-e2e-' | cut -d'|' -f1); do
	k delete event -n default "${ev}" --ignore-not-found >/dev/null 2>&1 || true
done

# On OrbStack the node's container runtime is the host's Docker engine: drop
# the runtime and fakegcp images it pulled from the registry (by tag or, as
# run.sh references them, by digest). Built images stay, and so do the
# third-party images the node pulled (Crossplane, ClickHouse, the registry),
# which later runs reuse.
if [ "${E2E_KUBE_CONTEXT}" = "orbstack" ] && command -v docker >/dev/null 2>&1; then
	for repo in "${E2E_PROVIDER}" "${E2E_FAKEGCP_REPO}"; do
		for img in $(docker images --digests --format '{{.Repository}}:{{.Tag}} {{.Repository}}@{{.Digest}}' "localhost:${E2E_REGISTRY_PORT}/${repo}" 2>/dev/null |
			tr ' ' '\n' | grep -v -E ':<none>$|@<none>$' || true); do
			docker image rm "${img}" >/dev/null 2>&1 || true
		done
	done
fi
# On kind each node's containerd keeps them until the cluster is deleted, so
# a cluster that is reused would collect a pair per run: remove them from the
# nodes of the cluster kind-<name> (Docker containers with kind's cluster
# label), and warn about what is left. node_images prints the IDs of those
# images in the kind node $1.
node_images() {
	docker exec "$1" crictl images 2>/dev/null | awk -v a="localhost:${E2E_REGISTRY_PORT}/${E2E_PROVIDER}" \
		-v b="localhost:${E2E_REGISTRY_PORT}/${E2E_FAKEGCP_REPO}" '$1 == a || $1 == b { print $3 }' | sort -u
}
case "${E2E_KUBE_CONTEXT}" in
kind-*)
	if command -v docker >/dev/null 2>&1; then
		for node in $(docker ps --filter "label=io.x-k8s.kind.cluster=${E2E_KUBE_CONTEXT#kind-}" --format '{{.Names}}' 2>/dev/null); do
			for img in $(node_images "${node}"); do
				docker exec "${node}" crictl rmi "${img}" >/dev/null 2>&1 || true
			done
			left="$(node_images "${node}" | tr '\n' ' ')"
			[ -z "${left}" ] || e2e::warn "images still in node ${node}: ${left}"
		done
	fi
	;;
esac

e2e::log "checking for leftovers"
leftovers=""
add() { leftovers="${leftovers}
  $*"; }
for ns in $(k get namespaces -l "${E2E_LABEL}" -o name); do add "${ns}"; done
for cpc in $(crd_exists "clusterproviderconfigs.${E2E_GROUP}" && k get "clusterproviderconfigs.${E2E_GROUP}" -l "${E2E_LABEL}" -o name || true); do add "${cpc}"; done
if [ "${crossplane_ours}" = true ]; then
	for crd in $(k get crd -o name | grep -E "\.${E2E_GROUP//./\\.}\$" || true); do add "${crd}"; done
	for crd in $(k get crd -o name | grep -E '\.crossplane\.io$' || true); do add "${crd}"; done
	for type in clusterroles clusterrolebindings validatingwebhookconfigurations mutatingwebhookconfigurations; do
		for obj in $(k get "${type}" -o name | grep '/crossplane' || true); do add "${obj}"; done
	done
fi
for pv in $(k get pv -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.spec.claimRef.namespace}{"\n"}{end}' | grep -E '\|(xpch-e2e-|crossplane-system)' | cut -d'|' -f1); do add "pv/${pv}"; done
if [ -n "${leftovers}" ]; then
	e2e::warn "leftovers in ${E2E_KUBE_CONTEXT}:${leftovers}"
	exit 1
fi
e2e::log "${E2E_KUBE_CONTEXT} is clean"
