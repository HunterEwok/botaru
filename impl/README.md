# impl: the reference implementation in Go

This folder holds the reference implementation of the botaru core (D-91, D-92 in [docs/decisions.md](../docs/decisions.md)). Today it is only a module skeleton: the command `botaru-core` prints its version, and the work on the core has not started.

## Rules for this folder

- The implementation does not change skeleton files outside `impl/`. A change of a contract goes through the skeleton rules: additive only inside a version (MAN-3), a new schema file `.v(N+1)` for a breaking change.
- It passes the `conformance/` suite and the evals of the skeleton; a rule with `waivable: false` cannot go into its list of expected failures.
- The agent runtime is our own loop over P4 (D-93); conversation memory, sessions and transcript compression live in the core stores.
- Money is an integer number of micro-euros `eur_micros` (D-87), never a floating-point number.
- Builds are static (`CGO_ENABLED=0`) for linux/amd64 and linux/arm64. Installation files (tenants, keys, machines, people) never go into this repository.
- Every Go file starts with `// SPDX-License-Identifier: AGPL-3.0-only`.

## Checks

The workflow `.github/workflows/go.yml` runs on every pull request. Locally, from this folder:

```sh
gofmt -l .
go mod tidy -diff
go vet ./...
go tool staticcheck ./...
go test -race ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
go tool govulncheck ./...
```

The `go` command downloads the toolchain named in `go.mod` by itself (`GOTOOLCHAIN=auto`). staticcheck and govulncheck are tool dependencies in `go.mod`, so their versions are pinned and Dependabot updates them.
