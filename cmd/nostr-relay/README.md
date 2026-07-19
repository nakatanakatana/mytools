# nostr-relay

A Nostr relay built on `fiatjaf.com/nostr/khatru`. It can run as an ephemeral
public relay or as an authenticated, persistent private relay.

## Running

Public, in-memory mode is the default:

```bash
go build -o dist/nostr-relay ./cmd/nostr-relay
NOSTR_RELAY_PORT=8080 ./dist/nostr-relay
```

Private mode requires a SQLite path, the relay's canonical external URL, one
administrator public key, and at least one reader public key:

```bash
NOSTR_RELAY_MODE=private-sqlite \
NOSTR_RELAY_DATABASE_PATH=/var/lib/nostr-relay/relay.db \
NOSTR_RELAY_SERVICE_URL=https://relay.example.com \
NOSTR_RELAY_ADMIN_PUBKEY=<64-hex-public-key> \
NOSTR_RELAY_READER_PUBKEYS=<64-hex-reader-key>[,<another-reader-key>] \
NOSTR_RELAY_MANAGEMENT_PORT=8081 \
./dist/nostr-relay
```

The values above are public keys, not secret keys. Never put Nostr secret keys
in relay environment variables.

## Configuration

| Variable | Default | Modes | Description |
|---|---:|---|---|
| `NOSTR_RELAY_HOST` | `0.0.0.0` | all | Interface on which the HTTP server listens. |
| `NOSTR_RELAY_PORT` | `8080` | all | HTTP server port. |
| `NOSTR_RELAY_MANAGEMENT_HOST` | `0.0.0.0` | private | Interface for the dedicated NIP-86 management listener. |
| `NOSTR_RELAY_MANAGEMENT_PORT` | `8081` | private | Dedicated NIP-86 management listener port. Do not expose it to readers. |
| `NOSTR_RELAY_NAME` | `mytools relay` | all | Name returned in the NIP-11 document. |
| `NOSTR_RELAY_DESCRIPTION` | `A minimal Nostr relay` | all | Description returned in the NIP-11 document. |
| `NOSTR_RELAY_MAX_QUERY_LIMIT` | `500` | all | Maximum stored events returned for a query filter. |
| `LOG_LEVEL` | `info` | all | `debug`, `info`, `warn`, or `error`. |
| `NOSTR_RELAY_MODE` | `public-memory` | all | `public-memory` or `private-sqlite`. |
| `NOSTR_RELAY_DATABASE_PATH` | none | private | Required SQLite database file. |
| `NOSTR_RELAY_SERVICE_URL` | none | private | Required canonical external absolute HTTP(S) URL, including any path. |
| `NOSTR_RELAY_ADMIN_PUBKEY` | none | private | Required 64-character hexadecimal public key authorized for NIP-86 management. It must not also be a reader key. |
| `NOSTR_RELAY_READER_PUBKEYS` | none | private | Required comma-separated 64-character hexadecimal public keys authorized to read after NIP-42 authentication. |

Private-mode-only variables are not required in `public-memory` mode.

## Modes and NIPs

| Mode | Storage and access | NIPs advertised by NIP-11 |
|---|---|---|
| `public-memory` | Anonymous publish/read; events disappear on process restart. | 1, 11, 40 |
| `private-sqlite` | Allowed publishers only; NIP-42-authenticated configured readers only; events and publisher allow-list persist across restarts. | 1, 9, 11, 42, 86, 98 |

In private mode, NIP-86 supports `allowpubkey`, `unallowpubkey`, and
`listallowedpubkeys`. NIP-98 authenticates those management requests. NIP-09
deletions are limited to events owned by the deletion-request author.

Fetch the relay information document with:

```bash
curl -H 'Accept: application/nostr+json' http://localhost:8080/
```

## Private-mode deployment

Mount `NOSTR_RELAY_DATABASE_PATH` on a persistent volume dedicated to this
relay. Do not share the SQLite file or its PVC with the bridge database or with
another relay replica. SQLite permits only one relay process to own this
database; use a single replica.

`NOSTR_RELAY_SERVICE_URL` is the canonical client-facing URL used to validate
NIP-42 and NIP-98 events. When TLS terminates at an ingress or load balancer,
configure the external `https://` URL (and its exact path), even though the
relay container receives internal HTTP. The internal host and scheme are not
substitutes for this value.

Private mode serves Nostr protocol traffic on `NOSTR_RELAY_PORT` and NIP-86 on
the separate `NOSTR_RELAY_MANAGEMENT_PORT`. Expose only the protocol port to
reader clients and restrict the management port to the bridge with NetworkPolicy
or an equivalent firewall. NIP-98 admin signatures remain mandatory defense in
depth.
