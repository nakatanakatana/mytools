# nostr-bridge

`nostr-bridge` copies selected public Bluesky and Mastodon activity to a
separately operated Nostr relay. Enable Bluesky, Mastodon, or both by setting
the provider's base URL. One process supports at most one account from each
provider. It does not host a relay.

All enabled providers contribute to one stable bridge-owner Nostr identity,
configured by `NOSTR_BRIDGE_OWNER_ID`. The owner's kind 3 follow list is the
union of actual follows and configured-list members from both providers;
provider lists become kind 30000 follow sets. Changing the master seed changes
all derived publisher identities.

## Synchronized data

Profiles become kind 0 events and posts become kind 1 events. Posts and
profiles retain source links. Attachments and avatars hotlink source-hosted
media, with attachment metadata emitted as NIP-92; the bridge does not copy
media. Mastodon spoiler/CW text is emitted as a NIP-36 `content-warning` tag.
Replies are linked when the parent is already mapped.

For each provider, synchronization targets are the union of accounts followed
by the authorized account and members of the configured lists. A list member
need not be followed by the authorized account. Mastodon synchronization reads
the home and configured-list timelines, accepts public statuses only, and
does not bridge boosts/reblogs. Mastodon does not offer a complete API stream
of every followed account. Viewing users follow the derived Nostr identities
manually; the bridge never signs or modifies a viewer's own follow list.

## Configuration

Shared and owner settings:

| Variable | Description | Default |
| --- | --- | --- |
| `NOSTR_BRIDGE_HOST` / `NOSTR_BRIDGE_PORT` | HTTP bind address | `127.0.0.1` / `8080` |
| `NOSTR_BRIDGE_DATABASE_PATH` | SQLite database path (required) | |
| `NOSTR_BRIDGE_MASTER_SEED` | Base64 encoding of exactly 32 random bytes (required) | |
| `NOSTR_BRIDGE_RELAY_URL` | External relay `ws`/`wss` URL (required) | |
| `NOSTR_BRIDGE_RELAY_MANAGEMENT_URL` | Private relay management URL (required) | |
| `NOSTR_BRIDGE_RELAY_CANONICAL_URL` | Canonical relay URL signed in management requests (required) | |
| `NOSTR_BRIDGE_RELAY_ADMIN_PRIVATE_KEY` | Hex Nostr management key (required) | |
| `NOSTR_BRIDGE_OUTBOX_LIMIT` | Durable queue limit | `10000` |
| `NOSTR_BRIDGE_OUTBOX_POLL_INTERVAL` | Dispatcher poll interval | `1s` |
| `NOSTR_BRIDGE_OWNER_ID` | Stable local identifier for the common bridge owner (required) | |
| `NOSTR_BRIDGE_OWNER_NAME` | Owner profile display name | `nostr-bridge` |
| `NOSTR_BRIDGE_OWNER_ABOUT` | Owner profile description | |
| `NOSTR_BRIDGE_OWNER_PICTURE` | Owner profile HTTPS image URL | |

Bluesky is enabled when `NOSTR_BRIDGE_BLUESKY_BASE_URL` is non-empty:

| Variable | Description | Default |
| --- | --- | --- |
| `NOSTR_BRIDGE_BLUESKY_ACCOUNT_DID` | Authorized account DID (required when enabled) | |
| `NOSTR_BRIDGE_BLUESKY_BASE_URL` | XRPC service base URL | disabled |
| `NOSTR_BRIDGE_BLUESKY_JETSTREAM_URL` | Jetstream WebSocket URL (required when enabled) | |
| `NOSTR_BRIDGE_BLUESKY_LIST_URIS` | Comma-separated list URIs | |
| `NOSTR_BRIDGE_BLUESKY_BACKFILL_LIMIT` | Initial backfill limit | `100` |
| `NOSTR_BRIDGE_BLUESKY_RECONCILE_INTERVAL` | Target reconciliation interval | `1h` |
| `NOSTR_BRIDGE_BLUESKY_OAUTH_CALLBACK_URL` | Public HTTPS URL ending `/oauth/bluesky/callback` | |
| `NOSTR_BRIDGE_BLUESKY_OAUTH_AUTHORIZATION_SERVER_URL` | AT Protocol authorization server | |
| `NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_ID` | Public HTTPS URL ending `/oauth/bluesky/client-metadata.json` | |
| `NOSTR_BRIDGE_BLUESKY_OAUTH_CLIENT_SIGNING_KEY` | Base64 PKCS#8 P-256 signing key | |
| `NOSTR_BRIDGE_BLUESKY_OAUTH_ENCRYPTION_KEY` | Base64 32-byte token/state encryption key | |

