# mytools

![publish-docker-image](https://github.com/nakatanakatana/mytools/actions/workflows/publish-docker-image.yaml/badge.svg)
![CI](https://github.com/nakatanakatana/mytools/actions/workflows/ci.yaml/badge.svg)
![Coverage](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/coverage.svg?raw=true)
![Code to Test Ratio](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/ratio.svg?raw=true)
![Test Execution Time](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/time.svg?raw=true)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/nakatanakatana/mytools)

A monorepo containing various utility tools written in Go.

## Included Tools

### 1. [sarif-to-codequality](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/README.md)
A CLI tool that converts SARIF (Static Analysis Results Interchange Format) files into GitLab Code Quality format.
It helps you merge security and analysis results into GitLab's code quality UI within CI pipelines.

### 2. [nip05](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/nip05/README.md)
A standalone server for managing, generating, and serving `.well-known/nostr.json` files for Nostr's NIP-05 (user identifier and domain verification).

---

## Development and Build

This repository uses [aqua](https://aquaproj.github.io/) to manage development tools (Go, GolangCI-Lint, GoReleaser, etc.).

### Setup Dependencies
```bash
aqua i
```

### Build
Build all tools and output binaries under the `dist/` directory.
```bash
make build
# or
go build -o ./dist/ ./cmd/...
```

### Run Tests
```bash
make test
# or
go test ./...
```

### Run Linter
```bash
make lint
# or
golangci-lint run ./...
```

---

## Docker Integration

You can build Docker images for individual tools or for the entire monorepo.

### Build Tool-Specific Image
```bash
# For sarif-to-codequality only
docker build --target sarif-to-codequality -t sarif-to-codequality .

# For nip05 only
docker build --target nip05 -t nip05 .
```

### Build All Tools Image
```bash
docker build --target mytools -t mytools .
```
