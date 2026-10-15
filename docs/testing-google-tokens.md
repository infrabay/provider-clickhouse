# Testing Google ID tokens without Google

The `GoogleIDToken` credentials source without a key (design 6.1) uses
Application Default Credentials. On GKE with Workload Identity that means the
GKE metadata server, which mints an ID token for the pod's Google service
account. ClickHouse (Altinity Antalya) validates the token with a
`jwt_dynamic_jwks` token processor against Google's public keys. Neither
endpoint is available in CI or in a local cluster, so the tests use a fake of
both: [`test/fakegcp`](../test/fakegcp).

```
provider ──GCE_METADATA_HOST──▶ fakegcp: /computeMetadata/v1/instance/service-accounts/default/identity
   │                                      (raw JWT, RS256, signed with a throwaway key)
   └── native protocol, token as password ──▶ ClickHouse ──jwks_uri──▶ fakegcp: /oauth2/v3/certs
```

No provider code changes are needed: the Google client libraries send every
metadata request to `GCE_METADATA_HOST` when it is set.

## What is simulated

**Metadata server** (`fakegcp.Server.MetadataHandler`), modelled on the GCE and
GKE metadata servers:

- `GET /` answers without any request header and with `Metadata-Flavor: Google`,
  as the real server does for GCE detection. Every response carries it. The
  Go libraries never call it in these tests, though: see GCE detection under
  "What is not simulated".
- Requests below `/computeMetadata/v1/` need `Metadata-Flavor: Google` (403
  otherwise). Requests with `X-Forwarded-For` are refused (403). Only `GET` and
  `HEAD` are allowed.
- Values: `project/project-id`, `project/numeric-project-id`, `instance/id`,
  `instance/name`, `instance/hostname`, `instance/zone`,
  `universe/universe-domain`, and for the service account (`default` or its
  email) `email`, `aliases`, `scopes`, `token` (an opaque access token in the
  real JSON shape, accepted by nothing) and `identity`. Directories list their
  entries, a directory without its trailing slash redirects (301), service
  account directories support `?recursive=true`, values support `?alt=json`,
  unknown paths are 404.
- `identity` requires `audience` (400 otherwise) and accepts
  `format=standard|full` (default `standard`) and `licenses=TRUE|FALSE`. The
  body is the raw JWT.

