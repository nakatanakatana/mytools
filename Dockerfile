ARG BUILDER=builder
FROM golang:1.26@sha256:2981696eed011d747340d7252620932677929cce7d2d539602f56a8d7e9b660b AS builder

WORKDIR /app/source

COPY go.* ./
RUN mkdir /app/output
RUN go mod download

COPY ./ /app/source

ARG CGO_ENABLED=0

RUN go build -o /app/output ./cmd/...

FROM ${BUILDER} AS builder-from

FROM gcr.io/distroless/static@sha256:47b2d72ff90843eb8a768b5c2f89b40741843b639d065b9b937b07cd59b479c6 AS base
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

# all apps image
FROM base AS mytools
COPY --from=builder-from /app/output /app
