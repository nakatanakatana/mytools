ARG BUILDER=builder
FROM golang:1.27@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS builder

WORKDIR /app/source

COPY go.* ./
RUN mkdir /app/output
RUN go mod download

COPY ./ /app/source

ARG CGO_ENABLED=0

RUN go build -o /app/output ./cmd/...

FROM ${BUILDER} AS builder-from

FROM gcr.io/distroless/static@sha256:4c2cd1dee442c9a99924a378c7c126fac8fef14771579ef567c3f2bdd7a5122a AS base
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

# sandbox-control image
FROM base AS sandbox-control
COPY --from=builder-from /app/output/sandbox-control /app/
USER 1000:1000
ENTRYPOINT ["/app/sandbox-control"]

# all apps image
FROM base AS mytools
COPY --from=builder-from /app/output /app