**ID tokens.** Header `{"alg":"RS256","kid":"<40 hex digits>","typ":"JWT"}`.
Claims `iss` (`https://accounts.google.com`), `aud`, `azp` and `sub` (the
service account's unique ID), `iat`, `exp` (1 h later), and with
`format=full` `email` and `email_verified: true`. By default the fake behaves
like the GKE metadata server with Workload Identity, whose tokens are plain
service account ID tokens; with `Options.ComputeEngine` it behaves like a
Compute Engine VM and adds `google.compute_engine` (and `license_id` with
`licenses=TRUE`) to `format=full` tokens. Google's own Go validator
(`idtoken.Validator`) accepts the fake's tokens against the fake's JWKS
(`TestGoogleLibraries`).

**JWKS** at `/oauth2/v3/certs`: `{"keys":[{"alg":"RS256","e":"AQAB","kid":…,"kty":"RSA","n":…,"use":"sig"}]}`
with `Cache-Control: public, max-age=21600, must-revalidate, no-transform`.

**Test controls**, as Go methods and as an HTTP control API that is served on
its own listener only (`ControlHandler`):

| Control | Effect |
|---|---|
| `SetIdentity` | Serve tokens for another service account: an attacker, or a recreated account with the same email and a new unique ID. |
| `SetTTL`, `SetClockOffset` | Token lifetime; skew between the metadata server's clock and ClickHouse's. |
| `AddKey`, `Activate`, `Rotate`, `Retire` | Publish a key ahead of use (as Google does), sign with it, rotate without a head start, remove a key from the JWKS. |
| `SetFault(f, n)` | The next `n` tokens (or all, `n <= 0`) are defective: `wrong-audience`, `wrong-issuer`, `email-unverified`, `no-email`, `wrong-typ` (`at+jwt`), `no-typ`, `unknown-kid`, `bad-signature`, `alg-none`, `hs256-confusion` (HMAC with the RSA public key as secret), `expired`, `issued-in-future`. |
| `SetOutage(target, mode, n)` | `metadata` (every value), `identity` (ID and access tokens) or `jwks` fail with `500`, `503`, `403` (a multi-line message, like a missing Workload Identity binding) or `timeout` (no answer until the client gives up). |
| `Mint` | Sign a token directly, for example for a workload that logs in as a jwt user. |
| `Requests`, `Issued` | What was requested, and summaries of issued tokens (key ID and claims, never the token). |

Logs contain key IDs and claim summaries only, never tokens or keys.

## What is not simulated

- **GCE detection.** With `GCE_METADATA_HOST` set, `metadata.OnGCE()` returns
  true at once; without it, the probe goes to the hard-coded
  `http://169.254.169.254` and a DNS lookup of `metadata.google.internal`,
  never to the fake. Every test sets `GCE_METADATA_HOST` (`fakegcp.UseFake`,
  and the deploy manifests for end-to-end tests), so detection is bypassed
  everywhere and the fake's `GET /` handler is never used by the libraries
  (`TestGoogleLibraries` fails on any request to it). A provider bug that only
  breaks detection on real GKE would not show up, for example an early
  `OnGCE` call with a cancelled context, which memoizes false for the life
  of the process.
- **The real GKE metadata server**: the Workload Identity binding
  (`iam.gke.io/gcp-service-account`, `roles/iam.workloadIdentityUser`), the
  per-pod identity, its latency, rate limits and caching, the exact text of
  its errors, and whether it adds anything to `format=full` tokens. The fake
  serves one identity to every client. `wait_for_change` is answered with
  400, and most directories do not support `recursive`.
- **Real Google key rotation**: when Google publishes, starts using and
  retires keys, the real `max-age`, and the `iss` variant without `https://`
  that some Google tokens carry. The tests rotate keys on command.
- **TLS to Google**: the fake's JWKS is plain HTTP. ClickHouse 26.6 fetches
  `http://` and `https://` `jwks_uri` alike (`JWKSClient::getJWKS`), so no CA
  is needed; production uses `https://www.googleapis.com/oauth2/v3/certs`.
- **The service account key path** (`credentialsSecretRef`), which exchanges a
  signed assertion at `https://oauth2.googleapis.com/token`. The provider
  refuses any other `token_uri` (design 6.1), so it cannot be pointed at a
  fake; it is covered by unit tests only.
- **Network policy and egress**: whether ClickHouse may reach
  `www.googleapis.com` and whether the provider may reach the metadata server.

## Behaviour the tests pin

Library versions: `google.golang.org/api` v0.301.0, `golang.org/x/oauth2`
v0.37.0, `cloud.google.com/go/compute/metadata` v0.10.0,
`cloud.google.com/go/auth` v0.24.1-0.20261001053825-dbc26066f70a. Server:
`altinity/clickhouse-server:26.6.4.20001.altinityantalya`.

**Client libraries** (read in the module sources, confirmed by the request
log of the tests):

- With `GCE_METADATA_HOST` set, `metadata.OnGCE()` is true without probing
  `169.254.169.254` or DNS; the result is memoized per process. Requests go to
  `http://$GCE_METADATA_HOST/computeMetadata/v1/…` with
  `Metadata-Flavor: Google`.
- Without a key, `idtoken.NewTokenSource` (default path, `golang.org/x/oauth2/google`)
  looks at `GOOGLE_APPLICATION_CREDENTIALS`, then gcloud's
  `~/.config/gcloud/application_default_credentials.json`, then the metadata
  server. It reads `project/project-id`, then fetches
  `instance/service-accounts/default/identity?audience=<audience>&format=full`
  at once (in `NewTokenSource`). Nothing else is requested, and no other Google
  endpoint is contacted. Tests call `fakegcp.UseFake`, which also hides the
  first two sources so a developer's own credentials are never used.
- With `GOOGLE_API_GO_EXPERIMENTAL_ENABLE_NEW_AUTH_LIB=true` the
  `cloud.google.com/go/auth` path fetches lazily and requests the same URL.
  Because the requests are identical, the tests check which library is in
  use: the new one returns an `oauth2adapt` token source
  (`fakegcp.UsesNewAuthLibrary`) and requests no token in `NewTokenSource`
  (`TestGoogleLibraries`, and a precondition of `NewAuthLibrary`).
- The default path caches a metadata ID token for 55 minutes (the new path:
  1 h, refreshed 5 min early) **whatever its `exp`** (`TestGoogleLibraries`
  checks the expiry the libraries report for a 5-minute token). Google's
  tokens live 1 h, so this only matters for shorter tokens: the provider
  keeps sending a short-lived token on new connections until ClickHouse
  refuses it, and the 516 retry (design 6.4) then fetches a fresh one
  (`ShortLivedTokenIsReplacedVia516`).
- The provider's `GoogleSource` caches a token only until the expiry the
  library reports (oauth2 refreshes 10 s early), without waiting for a 516
  (`TestGoogleSourceHonoursExpiry`). The integration tests cannot see this:
  the libraries' fixed 55 min to 1 h is longer than any test, so a source
  that cached its first token forever would pass them all.
- The metadata client gives each attempt 5 s and retries 5xx, 429 and timeouts
  five times with jittered backoff from 100 ms. A failing metadata server
  costs 1.5 to 3 s; a hanging one about 31 s (measured in two runs: 1.4 to
  1.6 s and 31.4 to 32.1 s). The provider's token source
  does not pass the reconcile context to the library, and the token is
  fetched after the TCP connection to ClickHouse is open, so neither the 5 s
  dial timeout nor the reconcile timeout bounds it. ClickHouse keeps the
  connection open meanwhile (`SlowMetadataServer` waits 10 s; 15 s also
  worked in a manual run).
- Metadata errors embed the response body (`compute: Received 500 \`…\``). The
  provider keeps the first line only (at most 256 characters): the managed
  resource gets `AuthenticationFailed` with `token source failed: failed to get
  JWT: GoogleIDToken: …`. A timeout's message contains the metadata URL with
  the audience, which is not secret.

**ClickHouse** (`jwt_dynamic_jwks`, confirmed by the integration tests and
read in `src/Access/TokenProcessorsJWT.cpp`, `Common/JWKSProvider.cpp` and
`ExternalAuthenticators.cpp` of the `antalya-26.6` branch):

- Every faulty token above is refused with `516 Token is invalid`. `alg-none`
  and `hs256-confusion` fail because the processor only verifies RS/ES
  algorithms and checks a JWK's `alg`; `wrong-typ`/`no-typ` because of
  `expected_typ: JWT`; `email-unverified` because of
  `claims: '{"email_verified": true}'`; `no-email` because `username_claim`
  is missing.
- A valid Google token for a service account that is not a ClickHouse user is
  refused (516): without `user_directories/token` nothing creates users.
- The leeway for `exp` and `iat` is `verifier_leeway`, 60 s unless
  configured (read in `TokenProcessorsParse.cpp`). The tests bound it: a
  metadata clock 30 s off works, 2 min ahead or 62 min behind gives 516.
- An unknown `kid` does not trigger a JWKS refetch: a new key is accepted only
  after `jwks_cache_lifetime`. A key published before it is used (Google's
  practice) switches without a failure (`KeyRotation`).
- When the cached JWKS has expired and the refetch fails, the login that
  triggered it gets 516, and the stale set is used until the next attempt
  after `jwks_cache_lifetime` (`JWKSOutage`). If no fetch has ever succeeded,
  every login fails until one does, and a fetch is tried at most once per
  `jwks_cache_lifetime` (read in the source, not tested).
- An accepted token is cached for `token_cache_lifetime` (at most 1 h), not
  until its `exp`: a token that expires after it was accepted keeps working
  until the cache entry expires (`ServerTokenCacheIgnoresExp`). Keep
  `token_cache_lifetime` short.
- Upstream ClickHouse (25.3.3) has no JWT login: the provider's token is sent
  as the password of user `" JWT AUTHENTICATION "` and refused with 516; the
  provider retries once with a fresh token and reports `AuthenticationFailed`
  (`TestGoogleIDTokenUpstream`).

**Operational advice that follows**: pin each Google service account user to
the processor and, with `CLAIMS '{"sub":"<unique ID>"}'`, to the account's
unique ID, because a deleted service account's email can be reused by a new
account; keep `token_cache_lifetime` short; allow ClickHouse egress to
`www.googleapis.com` and alert on JWKS fetch failures.

## Production processor

Tests, docs and the Kubernetes manifests render the processor with
`fakegcp.GoogleProcessor.YAML`. Production (`fakegcp.ProductionGoogleProcessor`)
differs from the tests only in `jwks_uri` and in the cache lifetimes, which
the tests shorten to 10 s so that rotation and expiry happen within seconds.
`TestProcessorYAML` checks that this block, and the one in the design's
server configuration examples (Appendix B), match the renderer:

```yaml
token_processors:
  google_jwt:
    type: jwt_dynamic_jwks
    jwks_uri: "https://www.googleapis.com/oauth2/v3/certs"
    expected_issuer: "https://accounts.google.com"
    expected_audience: "https://clickhouse.example.com"
    expected_typ: JWT
    claims: '{"email_verified": true}'
    username_claim: email
    jwks_cache_lifetime: 3600
    token_cache_lifetime: 300
```

Every validation key matters. A processor with only `expected_issuer`,
`expected_audience` and `username_claim` (an earlier version of the design's
example) accepted tokens with `typ` `at+jwt`, without `typ` and with
`email_verified: false` on 26.6.4 (checked once by hand against the fake);
wrong issuers and audiences were still refused.

Users log in through it as SQL users
(`IDENTIFIED WITH jwt PROCESSOR 'google_jwt' CLAIMS '{"sub":"…"}'`) or as
users.d users with `jwt: {processor: google_jwt}`. Do not enable
`user_directories/token`: any Google service account can mint a token for any
audience, and the list of jwt users is the only allow-list. The `google`
processor type (opaque access tokens) is known not to work in 26.6.4 (not
re-tested here); use `jwt_dynamic_jwks`.

## Running the tests

Unit tests (no Docker; the fake listens on localhost):

```console
go test -race ./test/fakegcp/... ./internal/clients/clickhouse/auth/...
```

Besides the fake itself they run `TestGoogleLibraries` (the Google client
libraries and Google's own `idtoken.Validator` against the fake, which library
is in use, and the libraries' fixed token expiry),
`TestGoogleSourceWithMetadataServer` (the provider's `GoogleSource`: caching,
`Invalidate`, retries, scrubbed errors), `TestGoogleSourceHonoursExpiry`
(`GoogleSource` drops a token at the expiry the library reports),
`TestProcessorYAML` (the processor in this document, in the design and in the
ConfigMap is the rendered one) and `TestDeployManifests` (the Kubernetes
manifests decode strictly into their Kubernetes and provider types and agree
on the Service name, audience, service account and processor).

Integration tests (Docker; the containers reach the test process as
`host.docker.internal`):

```console
TESTCONTAINERS_RYUK_DISABLED=true go test -tags integration -count=1 -run TestGoogleIDToken -v ./test/integration/
```

`make test-integration` runs them with every other integration test. The
Antalya test takes about two minutes, mostly waiting for cache lifetimes and
the metadata client's timeouts. It supports Antalya 26.6.4 (the default) and
26.3.13 (the CI job "antalya 26.3"); see "Open points" for one unexplained
failure on 26.3.13.

When a test fails, the server's token processor log lines from
`system.text_log` are logged (JWTs redacted), for example
`google_jwt: Failed to validate JWT: Connection refused` for a JWKS fetch that
never reached the fake. The fake's request log shows only the fetches that
did.

| Variable | Default | Meaning |
|---|---|---|
| `XPCH_ANTALYA_IMAGES` | `altinity/clickhouse-server:26.6.4.20001.altinityantalya` | Antalya images for `TestGoogleIDTokenAntalya` |
| `XPCH_GOOGLE_UPSTREAM_IMAGES` | `clickhouse/clickhouse-server:25.3.3.42-alpine`, or none if `XPCH_UPSTREAM_IMAGES=none` | Upstream images that must fail cleanly; the Antalya-only CI jobs set `XPCH_UPSTREAM_IMAGES=none` and so skip `TestGoogleIDTokenUpstream` |
| `XPCH_FAKEGCP_BIND` | `127.0.0.1` on macOS and Windows, `0.0.0.0` on Linux | Address of the fake's JWKS listener; containers reach it through `host.docker.internal` (the metadata listener is always on `127.0.0.1`) |
| `XPCH_FAKEGCP_LOG` | unset | `1` logs the fake's requests and issued tokens (key IDs and claims only) |

| Test | Shows |
|---|---|
| `ProviderLogsInAsServiceAccount` | The provider logs in as the service account email through its own connection code; one `format=full` token with the configured audience; no other metadata requests. |
| `DatabaseLifecycle`, `JWTUserForAServiceAccount` | Real reconciles of a `Database` and of a jwt `User` for a workload service account (pinned processor and `sub`); the workload logs in; a recreated account, an attacker and another audience are refused. |
| `FaultyTokensAreRejected` | Every fault gives 516, one invalidate and exactly one fresh token, and an error without token material. |
| `AttackerServiceAccount`, `ClockSkew` | See above. |
| `KeyRotation` | Pre-published key: no failure. Immediate rotation: 516 until `jwks_cache_lifetime`, then accepted. Retired key: refused after the refresh. |
| `ShortLivedTokenIsReplacedVia516`, `ServerTokenCacheIgnoresExp` | Token expiry on the provider side (exactly one fresh token replaces an expired one: through the 516 retry with today's libraries, before the dial if a library reported `exp`) and on the server side. |
| `MetadataOutage`, `SlowMetadataServer`, `JWKSOutage` | 500, 403 and hanging metadata server: clean `AuthenticationFailed`, recovery; slow server; JWKS outage. |
| `NewAuthLibrary` | The `cloud.google.com/go/auth` path, after checking that the switch took effect. |
| `TestGoogleIDTokenUpstream` | Upstream without JWT support fails cleanly. |

## Open points

- **One intermittent failure on Antalya 26.3.13.** In one full run,
  `SlowMetadataServer` got 516 after about 10 s, and `JWKSOutage` and
  `NewAuthLibrary` got 516 within milliseconds after it, without a JWKS fetch
  reaching the fake; the run's cleanup then took about 15 minutes before the
  container was stopped. It did not happen in six other full runs or in
  several subset runs. The pattern fits JWKS fetches that failed on the
  network, for example while another workload was using the same Docker
  host, but the cause is unknown. Failures now log the server's token
  processor lines, which tell a network failure (`Connection refused`,
  timeouts) from a refused token. Not changed: a bound on the shared
  harness's container cleanup, because it is unknown which cleanup step hung,
  and `go test -timeout` already bounds the run.

## Running the fake elsewhere

```console
go run ./test/fakegcp/cmd/fakegcp --listen 127.0.0.1:8080 --control-listen 127.0.0.1:8081 \
  --email provider@example-project.iam.gserviceaccount.com
GCE_METADATA_HOST=127.0.0.1:8080 <program using Application Default Credentials>
```

The image (`docker build -f test/fakegcp/Dockerfile -t fakegcp:dev .`, distroless,
non-root) and the Kubernetes manifests for end-to-end tests are described in
[test/fakegcp/deploy/README.md](../test/fakegcp/deploy/README.md).
