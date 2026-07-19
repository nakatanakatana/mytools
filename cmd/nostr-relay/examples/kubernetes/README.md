# Kubernetes reference example for nostr-relay

These files are reference-only resources for `nostr-relay` private SQLite
mode, not a complete cluster installation. Review every `REPLACE_*` value,
choose a namespace, pin the container image, and adapt storage and
NetworkPolicy selectors before use. They deliberately do not create a
Namespace or install/manage Tailscale or Cloudflare components.

SQLite requires a single Pod/single writer. Keep `replicas: 1` with the
`Recreate` strategy and mount the `ReadWriteOnce` PVC at
`/var/lib/nostr-relay`; the configured database path is
`/var/lib/nostr-relay/relay.db`. Do not share this file or PVC with the bridge
or another relay replica, and arrange database backups.

Configure `NOSTR_RELAY_SERVICE_URL` with the canonical client-facing HTTP(S)
URL, including its exact path when non-root. Configure the admin public key and
at least one reader public key as 64-character hexadecimal Nostr public keys;
the admin key must not appear in the reader list. These values are public keys,
not secrets. The corresponding admin private key belongs only in the bridge's
Secret and must never be placed in the relay Deployment.

The protocol Service uses port 8080. Add environment-specific Tailscale or
Cloudflare exposure for this Service if required; TLS termination must preserve
the canonical URL configured above. The management Service uses port 8081 and
must remain cluster-private. Never expose management to readers, Tailscale,
Cloudflare, an ingress, or a public load balancer: only `nostr-bridge` should be
allowed to connect. Adapt `network-policy.yaml` namespace labels and selectors
to enforce that boundary in your cluster.

Apply the files only after customization, for example:

```console
kubectl apply -f pvc.yaml -f deployment.yaml -f service.yaml -f network-policy.yaml
```
