#!/usr/bin/env bash
# Copyright 2026 The provider-clickhouse Authors
# SPDX-License-Identifier: Apache-2.0
#
# Writes diagnostics of a failed end-to-end run into the directory $1: the
# Provider and its revisions, activation policies and definitions, the
# provider's and Crossplane's logs, every managed resource and ProviderConfig
# of the test namespaces with their conditions, events, pods, and the
# ClickHouse server logs. Secrets are listed by name, type and keys only, never
# with their data, and every secret value the harness generated is replaced
# with [REDACTED] in all files.
set -uo pipefail

# shellcheck source=hack/e2e/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

out="${1:?usage: diagnostics.sh <directory>}"
e2e::guard
mkdir -p "${out}"
k() { e2e::kubectl "$@"; }

# dump writes the output of a kubectl command to a file, errors included.
dump() {
	local file="$1"
	shift
	k "$@" >"${out}/${file}" 2>&1 || true
}

dump nodes.txt get nodes -o wide
dump packages.yaml get providers.pkg.crossplane.io,providerrevisions.pkg.crossplane.io,deploymentruntimeconfigs.pkg.crossplane.io -o yaml
dump packages.txt get providers.pkg.crossplane.io,providerrevisions.pkg.crossplane.io -o wide
dump activation.yaml get managedresourceactivationpolicies.apiextensions.crossplane.io,managedresourcedefinitions.apiextensions.crossplane.io -o yaml
dump activation.txt get managedresourceactivationpolicies.apiextensions.crossplane.io,managedresourcedefinitions.apiextensions.crossplane.io
dump crds.txt get crd
dump clusterproviderconfigs.yaml get "clusterproviderconfigs.${E2E_GROUP}" -o yaml

namespaces="$(k get namespaces -l "${E2E_LABEL}" -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{end}')"
mrs=""
for kind in ${E2E_MR_KINDS} providerconfigs providerconfigusages; do mrs="${mrs:+${mrs},}${kind}.${E2E_GROUP}"; done
for ns in ${namespaces}; do
	dump "ns-${ns}-pods.txt" get pods -n "${ns}" -o wide
	dump "ns-${ns}-describe-pods.txt" describe pods -n "${ns}"
	dump "ns-${ns}-events.txt" get events -n "${ns}" --sort-by=.lastTimestamp
	dump "ns-${ns}-resources.txt" get "${mrs}" -n "${ns}" -o wide
	dump "ns-${ns}-resources.yaml" get "${mrs}" -n "${ns}" -o yaml
	# Secrets: name, type and keys, never data.
	# shellcheck disable=SC2016 # A Go template, not a shell expansion.
	k get secrets -n "${ns}" -o 'go-template={{range .items}}{{.metadata.name}} {{.type}} keys:{{range $k, $v := .data}} {{$k}}{{end}}{{"\n"}}{{end}}' \
		>"${out}/ns-${ns}-secrets.txt" 2>&1 || true
done
dump events-default.txt get events -n default --sort-by=.lastTimestamp

ns="${E2E_NS_CROSSPLANE}"
dump provider.log logs -n "${ns}" -l "pkg.crossplane.io/provider=${E2E_PROVIDER}" --all-containers --tail=-1 --prefix
dump provider-previous.log logs -n "${ns}" -l "pkg.crossplane.io/provider=${E2E_PROVIDER}" --all-containers --tail=-1 --prefix --previous
dump crossplane.log logs -n "${ns}" -l app=crossplane --all-containers --tail=-1 --prefix
dump crossplane-rbac-manager.log logs -n "${ns}" -l app=crossplane-rbac-manager --all-containers --tail=-1 --prefix
for server in upstream antalya; do
	dump "clickhouse-${server}.log" logs -n "${E2E_NS_CLICKHOUSE}" "deployment/${server}" --tail=2000
	# The server logs to files; the error log has the failed statements (the
	# server masks passwords in it).
	dump "clickhouse-${server}.err.log" exec -n "${E2E_NS_CLICKHOUSE}" "deployment/${server}" -- tail -n 1000 /var/log/clickhouse-server/clickhouse-server.err.log
done
dump registry.log logs -n "${E2E_NS_REGISTRY}" deployment/registry --tail=500

e2e::redact "${out}"/*
echo "diagnostics written to ${out}" >&2
