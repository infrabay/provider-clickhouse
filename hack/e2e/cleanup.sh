#!/usr/bin/env bash
# Copyright 2026 The provider-clickhouse Authors
# SPDX-License-Identifier: Apache-2.0
#
# make e2e-cleanup: removes everything make e2e installed from
# E2E_KUBE_CONTEXT and checks that nothing is left:
#
# - the managed resources, ProviderConfigs and ClusterProviderConfigs of the
#   test namespaces (while the provider still runs, so it can remove its
#   finalizers; finalizers that are left are removed afterwards);
# - the Provider, its DeploymentRuntimeConfig and the activation policies;
# - Crossplane (Helm release, CRDs, ClusterRoles, ClusterRoleBindings and
#   webhook configurations), but only if the harness installed it: the
#   crossplane-system namespace carries the harness label;
# - the provider's CRDs, unless managed resources outside the test
#   namespaces still use them;
# - every namespace with the label app.kubernetes.io/part-of=xpch-e2e (the
#   registry, the ClickHouse servers, the tenants, crossplane-system), which
#   takes their Secrets, Services and volumes with them;
# - events in the default namespace about the removed cluster-scoped objects
#   and about objects of the test namespaces.
#
# It is idempotent and safe to run when nothing is installed.
set -euo pipefail

# shellcheck source=hack/e2e/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

E2E_REGISTRY_PORT="${E2E_REGISTRY_PORT:-35000}"

e2e::guard
e2e::foreign_crossplane

k() { e2e::kubectl "$@"; }

# crd_exists reports whether a CRD is installed.
crd_exists() { k get crd "$1" >/dev/null 2>&1; }

# strip_finalizers removes the finalizers of every object of a resource type,
# in the given namespace scope (-n ns, or nothing for cluster-scoped types).
strip_finalizers() {
	local type="$1" obj
	shift
	for obj in $(k get "${type}" "$@" -o name 2>/dev/null); do
		k patch "${obj}" "$@" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
	done
}

# delete_and_wait deletes objects (kubectl delete arguments), waits up to $1
# seconds for them to disappear, and removes their finalizers if they do not.
delete_and_wait() {
	local timeout="$1" type="$2"
	shift 2
	k delete "${type}" "$@" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	local deadline=$(($(date +%s) + timeout))
	while k get "${type}" "$@" -o name 2>/dev/null | grep -q .; do
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			e2e::warn "${type} $* still exists after ${timeout}s; removing finalizers"
			strip_finalizers "${type}" "$@"
			k delete "${type}" "$@" --ignore-not-found --wait=false >/dev/null 2>&1 || true
			break
		fi
		sleep 2
	done
}

crossplane_ours=false
if k get namespace "${E2E_NS_CROSSPLANE}" >/dev/null 2>&1; then
	crossplane_ours=true # e2e::foreign_crossplane checked the label.
elif ! k get deployment -A -l app=crossplane -o name 2>/dev/null | grep -q .; then
	# No Crossplane at all: leftovers of Crossplane (CRDs, RBAC) can only be
	# from an interrupted cleanup.
	crossplane_ours=true
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
	crd_exists managedresourceactivationpolicies.apiextensions.crossplane.io &&
		delete_and_wait 30 managedresourceactivationpolicies.apiextensions.crossplane.io --all
	if e2e::helm status crossplane -n "${E2E_NS_CROSSPLANE}" >/dev/null 2>&1; then
		e2e::helm uninstall crossplane -n "${E2E_NS_CROSSPLANE}" --wait --timeout 5m >/dev/null
	fi
fi

# CRDs: the provider's (unless resources outside the test namespaces use
# them) and, if the harness installed Crossplane, Crossplane's.
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
	*.crossplane.io)
		[ "${crossplane_ours}" = true ] && crds="${crds} ${crd}"
		;;
	esac
done
if [ -n "${crds}" ]; then
	e2e::log "removing CRDs:${crds}"
	for crd in ${crds}; do
		# Instances with finalizers block the CRD's deletion once no
		# controller runs any more.
		# A CRD can disappear meanwhile: deleting the activation definitions
		# removes the CRDs they own.
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

if [ -n "${test_namespaces}" ]; then
	e2e::log "removing the namespaces: $(printf '%s\n' "${test_namespaces}" | tr '\n' ' ')"
	# shellcheck disable=SC2086 # One namespace per word.
	k delete namespace ${test_namespaces} --ignore-not-found --wait=false >/dev/null
	for ns in ${test_namespaces}; do
		k wait --for=delete "namespace/${ns}" --timeout=300s >/dev/null 2>&1 || e2e::warn "namespace ${ns} is still terminating"
	done
fi

if [ "${crossplane_ours}" = true ]; then
	# Crossplane's RBAC manager and Helm chart create these; the chart's are
	# removed by helm uninstall, the RBAC manager's are not.
	for type in clusterroles clusterrolebindings validatingwebhookconfigurations mutatingwebhookconfigurations; do
		for obj in $(k get "${type}" -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' | grep '^crossplane' || true); do
			k delete "${type}" "${obj}" --ignore-not-found >/dev/null
		done
	done
	# Events about cluster-scoped objects (Providers, revisions, activation
	# policies, CRDs, ClusterProviderConfigs) are recorded in "default".
	for ev in $(k get events -n default -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.involvedObject.apiVersion}{"|"}{.involvedObject.name}{"|"}{.source.component}{.reportingComponent}{"\n"}{end}' |
		grep -E 'crossplane\.io|infrabay\.io|clickhouse|crossplane' | cut -d'|' -f1); do
		k delete event -n default "${ev}" --ignore-not-found >/dev/null 2>&1 || true
	done
fi
# Some controllers record events about objects of the test namespaces
# without a namespace, so they land in "default" too (for example the
# endpoint controller about a Service of a namespace being deleted); only
# their message names the namespace.
for ev in $(k get events -n default -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.message}{"\n"}{end}' |
	grep -E '^[^|]*\|.*xpch-e2e-' | cut -d'|' -f1); do
	k delete event -n default "${ev}" --ignore-not-found >/dev/null 2>&1 || true
done

# On OrbStack the node's container runtime is the host's Docker engine: drop
# the runtime images it pulled from the registry. Built images stay.
if [ "${E2E_KUBE_CONTEXT}" = "orbstack" ] && command -v docker >/dev/null 2>&1; then
	for img in $(docker images --format '{{.Repository}}:{{.Tag}}' "localhost:${E2E_REGISTRY_PORT}/${E2E_PROVIDER}" 2>/dev/null); do
		docker image rm "${img}" >/dev/null 2>&1 || true
	done
fi

e2e::log "checking for leftovers"
leftovers=""
add() { leftovers="${leftovers}
  $*"; }
for ns in $(k get namespaces -l "${E2E_LABEL}" -o name); do add "${ns}"; done
for crd in $(k get crd -o name | grep -E "\.${E2E_GROUP//./\\.}\$" || true); do add "${crd}"; done
if [ "${crossplane_ours}" = true ]; then
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
