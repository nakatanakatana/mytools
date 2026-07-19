# Kubernetes reference example for nostr-bridge

These files are reference-only resources for `nostr-bridge`, not a complete
cluster installation. Review every `REPLACE_*` value, choose a namespace, pin
the container image, and adapt storage and NetworkPolicy selectors before use.
They deliberately do not create a Namespace or install/manage Tailscale,
Cloudflare, External Secrets Operator, or a cluster-wide `ClusterSecretStore`.

The bridge uses SQLite and must run as a single Pod/single writer with the
`ReadWriteOnce` PVC mounted at `/var/lib/nostr-bridge`; its database path is
`/var/lib/nostr-bridge/bridge.db`. Do not share this PVC with a relay. Back up
the database together with the secret versions required to decrypt its OAuth
state.

`deployment.yaml` needs the relay WebSocket, private management, and canonical
URLs; the OAuth client metadata and callback URLs; the Bluesky account DID;
and the authorization server, Bluesky XRPC, and Jetstream endpoints. The
canonical relay URL and management transport URL must have identical escaped
paths and raw queries. Optional settings such as list URIs may be added from
the configuration table in the app README.

Create `nostr-bridge-secrets` with these keys, or adapt the optional
`external-secret.yaml` to an existing secret provider:

- `relay-admin-private-key`: 64-character hexadecimal Nostr secret key
- `master-seed`: base64 encoding of exactly 32 random bytes
- `oauth-client-signing-key`: base64-encoded PKCS#8 ECDSA P-256 private key
- `oauth-encryption-key`: base64 encoding of exactly 32 bytes

Never commit those values. `external-secret.yaml` assumes External Secrets
Operator and the referenced SecretStore already exist.

The ClusterIP Service exposes port 8080 for OAuth callbacks and health routes.
Add environment-specific Tailscale Service/proxy configuration for private
access. If Cloudflare publishes the OAuth metadata and JWKS artifacts, expose
only `/oauth/client-metadata.json` and `/oauth/jwks`; do not publish callback,
authorization, health, readiness, metrics, or bridge API routes. The egress
policy must permit DNS, the relay ports, and TLS to the configured OAuth,
Bluesky, and Jetstream endpoints; adapt it for your CNI and DNS setup.

Apply the files only after customization, for example:

```console
kubectl apply -f pvc.yaml -f deployment.yaml -f service.yaml -f network-policy.yaml
# Optional when External Secrets Operator is already installed:
kubectl apply -f external-secret.yaml
```
