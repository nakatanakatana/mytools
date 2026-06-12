ARG BUILDER=builder
FROM golang:1.26@sha256:87a41d2539e5671777734e91f467499ed5eafb1fb1f77221dff2744db7a51775 AS builder

WORKDIR /app/source

COPY go.* ./
RUN mkdir /app/output
RUN go mod download

COPY ./ /app/source

ARG CGO_ENABLED=0

RUN go build -o /app/output ./cmd/...

FROM ${BUILDER} AS builder-from

FROM gcr.io/distroless/static@sha256:3592aa8171c77482f62bbc4164e6a2d141c6122554ace66e5cc910cadb961ff0 AS base
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

# nostr-relay image
FROM base AS nostr-relay
COPY --from=builder-from /app/output/nostr-relay /app/
ENTRYPOINT ["/app/nostr-relay"]

# all apps image
FROM base AS mytools
COPY --from=builder-from /app/output /app
