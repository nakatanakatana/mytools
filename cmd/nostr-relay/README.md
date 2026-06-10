# nostr-relay

A minimal Nostr relay built on `fiatjaf.com/nostr/khatru`.

## Running

```bash
go build -o dist/nostr-relay ./cmd/nostr-relay
NOSTR_RELAY_PORT=8080 ./dist/nostr-relay
```

## Configuration

| Variable | Default | Description |
|---|---:|---|
| `NOSTR_RELAY_HOST` | `0.0.0.0` | Host interface to bind to. |
| `NOSTR_RELAY_PORT` | `8080` | Port to listen on. |
| `NOSTR_RELAY_NAME` | `mytools relay` | Relay name returned by NIP-11. |
| `NOSTR_RELAY_DESCRIPTION` | `A minimal Nostr relay` | Relay description returned by NIP-11. |
| `NOSTR_RELAY_MAX_QUERY_LIMIT` | `500` | Maximum stored events returned for one query filter. |
| `LOG_LEVEL` | `info` | Logging level: `debug`, `info`, `warn`, or `error`. |

## NIP Support

### Supported

| NIP | Status | Implemented behavior |
|---|---|---|
| NIP-01 | Supported via `fiatjaf.com/nostr/khatru` | Event validation, `EVENT`, `REQ`, `CLOSE`, stored-event query, live subscriptions, `OK`, `EOSE`, `CLOSED`, and `NOTICE`. |
| NIP-11 | Supported via `fiatjaf.com/nostr/khatru` | Relay information document for `Accept: application/nostr+json` HTTP requests on the relay endpoint. |

### Not Supported

| NIP | Status | Reason |
|---|---|---|
| NIP-09 | Not supported | Deletion request policy is outside the first minimal relay scope. |
| NIP-13 | Not supported | Proof-of-work policy is not enforced. |
| NIP-20 | Not supported | Command result extensions beyond the library default behavior are not customized. |
| NIP-22 | Not supported | Comment event semantics are not interpreted by this relay. |
| NIP-40 | Not supported | Expiration timestamps are not enforced by this command. |
| NIP-42 | Not supported | Authentication is intentionally excluded from v1. |
| NIP-45 | Not supported | `COUNT` behavior is not exposed as a supported relay feature. |
| NIP-50 | Not supported | Search filters are not implemented. |
| NIP-70 | Not supported | Protected event policy is not enforced. |
| NIP-77 | Not supported | Negentropy sync is not implemented. |
| NIP-86 | Not supported | Relay management API is not enabled. |
| Other NIPs | Not supported | Events may pass through as opaque valid NIP-01 events, but higher-level semantics are not interpreted. |

## NIP-11

```bash
curl -H 'Accept: application/nostr+json' http://localhost:8080/
```

## Limitations

Events are stored in memory. Restarting the process clears the relay.
