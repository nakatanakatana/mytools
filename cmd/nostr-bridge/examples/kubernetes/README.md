# Kubernetes application examples

These are reference-only, application-owned resources for `nostr-bridge`.
They intentionally do not create a Namespace, ingress, Tailscale or Cloudflare
proxy, controller, External Secrets Operator, or secret store. Customize every
`REPLACE_*` value, pin the image, and adapt selectors and network policy.

The Deployment runs one process and can host one Bluesky account plus one
Mastodon account. Remove every environment variable for a provider to disable
it (in particular its base URL); at least one provider must remain enabled.
Both providers publish a combined kind 3 follow list from the common owner
configured by `NOSTR_BRIDGE_OWNER_ID`.

SQLite is a single-writer database, so the Deployment uses one replica,
`Recreate`, and a `ReadWriteOnce` PVC at `/var/lib/nostr-bridge`. Never share
this PVC with the relay. Back it up with the matching secret versions.

## Secrets

`external-secret.yaml` illustrates references to fields in a 1Password item
through an already-installed External Secrets Operator and existing
`ClusterSecretStore`/`SecretStore`. It creates `nostr-bridge-secrets` with:

- `relay-admin-private-key`
- `master-seed`
- `bluesky-oauth-client-signing-key`
- `bluesky-oauth-encryption-key`
- `mastodon-oauth-client-secret`
- `mastodon-oauth-encryption-key`

Never commit their values. The master seed is base64 for exactly 32 random
bytes. Each OAuth encryption key is independently rotatable, but rotation
requires reauthorizing that provider if stored credentials cannot be migrated.

## Exposure and networking

The ClusterIP Service is private by default. Expose only these callback or
public Bluesky protocol artifacts through environment-owned HTTPS routing:

- `/oauth/bluesky/callback`
- `/oauth/bluesky/client-metadata.json`
- `/oauth/bluesky/jwks`
- `/oauth/mastodon/callback`

Authorization starts and ordinary UI can remain Tailscale-only. Keep
`/healthz`, `/readyz`, and `/metrics` private. The app needs outbound relay
protocol/management access, HTTPS to both provider APIs and OAuth servers, and
WebSocket streams to Bluesky Jetstream and Mastodon streaming. NetworkPolicy
cannot usually constrain public hosts by DNS name, so tighten the example for
your CNI and environment.

Apply after customization:

```console
kubectl apply -f pvc.yaml -f deployment.yaml -f service.yaml -f network-policy.yaml
# Only when External Secrets Operator and the referenced store already exist:
kubectl apply -f external-secret.yaml
```
