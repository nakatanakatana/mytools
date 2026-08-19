# litestream-controller

A Kubernetes controller and admission webhook that runs [Litestream](https://litestream.io)
alongside your application Pods. It restores a SQLite database before your
application starts, replicates it continuously while your application runs,
or both — without baking Litestream into your application's image.

- The `LitestreamReconciler` resolves shared resources, then renders each
  `Litestream` resource into an owned, non-secret ConfigMap and reports
  rendering success or failure on the resource's `Ready` status condition.
- The `/mutate-v1-pod` admission webhook injects Litestream init containers
  and a replication sidecar into any Pod annotated with a reference to a
  `Ready` `Litestream` resource.
- The validating webhooks reject invalid `Litestream` and `LitestreamReplica`
  resources, reject multi-Pod workloads that would run replicated Litestream
  sidecars, and protect referenced Replicas from deletion. The Pod injection
  webhook also rejects an active Pod that would reuse a replication destination
  already used by another active Pod.

Secret values (backend credentials) are never read by the controller or the
webhook, never stored in a ConfigMap, and never appear in a `Litestream`
resource's spec. A `LitestreamReplica` contains only a
`SecretKeySelector` (`secretKeyRef`) reference, and the controller does not
read Secret values. The kubelet resolves those references — as environment
variables or projected files — only inside the Pods that actually run.

## Resource model and dependency lifecycle

The API separates reusable remote storage definitions from workload behavior:

- `LitestreamReplica` defines one remote backend endpoint and path, plus
  optional credential references.
- Each `Litestream` database binding declares its local inline `path`, an
  optional restore source through `restore.replicaRef` and restore policies,
  and an optional replication destination through `replicate.replicaRef`. The
  operation is inferred from the presence of source and destination, and their
  identity: source only restores, destination only replicates, the same source
  and destination replicates, and different source and destination clones.

All `replicaRef` values are same namespace references. A reference never
crosses namespaces; create the corresponding `LitestreamReplica` in each
workload namespace. Each binding repeats its local path and, when needed, its
source Replica reference. Centralize that repeated YAML with Helm, Kustomize,
or another YAML composition tool when multiple resources share it.

Apply dependencies before consumers: create source and destination
`LitestreamReplica` resources, then create the `Litestream`, and finally
create the annotated workload. The controller sets `Ready=False` (reason
`InvalidConfiguration`) and publishes no ConfigMap while a referenced Replica
is missing or invalid. Under the default webhook policy, a Pod that refers to
that non-Ready `Litestream` is rejected rather than started without
Litestream.

Referenced Replicas are protected. Their deletion is rejected while a
same-namespace `Litestream` uses the Replica as a restore source or replication
destination. Delete all consuming `Litestream` resources first, then the
`LitestreamReplica`.

Updates propagate live to future Pods. When a `Litestream` or
`LitestreamReplica` changes, the controller reconciles all consuming
Litestream resources and renders a new ConfigMap revision for every changed
configuration. Existing Pods keep the revision injected at creation time, so
restart their owning workload when the update must take effect.

## Container ordering

The webhook places newly injected Litestream restore init containers before
all init containers already declared by the workload, so a database is
restored before workload setup begins. When replication is configured, the
native replication sidecar follows the restore containers and starts only
after every restore has completed.

## Requirements

- **Kubernetes 1.30 or later.** A Kustomize patch adds stable admission webhook
  `matchConditions` to the mutating webhook, selecting only Pods whose
  `litestream.mytools.nakatanakatana.app/inject` annotation is non-empty after
  trimming whitespace. The replication sidecar is injected as a *native
  sidecar* — an init container with
  `restartPolicy: Always` — a feature that defaults on starting in Kubernetes
  1.29 and reached general availability in 1.33. A cluster older than 1.29
  will run the restore init containers, but the injected replication container
  will behave like an ordinary init container: it will run to completion (or
  block Pod startup forever) instead of running alongside your application.
  **Replicating workloads (including inferred clones) require Kubernetes
  1.29+.** The `SidecarContainers` feature is enabled by
  default from Kubernetes 1.29 and is stable from 1.33, but it may be disabled
  by cluster configuration. Before installing, confirm that the API server and
  every node that may run an annotated workload meet this minimum:

  ```bash
  kubectl version --short 2>/dev/null || kubectl version
  # Server Version must report v1.30 or newer.

  kubectl get nodes \
    -o custom-columns='NAME:.metadata.name,KUBELET-VERSION:.status.nodeInfo.kubeletVersion'
  # Every node that may run an annotated workload must report v1.29 or newer.
  ```

  If your cluster explicitly configures feature gates, verify that
  `SidecarContainers` is enabled for the API server and the relevant kubelets:

  ```bash
  kubectl get --raw /metrics | grep 'name="SidecarContainers"'
  kubectl get --raw /api/v1/nodes/<node-name>/proxy/metrics | grep 'name="SidecarContainers"'
  ```
- [cert-manager](https://cert-manager.io) installed in the cluster, to issue
  the webhook's serving certificate.

## Installing

### 1. Install cert-manager

The webhook requires a TLS certificate that the API server trusts.
`config/litestream-controller/certmanager` provisions a self-signed `Issuer` and a `Certificate`
for the webhook service; cert-manager itself must already be running in the
cluster:

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available --timeout=120s -n cert-manager deployment --all
```

### 2. Select the published controller image

This repository publishes the controller image to GitHub Container Registry:
`ghcr.io/nakatanakatana/litestream-controller`. No local Docker build or
push is required for installation. The checked-in manager `Deployment` uses
the `latest` tag for convenience and sets `imagePullPolicy: IfNotPresent`;
use a release tag or image digest for reproducible production installs.

To pin a release tag without editing `manager.yaml`, add an `images:` entry to
`config/litestream-controller/manager/kustomization.yaml` or to a local overlay.
The publish workflow converts a Git tag such as `v0.1.0` into the image tag
`0.1.0`. Replace `0.1.0` with the published release you want to install:

```yaml
# config/litestream-controller/manager/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- manager.yaml

images:
- name: ghcr.io/nakatanakatana/litestream-controller
  newTag: 0.1.0
```

For the strongest supply-chain and rollback guarantees, pin the image by its
multi-platform manifest digest instead of a tag:

```yaml
# config/litestream-controller/manager/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- manager.yaml

images:
- name: ghcr.io/nakatanakatana/litestream-controller
  digest: sha256:<multi-platform-manifest-digest>
```

If you use `latest`, remember that it is mutable and may remain cached because
the Deployment uses `IfNotPresent`. Pin a digest in production. After
publishing a new `latest` image, select the desired rollout behavior and run:

```bash
kubectl rollout restart deployment/litestream-controller-manager \
  -n litestream-controller-system
```

### 3. Install the controller with Kustomize

```bash
kubectl apply -k config/litestream-controller/default
```

This creates the `litestream-controller-system` namespace and installs, in
order: the `Litestream` CRD (`config/litestream-controller/crd`), RBAC
(`config/litestream-controller/rbac`), the controller `Deployment`
(`config/litestream-controller/manager`), the mutating and validating webhook
configurations plus the `Service` (`config/litestream-controller/webhook`), and
the cert-manager `Issuer`/`Certificate`
(`config/litestream-controller/certmanager`). cert-manager's CA injector
populates the webhook's `caBundle` once the `Certificate` is issued, so the
webhook can take a few seconds to become reachable after installation.

The generated `MutatingWebhookConfiguration` defaults to
**`failurePolicy: Fail`**. Its Kustomize patch adds a `namespaceSelector` and
an admission `matchCondition`: only Pods whose
`litestream.mytools.nakatanakatana.app/inject` annotation is non-empty after
trimming whitespace, in namespaces labeled
`litestream.mytools.nakatanakatana.app/injection=enabled`, are sent to the
Pod-injection webhook. Unrelated Pods in labeled namespaces do not depend on
this webhook being available. The generated `ValidatingWebhookConfiguration`
also uses **`failurePolicy: Fail`** for `Litestream` CREATE and UPDATE
requests. Its workload webhook covers Deployment, StatefulSet, and DaemonSet
CREATE/UPDATE requests plus Deployment and StatefulSet `scale` subresources,
and is patched with the same opt-in namespace selector as Pod injection. The
controller namespace is excluded, so the manager can recover if the webhook
Service or its certificate is temporarily unavailable. In an opted-in
namespace, all Deployment, StatefulSet, and DaemonSet create, update, and
scale requests depend on this webhook being available, even when the workload
does not request Litestream injection; the handler allows unrelated workloads
after inspecting them. The `webhook-ignore` overlay only changes Pod injection
and does not make the single-writer validation fail-open. Label a workload
namespace before creating annotated Pods:

```bash
kubectl label namespace <workload-namespace> \
  litestream.mytools.nakatanakatana.app/injection=enabled
```

For an annotated Pod in a labeled namespace, if the webhook is unreachable or
errors, Pod creation is blocked to guarantee that it is never scheduled
without its Litestream sidecar. If you would rather annotated Pods start
unmodified when the webhook is unavailable — at the cost of a Pod occasionally
running without Litestream injected — apply the `webhook-ignore` overlay
instead, which patches `failurePolicy` to `Ignore`:

```bash
kubectl apply -k config/litestream-controller/overlays/webhook-ignore
```

### 4. Create backend credentials as Secrets

Every backend that requires credentials reads them from a `Secret` you
create yourself — the `LitestreamReplica` resource only ever references
`secretKeyRef`s, never raw values:

```bash
kubectl create secret generic litestream-s3-credentials \
  --from-literal=access-key-id=AKIA... \
  --from-literal=secret-access-key='...'
```

Some backends can omit credentials entirely and rely on ambient identity
instead of a Secret (GCS with GKE Workload Identity, Azure with a managed
identity); see the per-backend fields below.

### Workload RBAC

No additional Role or RoleBinding is required for these Secret references.
The kubelet resolves `secretKeyRef` values and projected Secret volumes when
it starts the Pod; it does not use the workload's ServiceAccount to read the
Secret through the Kubernetes API. Grant a workload ServiceAccount access to
Secrets only when the application itself calls the Kubernetes API, following
your cluster's least-privilege policy. Required Secret references still leave
the affected Pod in `CreateContainerConfigError` until the referenced data
exists. Environment-backed references with `secretKeyRef.optional: true` are
rejected during Litestream validation; omit the entire reference when ambient
credentials should be used.

### 5. Apply Replica resources, then a Litestream resource and its Pods

Apply the source and destination Replicas, then one of
`config/litestream-controller/samples/litestream_v1alpha1_restore_only.yaml`,
`config/litestream-controller/samples/litestream_v1alpha1_replicate.yaml`, or
`config/litestream-controller/samples/litestream_v1alpha1_clone_pr.yaml`. Wait
for the `Litestream` to report `Ready`, then apply
`config/litestream-controller/samples/deployment.yaml` or
`config/litestream-controller/samples/statefulset.yaml` (adjusted to your
application's image and container/volume names):

```bash
kubectl apply -f config/litestream-controller/samples/litestream_replica_source.yaml
kubectl apply -f config/litestream-controller/samples/litestream_replica_destination.yaml
kubectl apply -f config/litestream-controller/samples/litestream_v1alpha1_replicate.yaml
kubectl wait --for=condition=Ready litestream/app-db --timeout=60s
kubectl apply -f config/litestream-controller/samples/deployment.yaml
```

For the clone example, apply
`litestream_replica_clone_destination.yaml` instead of the regular destination
sample, after replacing `<number>` with the pull request number, and then
apply `litestream_v1alpha1_clone_pr.yaml`.

A Pod created before its `Litestream` resource is `Ready` is rejected by
the webhook (under the default `failurePolicy: Fail`) with an error
explaining why, rather than started without Litestream injected.

### Breaking migration from `LitestreamDatabase`

For existing installations, migrate in this order:

1. Create or update the source/destination `LitestreamReplica` resources.
2. Apply migrated `Litestream` resources with inline `path` and `restore.replicaRef`.
3. Wait for all migrated resources to become Ready.
4. Delete obsolete `LitestreamDatabase` objects.
5. Remove the obsolete `LitestreamDatabase` CRD during the controller upgrade after confirming no objects remain.

## Annotations

Set these on the **Pod template** (`spec.template.metadata.annotations` of
a Deployment or StatefulSet, or a bare Pod's own annotations) — not on the
`Litestream` resource:

| Annotation | Required | Description |
|---|---|---|
| `litestream.mytools.nakatanakatana.app/inject` | Yes | Name of the `Litestream` resource, in the Pod's own namespace, to inject. |
| `litestream.mytools.nakatanakatana.app/target-container` | No | Selects the application container by name, when a Pod has more than one container. Overrides `spec.injection.targetContainer` on the resource. |
| `litestream.mytools.nakatanakatana.app/volume` | No | Selects the volume mount holding every configured database path, when a Pod has more than one candidate. Overrides `spec.injection.volume` on the resource. |

When a Pod has exactly one container with exactly one volume mount that
contains every database path configured on the resource, both selectors
are optional: the webhook resolves the target automatically. An ambiguous
or missing target is rejected with an error naming every candidate, rather
than guessed.

## Backends

Every backend is configured in `LitestreamReplica.spec.replica`, tagged by
`type`. Each `Litestream` binding chooses its restore source and, when it
replicates, its destination:

| `type` | Spec field | Credentials |
|---|---|---|
| `s3` | `s3` | Optional `credentials.accessKeyID` / `credentials.secretAccessKey` `Secret`s; omit both to use the instance/IRSA role. |
| `gcs` | `gcs` | Optional `serviceAccountJSON` `Secret`; omit to use Workload Identity or another ambient credential. |
| `azure` | `azure` | Optional `accountKey` `Secret`; omit to use a managed identity. |
| `file` | `file` | None — a path on a volume the injected containers already mount. |
| `nats` | `nats` | Any of `username`/`password`, `jwt`+`seed`, `creds`, `nkey`, or `token`, each an optional `Secret`; plus optional `rootCAs`, `clientCert`, `clientKey`. |
| `oss` | `oss` | Optional `accessKeyID` / `accessKeySecret` `Secret`s. |
| `sftp` | `sftp` | Optional `password` and/or `privateKey` `Secret`s. |
| `webdav` | `webdav` | Optional `username` / `password` `Secret`s. |

Exactly one backend block must be set, and it must match `type`. If a resolved
`LitestreamReplica` is invalid, the controller marks its consumer `Litestream`
`Ready=False` and does not publish a ConfigMap.

For credentials, “optional” means omitting the entire `SecretReference` field.
Setting `secretKeyRef.optional: true` is rejected for both environment-backed
credentials and file-backed credentials (`gcs.serviceAccountJSON`, NATS `creds`,
`rootCAs`, `clientCert`, `clientKey`, and `sftp.privateKey`). This prevents an
environment-backed credential from silently falling back to ambient identity
and prevents Litestream from receiving a path to a file that may not exist.

Because the replication sidecar exposes GCS credentials through one
process-wide `GOOGLE_APPLICATION_CREDENTIALS` value, all GCS destination
Replicas referenced through `replicate.replicaRef` in one `Litestream` resource
must either share the same `serviceAccountJSON` Secret reference or all use
ambient credentials.

## Secret startup behavior

Credentials never pass through the controller, the webhook, or any
ConfigMap. The webhook only ever writes a `SecretKeySelector` — a
`Secret` name and key — into either an injected container's environment
(`valueFrom.secretKeyRef`) or a projected volume under
`/etc/litestream-secrets`; the kubelet resolves the actual value at Pod
startup, the same way it resolves any other Pod's Secret references. If a
required referenced `Secret` or key does not exist when the Pod starts, the
kubelet fails that container and the Pod remains in
`CreateContainerConfigError`. An environment-backed reference cannot be
optional; to use ambient identity, omit the entire Secret reference. Required
references still fail container startup when the kubelet cannot resolve the
referenced Secret or key.

## Manual rollout

Applying a changed `Litestream` or `LitestreamReplica` that changes rendered
configuration publishes a new immutable ConfigMap revision. A Replica change
fans out to all consuming Litestream resources. Revision identity includes both
ConfigMap data and the non-secret credential binding metadata that the webhook injects
(`SecretKeySelector` name, key, optional flag, container purpose, environment
variable name, and file mount path), so changing a credential selector also
publishes a new revision without reading or storing a Secret value. Changes
only to the image or injection settings reuse the existing ConfigMap revision;
those settings are still applied to newly created Pods. Owned immutable
ConfigMap revisions are retained for the lifetime of their `Litestream`
resource, so a Pod admitted against an earlier Ready revision can still mount
it; Kubernetes garbage collection removes those revisions with the owner. The
webhook only injects containers **at Pod creation**: it never mutates a running Pod, and existing
Pods keep the image, injection settings, and ConfigMap revision they were
created with. After any change that must reach existing Pods, restart the
owning workload yourself:

```bash
kubectl rollout restart deployment/app
# or
kubectl rollout restart statefulset/app
```

If an existing replicating Deployment still uses the default
`RollingUpdate`, first scale it to zero, change its strategy to `Recreate` (or
set `rollingUpdate.maxSurge: 0`), and then scale it back to one. The workload
webhook rejects an active unsafe rollout to prevent two replication sidecars
from running concurrently. Use `kubectl scale` for this temporary replica-only
change; applying a Deployment update before changing the strategy is rejected.

When handing the destination Replica to another workload, keep the old
workload at `replicas: 0` and wait until its Pods have been deleted before
creating or scaling up the new workload. A terminating Pod is still treated as
active until deletion completes because it may continue writing. A
zero-replica workload is treated as inactive, so the new active workload can
take over the destination. Once the new workload is active, scaling the old
workload back up is rejected until the new workload is scaled down or removed.

A plain `kubectl apply` to the `Litestream` resource is enough only when the
change does not need to reach an existing Pod. A workload restart is required
for every change that should use the latest Litestream configuration.

## Image compatibility

The controller injects the image named by `spec.image` (or, when unset, the
manager's `--default-litestream-image`) and runs two kinds of commands in
it:

- Restore init containers run `/bin/sh <rendered restore script>`; clone
  resources using `require-empty` also need `tr` on `PATH` to inspect the
  format-aware restore plan.
- The replication sidecar runs `litestream replicate -config <rendered config>`.

The image must therefore provide `/bin/sh`, `tr`, and a `litestream` binary on
`PATH`; a custom image must provide all three or injected containers fail to
start. The controller's built-in default is Litestream 0.5.15, pinned to its multi-platform manifest
digest:

```
litestream/litestream@sha256:f45ca298a567bef6edd23d43429b5f80721473a9a9719e467f11d7888999403e
```

When overriding `spec.image`, set `digest` to a full
`sha256:<64 lowercase hexadecimal characters>` value. `repository`
is optional and defaults to the built-in image repository; it must not include
a tag or digest. `tag` is optional metadata for tracking a release, but it is
accepted only alongside `digest` and is rendered as `repository:tag@digest`.
This lets Renovate update the tracked tag and digest while the running image
remains immutable; a tag without a digest is rejected. The `autoRecover` option is rendered as
`auto-recover` in the Litestream configuration and requires Litestream
0.5.7 or newer; the built-in default satisfies this requirement.

## Security contexts

Injected containers default to a hardened `SecurityContext`
(`runAsUser: 65532`, `runAsGroup: 65532`, `runAsNonRoot: true`,
`readOnlyRootFilesystem: true`,
`allowPrivilegeEscalation: false`, and `capabilities.drop: [ALL]`) unless
`spec.injection.containerSecurityContext` overrides a given field. At the
Pod level, only `spec.injection.podSecurityContext.fsGroup` and
`fsGroupChangePolicy` are honored — any other field set there is rejected
at admission, because every other `PodSecurityContext` field would apply to
your application container too, which is not this controller's decision to
make. A Pod's own `securityContext.fsGroup`, if already set, must match the
resource's configured value exactly; a mismatch is also rejected, rather
than silently overridden either way.

### CSI `fsGroup` caveats

`fsGroup` only takes effect on a `PersistentVolumeClaim` if the volume's
CSI driver reports an `fsGroupPolicy` of `ReadWriteOnceWithFSType` or
`File`. Many block-storage CSI drivers report `None` for performance, and
silently ignore `fsGroup` entirely — the application and Litestream
containers then only share file ownership if they already run as the same
UID. Check your storage class's CSI driver's `fsGroupPolicy` before relying
on `fsGroup` with a `PersistentVolumeClaim` (see
`config/litestream-controller/samples/statefulset.yaml`).

### Restore permissions

`spec.injection.permissions.directoryMode` and `databaseMode` are optional.
When either field is empty, the corresponding existing permissions are
preserved; the controller does not emit a `chmod` command.
When a mode is explicitly configured, the restore script applies it and exits
with an error if `chmod` fails.

## Single-writer limits

SQLite allows only one process to write a given database file safely.
Every workload injected with Litestream must therefore run with
**`replicas: 1`** — never scale it horizontally. Running two replicas that
write the same database file risks database corruption. A destination
`LitestreamReplica` must not have concurrent writers: two replication
sidecars writing one Replica path race each other. The workload validating
webhook rejects annotated Deployments and StatefulSets with more than one
replica, rejects annotated DaemonSets, and checks their `scale` subresources
when the referenced Litestream resource configures replication. A Deployment
using replication must also use `strategy.type: Recreate` or set
`strategy.rollingUpdate.maxSurge: 0`; the default RollingUpdate can briefly
run two Pods even when `replicas: 1`. A multi-Pod or active unsafe Deployment
must reference an existing Litestream before the workload is created.
The Pod injection webhook also checks active Pods, including bare Pods and
workloads whose controller type is outside the workload webhook's scope. A
destination is identified by the referenced Replica name and database path;
different database paths may share one Replica backend. Updating a restore-only
Litestream to enable replication is rejected when it would create multiple
writers, including multiple existing workloads that reference the same
destination through different Litestream resources. Give each independent
workload its own database file and its own destination Replica path.

These checks inspect the current API state and are best-effort for simultaneous
admission requests: two creates that race before either object is persisted can
both pass validation. Serialize hand-offs and workload creation when strict
single-writer exclusivity is required.

## PR clone lifecycle

`config/litestream-controller/samples/litestream_v1alpha1_clone_pr.yaml` shows
different source and destination Replica references (`resume-or-create` by
default, see below): restore once from the shared production source, then
replicate ongoing writes to a destination Replica.
For every pull request, apply
`config/litestream-controller/samples/litestream_replica_clone_destination.yaml`
with a unique PR number before applying the clone resource. This gives the
preview a uniquely named destination Replica and path, so it can start from
real data without writing back to (or corrupting) production.

`clonePolicy: resume-or-create` (the default) means redeploying the same PR
preview resumes its own scratch replica if it already exists, instead of
re-cloning from production every time; `require-empty` instead rejects
startup if the destination replica already has data, for workflows that
must never resume a previous run. Neither restore nor replicate, in any
mode, ever deletes existing remote data — see "Uninstalling" below for what
that means when a PR closes.

## Troubleshooting

- **Litestream rejected at creation or update**: the validating webhook found
  an invalid cross-field combination. Inspect the API error; common causes
  include a backend that does not match `replica.type`, a `clonePolicy` without
  distinct source and destination Replica references, duplicate database paths,
  or conflicting GCS credentials.
- **Deployment, StatefulSet, or DaemonSet rejected at creation or update**:
  its Pod template requests a `Litestream` resource with replication, but the
  workload can create multiple Pods, an unsafe rollout, or a different
  workload already uses the same destination Replica and database path. Use
  one active replica, a safe Deployment strategy, and a unique
  Litestream/destination path per workload.
- **Pod rejected at creation, "is not Ready"**: the referenced `Litestream`
  resource has not reported `status.conditions[Ready]=True` yet, or its
  `Ready` condition is stale relative to the resource's current generation.
  Run `kubectl get litestream <name> -o yaml` and check `status.conditions`
  for the actual reason (commonly `InvalidConfiguration`, from a missing or
  invalid referenced Replica — see `status.conditions[].message`).
- **Pod rejected, "no mount contains all database paths" / "ambiguous
  target candidates"**: the webhook could not find exactly one container
  volume mount covering every configured database path. Set the
  `target-container` and `volume` annotations explicitly (see Annotations
  above).
- **Pod stuck `CreateContainerConfigError`**: an injected container has a
  required reference to a `Secret` or key that does not exist. Check
  `kubectl describe pod` for the missing reference, then create or fix the
  `Secret`. Omit a credential reference to use ambient credentials. See Secret
  startup behavior above.
- **A change to a `Litestream` or Replica never reaches a running Pod**: the
  webhook only injects at Pod creation. Restart the owning workload — see
  Manual rollout above.
- **Replication sidecar never starts, or blocks Pod startup indefinitely**:
  the cluster is likely older than Kubernetes 1.29, so the injected native
  sidecar is treated as an ordinary init container instead. See
  Requirements above.

## Uninstalling

```bash
kubectl delete -k config/litestream-controller/default
```

removes the controller, its webhook, its RBAC, and the CRDs (which delete
every `Litestream` and `LitestreamReplica` resource, along with the ConfigMaps
the controller owns).
Deleting the CRD does **not** touch the Pods it was injected into, and it
does not delete any remote replica data: Litestream, the controller, and
the webhook only ever write to configured replica paths — none of them
ever deletes objects from a replica backend, at uninstall or otherwise. If
you no longer need a replica's data (for example, after closing a PR
preview environment created from `litestream_v1alpha1_clone_pr.yaml`), you
must delete the objects at that replica's path directly in the backend
yourself, for example:

```bash
aws s3 rm --recursive "s3://my-app-backups/clone-preview/pr-123"
```

before or after deleting the `Litestream` resource that wrote them — the
controller performs no cleanup of its own.
