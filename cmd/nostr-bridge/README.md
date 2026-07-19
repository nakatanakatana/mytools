# nostr-bridge

`nostr-bridge` bridges selected Bluesky records to a separately operated Nostr
relay. It owns OAuth/health HTTP endpoints, SQLite outbox state, Jetstream, and
external relay delivery clients; it does not host a relay.

## Configuration

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `NOSTR_BRIDGE_HOST` | HTTP bind host | `127.0.0.1` | No |
| `NOSTR_BRIDGE_PORT` | HTTP bind port | `8080` | No |
| `NOSTR_BRIDGE_DATABASE_PATH` | Path to the bridge database | | Yes |
| `NOSTR_BRIDGE_RELAY_URL` | External relay WebSocket URL (`ws`/`wss`) | | Yes |
| `NOSTR_BRIDGE_RELAY_MANAGEMENT_URL` | Cluster-private relay management URL (`http`/`https`) | | Yes |
| `NOSTR_BRIDGE_RELAY_CANONICAL_URL` | External canonical relay URL signed in NIP-98 management requests; escaped path and query must equal the management URL | | Yes |
| `NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY` | Hex Nostr secret used only for signed management requests | | Yes |
| `NOSTR_BRIDGE_OUTBOX_LIMIT` | Hard durable delivery queue limit | `10000` | No |
| `NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL` | Dispatcher idle poll interval | `1s` | No |
| `NOSTR_BRIDGE_OAUTH_CALLBACK_URL` | Public OAuth callback URL | | Yes |
| `NOSTR_BRIDGE_OAUTH_AUTHORIZATION_SERVER_URL` | AT Protocol OAuth authorization-server URL | | Yes |
| `NOSTR_BRIDGE_OAUTH_CLIENT_ID` | Public URL of the OAuth client metadata document | | Yes |
| `NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY` | Base64-encoded PKCS#8 ECDSA P-256 private key | | Yes |
| `NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY` | Base64-encoded 32-byte key for persisted OAuth state and tokens | | Yes |
| `NOSTR_BRIDGE_ACCOUNT_DID` | DID of the OAuth-authorized Bluesky account | | Yes |
| `NOSTR_BRIDGE_BLUESKY_BASE_URL` | Bluesky XRPC service base URL | | Yes |
| `NOSTR_BRIDGE_MASTER_SEED` | Base64 encoding of an exactly 32-byte bridge key-derivation seed | External Secrets | Yes |
| `NOSTR_BRIDGE_JETSTREAM_URL` | Bluesky Jetstream endpoint URL | | Yes |
| `NOSTR_BRIDGE_LIST_URIS` | Comma-separated AT Protocol list URIs to bridge | | No |
| `NOSTR_BRIDGE_BACKFILL_LIMIT` | Maximum records read during initial backfill | `100` | No |
| `NOSTR_BRIDGE_RECONCILE_INTERVAL` | Interval between reconciliation passes (Go duration) | `1h` | No |

## Network exposure

Bridge application endpoints are Tailscale-only: run the service on the
private tailnet and do not expose its authorization, token, callback, or bridge
API routes through Cloudflare.

Cloudflare publishes only the public OAuth client metadata endpoint
`/oauth/client-metadata.json` and the JWKS endpoint `/oauth/jwks`. It does not
proxy the rest of the bridge service.

## Operations

The service exposes these Tailscale-only endpoints:

| Endpoint | Purpose |
| --- | --- |
| `/healthz` | Process liveness; returns success while the HTTP process is running. |
| `/readyz` | Readiness; requires a responsive database, an OAuth connection, a connected Jetstream consumer when targets exist, and an outbox count below `NOSTR_BRIDGE_OUTBOX_LIMIT`. Quiet Jetstream connections remain ready; no consumer is required when the target set is empty. |
| `/metrics` | Prometheus text metrics for sync and Jetstream state, target count, pending work, OAuth expiry, outbox pressure, and the last successful external-relay delivery. |

Do not publish these endpoints through Cloudflare. In particular, a readiness
failure can reveal that a private dependency is unavailable even though it does
not reveal tokens, keys, DIDs, or event contents.

### External secrets

Provide `NOSTR_BRIDGE_OAUTH_CLIENT_SIGNING_KEY`,
`NOSTR_BRIDGE_OAUTH_ENCRYPTION_KEY`, `NOSTR_BRIDGE_MASTER_SEED`, and
`NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY` through 1Password External Secrets.
Do not put them in source control, image layers, Cloudflare configuration,
metrics, or logs. Rotate the signing key only after updating the public JWKS
artifact, and rotate the encryption key with a planned token reauthorization:
existing encrypted OAuth sessions and tokens cannot be read with a new key.

Generate the master seed from 32 cryptographically random bytes (for example,
`openssl rand -base64 32`) and keep the same value across restarts. Changing it
changes every derived bridge publisher identity.

### Public Cloudflare artifacts

Cloudflare is limited to immutable or explicitly deployed public OAuth
artifacts: the client metadata and the OAuth client JWKS. Keep
the callback, `/oauth/start`, `/oauth/callback`, relay, health, readiness, and
metrics routes off Cloudflare and reachable only over Tailscale.

### Recovery

Give the bridge database its own PVC; do not share the relay database/PVC.
Back up the SQLite database and retain the external secret versions needed to
decrypt it. On recovery, restore both together, start the bridge on Tailscale,
and check `/healthz`, `/readyz`, and `/metrics`. If encryption material is lost
or rotated without a migration, delete the unreadable OAuth records and have
the operator complete OAuth authorization again. Jetstream resumes from its
stored cursor with a small rewind, so replayed source operations are handled
idempotently.

### Nostr data disclosure

Treat every bridged Nostr event as disclosed to the external relay and its
authorized clients. Keep the management endpoint cluster-private. Do not
bridge private messages, access tokens, OAuth callback parameters, or other
credentials; Nostr event content and metadata can be replicated by authorized
clients and are not a secrecy boundary.

## Running

```bash
go run ./cmd/nostr-bridge
```
