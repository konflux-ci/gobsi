# gobsi

`gobsi` builds [OCI](https://github.com/opencontainers/image-spec) **source-container
images** — images whose layers carry the *source* (SRPMs and arbitrary source
directories) that a binary artifact was built from, rather than a runnable
filesystem.

It is a Go rewrite of the [BuildSourceImage](https://github.com/containers/BuildSourceImage)
(BSI) shell tool. Both a CLI and a Go API are provided: the CLI for standalone
use, the `pkg/` API for embedding in other tools.

## What it does

Given one or more inputs, `gobsi` produces an OCI image layout directory:

- **SRPMs** — every `*.src.rpm` found under a directory (recursively) becomes a
  layer, annotated with metadata read from the RPM header (name, version,
  release, epoch, license, build time, pkgid).
- **Extra source directories** — a catch-all for source material not covered by
  the SRPM driver. Each directory's entire tree is packed verbatim (recursively,
  no filtering) into a reproducible tar and added as a layer. Typical contents
  are things like vendored dependencies, patches, build scripts, licenses, or
  the source of software that isn't shipped as an RPM.

At least one input (SRPMs or an extra source directory) is required. The result
is a spec-compliant OCI image layout with `oci-layout`, `index.json`, and a
`blobs/sha256/` store.

`gobsi` does no network I/O: it does not pull, push, or fetch SRPMs. It operates
purely on local inputs and writes a local OCI layout.

## Install

```sh
go install github.com/konflux-ci/gobsi/cmd/gobsi@latest
```

Or build from source:

```sh
go build -o gobsi ./cmd/gobsi
```

## Usage

```
gobsi -o <output-dir> [-s <srpm-dir>] [-e <extra-src-dir>]... [-d]
```

| Flag | Short | Description |
|------|-------|-------------|
| `--output` | `-o` | Output OCI image layout directory (**required**) |
| `--srpm-dir` | `-s` | Directory of `*.src.rpm` files to add (searched recursively) |
| `--extra-src-dir` | `-e` | Extra source directory to add as a layer |
| `--debug` | `-d` | Enable debug logging |

### Examples

Build from a directory of SRPMs:

```sh
gobsi -s ./srpms -o ./source-image
```

Build from extra source directories:

```sh
gobsi -e ./vendored-sources -e ./patches -o ./source-image
```

Combine both:

```sh
gobsi -s ./srpms -e ./vendored-sources -o ./source-image
```

The output directory can then be consumed by any OCI-aware tool, e.g.:

```sh
skopeo copy oci:./source-image docker://registry.example.com/app:latest-source
```

## Library API

The build logic lives in `pkg/` and can be called directly:

```go
import "github.com/konflux-ci/gobsi/pkg/gobsi"

err := gobsi.BuildSourceImage(gobsi.BuildConfig{
    SRPMDir:   "./srpms",
    ExtraDirs: []string{"./vendored-sources"},
    OutputDir: "./source-image",
})
```

## Implementation notes

- **Source types** (`pkg/source`) — each input kind produces `Artifact`s with
  their own `DriverName` (`rpm_dir`, `extra_src_dir`) and metadata. SRPM metadata
  is read from the RPM header via
  [`cavaliergopher/rpm`](https://github.com/cavaliergopher/rpm); the whole file
  is also hashed so every artifact carries a checksum.
- **OCI layout** (`pkg/oci`) — builds the layout, per-artifact layer tars, config,
  manifest, and index using the official
  [`opencontainers/image-spec`](https://github.com/opencontainers/image-spec)
  Go types. Each layer tar mirrors the content layout BSI produces
  (`blobs/sha256/<checksum>` plus a `<driver>/` symlink), with normalized modes,
  zeroed mtimes/ownership, and deterministic ordering so builds are reproducible.
- **Artifact metadata** is emitted as `source.artifact.*` OCI descriptor
  annotations on each layer.

### Reproducibility

Layer tars are written deterministically: entries are ordered depth-first by
path segment, mtimes and ownership are zeroed, and directory/file modes are
normalized. The same inputs yield byte-identical tars.

## Development and Testing

### Requirements

- A Go toolchain.
- For the integration test only: `rpmbuild` and `tar` on `PATH`. It skips itself
  when either is missing, so unit tests run anywhere.

### Repository layout

```
cmd/gobsi        CLI entry point
pkg/gobsi        build orchestration (the public API)
pkg/source       source drivers: SRPM and extra-source-dir → Artifacts
pkg/oci          OCI layout, layer tars, config/manifest/index
pkg/pathutil     path-segment ordering for deterministic tars
test/integration end-to-end build test
```

### Testing

```sh
go test ./...              # run the whole suite
go test ./pkg/...          # unit tests only (no external tools needed)
go test ./test/integration # integration test only (needs rpmbuild and tar)
```

The suite has two layers:

- **Unit tests** live next to the code in `pkg/`. They need no external tools.
- **Integration test** (`test/integration`) exercises a full build end to end.
  It builds a real SRPM with `rpmbuild`, runs the build via the API, and
  inspects the resulting OCI layout. It **skips** itself if `rpmbuild` or `tar`
  is unavailable.
