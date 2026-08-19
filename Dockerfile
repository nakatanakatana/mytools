ARG BUILDER=builder
FROM golang:1.26@sha256:45a5f7a810238aabcbad211d70b9ae082022d96f7c7259e94041ad1b933575ac AS builder

WORKDIR /app/source

COPY go.* ./
RUN mkdir /app/output
RUN go mod download

COPY ./ /app/source

ARG CGO_ENABLED=0

RUN go build -o /app/output ./cmd/...

FROM ${BUILDER} AS builder-from

FROM gcr.io/distroless/static@sha256:9197324ba51d9cd071af8505989365c006adf9d6d2067eada25aef00abbb5278 AS base
COPY --from=builder-from /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# sarif-to-codequality image
FROM base AS sarif-to-codequality
COPY --from=builder-from /app/output/sarif-to-codequality /app/
ENTRYPOINT ["/app/sarif-to-codequality"]

# nip05 image
FROM base AS nip05
COPY --from=builder-from /app/output/nip05 /app/
ENTRYPOINT ["/app/nip05"]

# wsl-keyring image
FROM base AS wsl-keyring
COPY --from=builder-from /app/output/wsl-keyring /app/
ENTRYPOINT ["/app/wsl-keyring"]

# ff image
FROM base AS ff
COPY --from=builder-from /app/output/ff /app/
ENTRYPOINT ["/app/ff"]

# nostr-relay image
FROM base AS nostr-relay
COPY --from=builder-from /app/output/nostr-relay /app/
ENTRYPOINT ["/app/nostr-relay"]

# nostr-bridge image
FROM base AS nostr-bridge
COPY --from=builder-from /app/output/nostr-bridge /app/
ENTRYPOINT ["/app/nostr-bridge"]

# litestream-controller image
FROM base AS litestream-controller
COPY --from=builder-from /app/output/litestream-controller /app/
ENTRYPOINT ["/app/litestream-controller"]

# all apps image
FROM base AS mytools
COPY --from=builder-from /app/output /app
