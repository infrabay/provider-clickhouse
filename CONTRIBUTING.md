# Contributing

Thanks for your interest in improving `provider-clickhouse`.

## Development

See [`DEVELOPMENT.md`](DEVELOPMENT.md) for the full setup. In short:

```sh
make generate          # after changing anything in apis/
make test              # unit tests (no network, no Docker)
make lint              # golangci-lint
make envtest           # CRD validation against a real kube-apiserver
make test-integration  # ClickHouse containers (Docker)
```

`make reviewable` runs what CI checks first. Please make sure it passes before
opening a pull request.

## Pull requests

- Keep the change focused, and add or update tests for behaviour changes.
- Follow [`docs/design.md`](docs/design.md). A change that departs from it
  updates the design in the same pull request.
- Unit tests must not need the network, Docker or a cluster. Behaviour that
  depends on a real server belongs in `test/integration` (build tag
  `integration`); CRD validation rules belong in `test/envtest`.
- Build every SQL statement with the quoting helpers in
  `internal/clients/clickhouse`, and run reads with server-side parameters.
  Never split names on `@` or `.`.
- Never log or return passwords, password hashes, salts or tokens. Tests
  assert this for every new statement that carries a secret.
- Commit the output of `make generate` (CRDs and generated Go files).
- New files carry the SPDX header from `hack/boilerplate.go.txt`. Files
  adapted from other Apache-2.0 projects keep their original header and say
  that they were modified (see [`NOTICE`](NOTICE)).
- Examples and docs use generic names (`example.com`, `my-project`,
  `my-cluster`), never real organisations, hosts or project IDs.

## Reporting issues

Please include the provider version, the Crossplane version, the ClickHouse
server version, the managed resource (with secrets removed) and its status
conditions and events.

Report security vulnerabilities privately, as described in
[`SECURITY.md`](SECURITY.md), not in a public issue.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
