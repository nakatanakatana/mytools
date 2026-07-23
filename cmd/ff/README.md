# ff

![publish-docker-image](https://github.com/nakatanakatana/mytools/actions/workflows/publish-docker-image.yaml/badge.svg)
![CI](https://github.com/nakatanakatana/mytools/actions/workflows/ci.yaml/badge.svg)
![Coverage](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/coverage.svg?raw=true)
![Code to Test Ratio](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/ratio.svg?raw=true)
![Test Execution Time](https://github.com/nakatanakatana/octocov-central/blob/main/badges/nakatanakatana/mytools/time.svg?raw=true)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/nakatanakatana/mytools)

A feed filtering proxy server that allows filtering and modifying RSS/Atom feeds via URL query parameters.

## Features

- **Query-based filtering & modification**: Filter or modify feeds dynamically by adding query parameters.
- **Caching**: Built-in caching middleware with ETag and Last-Modified validation to minimize upstream requests.
- **Environment variables**: Mute specific authors or URLs by default, or force latest-only mode.

## Running

```bash
# Build the binary
go build -o dist/ff ./cmd/ff

# Run the server
./dist/ff
```

The server listens on `:8080` by default.

## Docker

```bash
docker build --target ff -t ff .
docker run -p 8080:8080 ff
```
