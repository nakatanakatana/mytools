# nip05

![publish-docker-image](https://github.com/nakatanakatana/mytools/actions/workflows/publish-docker-image.yaml/badge.svg)
![CI](https://github.com/nakatanakatana/mytools/actions/workflows/ci.yaml/badge.svg)
![Coverage](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/coverage.svg?raw=true)
![Code to Test Ratio](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/ratio.svg?raw=true)
![Test Execution Time](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/time.svg?raw=true)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/nakatanakatana/mytools)

A suite of tools to easily and securely build and operate external services such as Nostr relays and NIP-05.

## Components

### NIP-05 Hosting Server (`cmd/nip05`)

A standalone server that handles HTTP requests to serve NIP-05 identifiers.

#### Configuration

The server is configured entirely via environment variables:

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `NIP05_PORT` | Port to listen on | `8080` | No |
| `NIP05_HOST` | Host interface to bind to | `0.0.0.0` | No |
| `NIP05_MAPPING` | Comma-separated `name:pubkey` pairs | | Yes |
| `LOG_LEVEL` | Logging level (`debug`, `info`, `warn`, `error`) | `info` | No |

Example `NIP05_MAPPING`: `bob:73c91d8...,alice:83d12...`

#### Running

```bash
# Build the binary
go build -o dist/nip05 ./cmd/nip05

# Run with environment variables
export NIP05_MAPPING="yourname:yourhexpubkey"
./dist/nip05
```

#### NIP-05 Verification Endpoint

The server serves the standard NIP-05 endpoint:
`GET /.well-known/nostr.json?name=<name>`

Example:
`curl "http://localhost:8080/.well-known/nostr.json?name=yourname"`

## Development

### Requirements
- Go 1.25.5 or later

### Running Tests
```bash
go test ./...
```

### Linting
```bash
golangci-lint run
```