Mastodon is enabled when `NOSTR_BRIDGE_MASTODON_BASE_URL` is non-empty:

| Variable | Description | Default |
| --- | --- | --- |
| `NOSTR_BRIDGE_MASTODON_BASE_URL` | Account's instance origin | disabled |
| `NOSTR_BRIDGE_MASTODON_ACCOUNT` | Exactly one `user@instance` account (required when enabled) | |
| `NOSTR_BRIDGE_MASTODON_LIST_IDS` | Comma-separated list IDs | |
| `NOSTR_BRIDGE_MASTODON_BACKFILL_LIMIT` | Per-timeline backfill limit | `100` |
| `NOSTR_BRIDGE_MASTODON_RECONCILE_INTERVAL` | Target reconciliation interval | `1h` |
| `NOSTR_BRIDGE_MASTODON_OAUTH_CALLBACK_URL` | Public HTTPS URL ending `/oauth/mastodon/callback` | |
| `NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_ID` | Mastodon application client ID | |
| `NOSTR_BRIDGE_MASTODON_OAUTH_CLIENT_SECRET` | Mastodon application client secret | |
| `NOSTR_BRIDGE_MASTODON_OAUTH_ENCRYPTION_KEY` | Base64 32-byte token/state encryption key | |

## OAuth and network exposure

Only public OAuth protocol callbacks/artifacts require external HTTPS access:
`/oauth/bluesky/callback`, `/oauth/bluesky/client-metadata.json`,
`/oauth/bluesky/jwks`, and `/oauth/mastodon/callback`. Ordinary UI and auth
starts (`/oauth/bluesky/start` and `POST /oauth/mastodon/start`) may remain
Tailscale-only. Do not expose health, metrics, or unrelated routes publicly.
Allow outbound HTTPS to OAuth and provider APIs, outbound WebSockets to
Bluesky Jetstream and Mastodon streaming, and relay protocol/management
connections.

### OAuth authorization

The public client metadata and pushed authorization request ask the Bluesky
AppView for permission to call `app.bsky.graph.getFollows`,
`app.bsky.graph.getList`, `app.bsky.actor.getProfile`, and
`app.bsky.feed.getTimeline`. When a release adds or changes these permissions,
existing tokens do not gain them through refresh. After deploying such a
release, start a new authorization with `/oauth/bluesky/start` and complete the OAuth
flow before checking synchronization health.

## Operations and recovery

SQLite requires one process/one writer and persistent storage. Do not share
its PVC with the relay. Back up the database with the exact external-secret
versions used to encrypt OAuth data. `/healthz` reports process liveness,
`/readyz` gates on the shared database, outbox, dispatcher, and each enabled
provider's authentication and bootstrap state. Stream connection and last
event are reported by health/metrics, but a quiet stream or transient stream
disconnect does not fail readiness. `/metrics` exposes provider-labelled
operational metrics.

If one provider loses or rotates its OAuth encryption material, remove that
provider's unreadable OAuth rows and authorize it again; the other provider's
credentials are independent. Restore the database and matching secrets
together. Stream cursors and idempotent mappings allow recovery/replay without
intentionally duplicating Nostr events.

Keep the relay admin key, master seed, both provider encryption keys, the
Bluesky signing key, and Mastodon client secret in an external secret store.
Never put credentials in manifests, images, logs, or metrics.

## Running

```bash
go run ./cmd/nostr-bridge
```
