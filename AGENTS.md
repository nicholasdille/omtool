# AGENTS.md

Development workflow notes for anyone (human or agent) working on this repo.

## Project layout

- `cmd/omtool/` — the entire CLI (single Go module, single binary). Each
  subcommand has its own file (`convert.go`, `send.go`, `validate.go`) plus
  `metricsio.go` for shared OpenMetrics/protobuf parsing, and a `_test.go`
  next to each source file.
- `main.go` wires subcommands onto the cobra root command in `newRootCmd()`.

## Build

```sh
go build -o omtool ./cmd/omtool
```

## Test

```sh
go test -cover ./...
```

Add unit tests next to the code they cover (`foo.go` -> `foo_test.go`),
following the existing table-driven style.

## Lint / vet

```sh
go vet ./...
golangci-lint run ./...
```

Both must be clean before committing; CI (`.github/workflows/tests.yml`)
runs `go vet`, `golangci-lint`, and `go test -cover` on every push, plus a
GoReleaser snapshot build.

## Adding a subcommand

1. Create `cmd/omtool/<name>.go` with a `new<Name>Cmd() *cobra.Command`
   constructor and a `run<Name>(...)` function that does the work (kept
   separate from the cobra wiring so it's easy to unit test).
2. Register it in `newRootCmd()` in `main.go`.
3. Reuse `openInput`/`openOutput`/`loadFamilies`/`parseOpenMetrics` from
   `main.go`/`metricsio.go` instead of duplicating I/O or parsing logic.
4. Add a `cmd/omtool/<name>_test.go` and a README section.

## Release

Releases are tag-driven: pushing a `v*` tag triggers `.github/workflows/
release.yml`, which runs GoReleaser (see `.goreleaser.yml`) to build and
publish binaries with SBOM/provenance attestation. `auto-patch-release.yml`
can be triggered manually to cut a patch release when all commits since
the last tag are bot-authored (e.g. Renovate dependency bumps).
