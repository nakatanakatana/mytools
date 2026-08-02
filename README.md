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

### 3. [wsl-keyring](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/wsl-keyring/README.md)
A D-Bus Secret Service provider daemon that exposes the `org.freedesktop.secrets` interface and integrates with 1Password (`op.exe`) as the storage backend. Designed for WSL environments.

### 4. [nostr-relay](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/nostr-relay/README.md)
A minimal Nostr relay built on `fiatjaf.com/nostr/khatru`, supporting NIP-01 relay flow and NIP-11 relay metadata with in-memory storage.

### 5. [herdr-plugin-tabinfo](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/herdr-plugin-tabinfo/README.md)
A Herdr plugin that rewrites tab labels with live tab information through the Herdr Socket API.

### 6. [ff](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/ff/README.md)
A feed filtering proxy server that allows filtering and modifying RSS/Atom feeds via URL query parameters.

### 7. [litestream-controller](file:///home/tanaka/repos/github.com/nakatanakatana/mytools/cmd/litestream-controller/README.md)
A Kubernetes controller and admission webhook that runs [Litestream](https://litestream.io) alongside your application Pods, restoring and continuously replicating SQLite databases without baking Litestream into your application's image.

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

The integration tests use controller-runtime's envtest assets. Set
`KUBEBUILDER_ASSETS` before running the test suite:

```bash
envtest_version="$(go list -m -f '{{.Version}}' sigs.k8s.io/controller-runtime)"
envtest_k8s_version="$(go list -m -f '{{.Version}}' k8s.io/api | sed -E 's/^v[0-9]+\.([0-9]+)\..*$/1.\1/')"
export KUBEBUILDER_ASSETS="$(
  go run sigs.k8s.io/controller-runtime/tools/setup-envtest@"${envtest_version}" \
    use -p path "${envtest_k8s_version}"
)"

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

# For wsl-keyring only
docker build --target wsl-keyring -t wsl-keyring .

# For nostr-relay only
docker build --target nostr-relay -t nostr-relay .

# For ff only
docker build --target ff -t ff .

# For litestream-controller only
docker build --target litestream-controller -t litestream-controller .
```

### Build All Tools Image
```bash
docker build --target mytools -t mytools .
```
