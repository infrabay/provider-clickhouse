# fakegcp manifests for end-to-end tests

Manifests that run the fake GCE metadata server and Google JWKS of
[`test/fakegcp`](..) in a Kubernetes cluster, so that an end-to-end test can
exercise the provider's `GoogleIDToken` credentials (Application Default
Credentials, as on GKE with Workload Identity) and ClickHouse's
`jwt_dynamic_jwks` processor without Google. They are not wired into
`test/e2e` yet. See [docs/testing-google-tokens.md](../../../docs/testing-google-tokens.md)
for what the fake simulates and what it does not.

**Test clusters only.** The control API mints tokens for any identity, and
whatever trusts the fake's JWKS trusts those tokens.

| File | What |
|---|---|
| `fakegcp.yaml` | Namespace `fakegcp`, Deployment (distroless, non-root, read-only root file system), Service `fakegcp` on port 80 (metadata server and `/oauth2/v3/certs`) and Service `fakegcp-control` on 8081 (control API). |
| `clickhouse-google-jwt.yaml` | ConfigMap with the `google_jwt` processor (`jwks_uri` = the fake) and the provider's service account as a users.d jwt user pinned to it. Mount the keys into `config.d` and `users.d` of an Antalya 26.6 server. |
| `provider-runtime.yaml` | `DeploymentRuntimeConfig` that sets `GCE_METADATA_HOST` for the provider, and a `ClusterProviderConfig` with `GoogleIDToken` credentials and the matching audience. |

## Use

```console
# Build the image from the repository root and make it available to the
# cluster (OrbStack uses local images; for kind: kind load docker-image fakegcp:dev).
# With a docker-container buildx builder, add --load so the image lands in the
# local image store. The build needs no module downloads (GOPROXY=off).
docker build -f test/fakegcp/Dockerfile -t fakegcp:dev .

kubectl apply -f test/fakegcp/deploy/fakegcp.yaml
kubectl -n fakegcp rollout status deploy/fakegcp

# ClickHouse: apply the ConfigMap in its namespace and mount it (see the file).
kubectl -n clickhouse apply -f test/fakegcp/deploy/clickhouse-google-jwt.yaml

# Provider: reference the runtime config from the Provider
# (spec.runtimeConfigRef.name: provider-clickhouse-fakegcp), then apply the
# ClusterProviderConfig.
kubectl apply -f test/fakegcp/deploy/provider-runtime.yaml
```

Names to keep consistent if you change them: the Service DNS name
(`fakegcp.fakegcp.svc.cluster.local`) appears in `GCE_METADATA_HOST` and in
`jwks_uri`; the audience (`https://clickhouse-e2e.example.com`) in the
processor and in the `ClusterProviderConfig`; the service account email in the
Deployment's `--email` and in users.d. Unit tests in `test/fakegcp` keep them
honest: `TestDeployManifests` decodes every manifest strictly into its
Kubernetes or provider type and checks that these names agree (and that the
ClusterProviderConfig's policy compiles and allows the processor), and
`TestProcessorYAML` checks that the processor in the ConfigMap is the one
`fakegcp.GoogleProcessor.YAML` renders.

## Driving the fake from a test

Port-forward the control API (`kubectl -n fakegcp port-forward svc/fakegcp-control 8081`)
and call it with JSON:

```console
curl -s localhost:8081/fakegcp/v1/state
curl -s -XPOST localhost:8081/fakegcp/v1/fault  -d '{"fault":"expired","count":0}'
curl -s -XPOST localhost:8081/fakegcp/v1/outage -d '{"target":"identity","mode":"500"}'
curl -s -XPOST localhost:8081/fakegcp/v1/keys   -d '{"action":"rotate"}'
curl -s -XPOST localhost:8081/fakegcp/v1/mint   -d '{"audience":"https://clickhouse-e2e.example.com","email":"app-api@example-project.iam.gserviceaccount.com","uniqueId":"100000000000000000202"}'
curl -s -XPOST localhost:8081/fakegcp/v1/reset
```

Faults: `wrong-audience`, `wrong-issuer`, `email-unverified`, `no-email`,
`wrong-typ`, `no-typ`, `unknown-kid`, `bad-signature`, `alg-none`,
`hs256-confusion`, `expired`, `issued-in-future`. Outage targets: `metadata`
(every metadata value), `identity` (ID and access tokens only), `jwks`; modes
`500`, `503`, `403` (like a missing Workload Identity binding) and `timeout`.
Settings: `{"ttl": "5m", "clockOffset": "-90s"}` at `/fakegcp/v1/settings`.
The full list is in the doc comment of `ControlHandler`.

Keys, faults and outages are in memory: keep one replica, and expect a
restart to generate new keys (ClickHouse picks them up after
`jwks_cache_lifetime`, 60 s here).
