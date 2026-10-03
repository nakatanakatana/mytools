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
| `NOSTR_BRIDGE_UI_URL` | Optional private dashboard root URL used after OAuth callbacks; when unset, callbacks redirect to the same-origin `/` dashboard | (optional) |
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
| `NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_PERIOD` | Time since the persisted successful refresh before another refresh is due | `720h` |
| `NOSTR_BRIDGE_BLUESKY_OAUTH_REFRESH_CHECK_INTERVAL` | How often OAuth maintenance inspects whether a refresh is due | `24h` |

OAuth maintenance measures the refresh period from the persisted successful
refresh time, not from process start or the previous inspection. The check
interval controls only inspection frequency; it does not make a refresh due
sooner. An expired access token can remain Ready when the durable
authorization is refreshable. A transient refresh failure is reported as
degraded and remains Ready while the authorization is refreshable. A permanent
failure requires reauthorization and makes `/readyz` NotReady until the OAuth
flow completes successfully.

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

## Private web UI, status, and network exposure

`GET /` serves the embedded private dashboard. It polls `GET /api/status` on
the same bridge origin; that endpoint returns a sanitized operational snapshot
with overall readiness, database/dispatcher state, an outbox summary, and
provider-level authorization, bootstrap, stream, target, pending-work, and
timestamp fields. It does not return credentials, OAuth tokens or state,
keys, cursors, target identities, outbox payloads, or raw errors.

`ready: false` in a successful status response is a normal degraded-state
report, not a failure of the status endpoint itself. Timestamp fields are
`null` until the relevant event, delivery, sync, or reconciliation has first
occurred. `target_count` and `pending_work` are current operational indicators;
they are not a completed/total synchronization percentage.

The dashboard starts the existing same-origin OAuth flows: Bluesky sends
`POST /oauth/bluesky/start` with a JSON handle hint, and Mastodon sends
`POST /oauth/mastodon/start` with no request body. Both successful callbacks
redirect to `NOSTR_BRIDGE_UI_URL?oauth=success` when configured, or to
`/?oauth=success` when it is unset. When configured, the value must be an
absolute HTTP/HTTPS URL for the dashboard root; path prefixes are not supported.
Public OAuth callback routes may use a different host when
`NOSTR_BRIDGE_UI_URL` is configured. The UI URL controls the OAuth completion
destination; it is not an access-control mechanism. Keep the
dashboard, status, and OAuth-start routes behind the private network or ingress
boundary, and expose only the callback, metadata, and JWKS routes needed by the
providers. The dashboard displays a separate completion notice and keeps it
visible while the first updated status is fetched.

### Browser push notifications

Browser push subscriptions and generated VAPID keys are stored in SQLite. The
generated VAPID private key is stored as plaintext, so restrict access to the
database file, its persistent volume, and its backups. If VAPID keys are
provided through `NOSTR_BRIDGE_VAPID_PRIVATE_KEY` and
`NOSTR_BRIDGE_VAPID_PUBLIC_KEY`, keep that pair available across restarts;
environment-provided keys are not copied into SQLite. Removing the configured
pair loads a previously generated pair from SQLite, if one exists; otherwise,
the bridge generates and stores a new pair. Preserve the previous configured
pair in your secrets manager until the new pair has been verified, so it is
available if you need to roll back.

If startup reports invalid stored VAPID keys, stop the bridge and back up its
database before removing the row with
`sqlite3 /path/to/nostr-bridge.db 'DELETE FROM webpush_vapid_keys WHERE id = 1;'`.
Restarting generates a new key pair. Existing browser subscriptions must be
re-registered with the new key; the dashboard offers this for browsers that
visit it again.

Changing the VAPID key pair invalidates existing browser push subscriptions.
The dashboard detects a mismatch and offers to replace the subscription with
one using the current key. Existing subscriptions on other browsers or devices
must be re-registered from those clients. Set `NOSTR_BRIDGE_VAPID_SUBJECT` to
change the contact value used in VAPID tokens; this setting takes effect after
the bridge restarts.

Push notifications use the same SQLite database to read subscriptions. If
SQLite is unavailable, the process cannot send a push alert about that database
failure. Monitor `/readyz` through an independent health-checking system when
database availability needs external alerting. `/healthz` reports only process
liveness.

The push API has no application-level authentication and limits the instance
to 100 subscriptions. Keep `/api/push/vapid-public-key`,
`/api/push/subscribe`, and `/api/push/unsubscribe` behind the private ingress
boundary described above.

| Variable | Description | Default |
| --- | --- | --- |
| `NOSTR_BRIDGE_NOTIFICATION_REMIND_INTERVAL` | Minimum interval between reminders while the same issue remains active (at least `1m`; a recurrence after recovery is notified immediately) | `24h` |
| `NOSTR_BRIDGE_NOTIFICATION_EVALUATION_INTERVAL` | How often the bridge checks for notification issues and retries failed deliveries (at least `1s`) | `15s` |

Abandoned subscriptions are removed when a push service reports them expired.
If browser data was cleared and the 100-subscription limit is reached before
those entries are reclaimed, stop the bridge and back up its database, then
clear the push subscriptions from the configured SQLite database with
`sqlite3 /path/to/nostr-bridge.db 'DELETE FROM webpush_subscriptions;'` and
restart the bridge. This clears all browser push registrations; users must
toggle notifications off and on again from each browser to register them.
Permanent push-service 4xx responses are logged and not retried for the same
active issue and unchanged subscription. Check those logs after configuration
or subscription changes. Restart after correcting configuration; updating a
subscription also allows delivery to be retried.

The dashboard is not a configuration editor. Provider credentials and
instance, account, and list configuration remain environment variables read
at process startup; changing them requires updating the environment and
restarting the bridge.

Keep `/`, `/api/status`, `/oauth/bluesky/start`, `/oauth/mastodon/start`,
`/healthz`, `/readyz`, and `/metrics` private behind the existing network
boundary. The embedded UI does not add authentication. Only public OAuth
protocol callbacks/artifacts require external HTTPS access:
`/oauth/bluesky/callback`, `/oauth/bluesky/client-metadata.json`,
`/oauth/bluesky/jwks`, and `/oauth/mastodon/callback`. Allow outbound HTTPS to
OAuth and provider APIs, outbound WebSockets to Bluesky Jetstream and
Mastodon streaming, and relay protocol/management connections.

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
provider's authentication and bootstrap state. An enabled provider with
targets must also have a connected live stream; an enabled provider with no
targets needs no stream connection. A quiet but connected stream remains
ready. `/metrics` exposes provider-labelled operational metrics.

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
