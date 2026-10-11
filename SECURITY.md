# Security policy

## Supported versions

Security fixes are made on `main` and released in a new patch version of the
latest minor release. Older minor versions do not receive fixes; upgrade to
the latest release to pick them up.

| Version              | Supported |
|----------------------|-----------|
| latest minor (0.x.y) | yes       |
| older minors         | no        |

## Reporting a vulnerability

Please **do not** open a public issue, pull request or discussion for a
security problem.

Report it privately through GitHub's private vulnerability reporting: open
the repository's
[Security tab](https://github.com/infrabay/provider-clickhouse/security) and
choose **Report a vulnerability**
([direct link](https://github.com/infrabay/provider-clickhouse/security/advisories/new)).

Please include:

- the provider version, the Crossplane version and the ClickHouse server
  version involved;
- a description of the issue and its impact;
- the steps or minimal manifests that reproduce it, with passwords, tokens,
  keys and other secrets removed.

Reports are acknowledged as soon as possible. Once the issue is confirmed, a
fix is prepared in a private security advisory and released, and the
advisory is published with the fixed version, crediting the reporter unless
they prefer otherwise.

## Scope

This policy covers the code in this repository: the provider binary, its
Crossplane package and the release artifacts. Vulnerabilities in ClickHouse,
Crossplane or other dependencies should be reported to those projects; if one
affects this provider, a report here is still welcome so the dependency can
be updated.

The security model, including what the provider protects against and what it
relies on Kubernetes RBAC and server-side grants for, is described in
[section 9 of the design](docs/design.md#9-security-model). Behaviour
documented there (for example that ownership annotations are writable by
anyone who can edit a managed resource) is not a vulnerability by itself.
