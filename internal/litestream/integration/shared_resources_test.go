package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/webhook"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// TestSharedResourcesRenderAndPropagateChanges catches a controller that
// renders profiles from inline data, fails to fan out a source change, or
// fans out a destination-only change to unrelated profiles.
func TestSharedResourcesRenderAndPropagateChanges(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	graph := newSharedResourceGraph(namespace)
	createSharedResourceGraph(t, ctx, graph)

	first := waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionTrue)
	second := waitForReadyCondition(t, namespace, graph.secondProfile.Name, metav1.ConditionTrue)
	firstConfig := configMapForProfile(t, ctx, &first)
	secondConfig := configMapForProfile(t, ctx, &second)
	assert.Assert(t, strings.Contains(configMapContents(firstConfig), "shared-source-v1"))
	assert.Assert(t, strings.Contains(configMapContents(secondConfig), "shared-source-v1"))
	assert.Assert(t, strings.Contains(configMapContents(firstConfig), "destination-one-v1"))
	assert.Assert(t, strings.Contains(configMapContents(secondConfig), "destination-two-v1"))

	updateReplicaBucket(t, ctx, graph.source.Name, namespace, "shared-source-v2")
	firstAfterSource := waitForConfigChange(t, namespace, first)
	secondAfterSource := waitForConfigChange(t, namespace, second)
	assert.Assert(t, strings.Contains(configMapContents(configMapForProfile(t, ctx, &firstAfterSource)), "shared-source-v2"))
	assert.Assert(t, strings.Contains(configMapContents(configMapForProfile(t, ctx, &secondAfterSource)), "shared-source-v2"))

	updateReplicaBucket(t, ctx, graph.firstDestination.Name, namespace, "destination-one-v2")
	firstAfterDestination := waitForConfigChange(t, namespace, firstAfterSource)
	assert.Assert(t, strings.Contains(configMapContents(configMapForProfile(t, ctx, &firstAfterDestination)), "destination-one-v2"))
	assertLitestreamUnchangedFor(t, ctx, namespace, secondAfterSource, time.Second)
}

// TestDestinationOnlyReplicateRendersWithoutSource catches a resolver that
// requires a restore source for a destination-only replication binding.
func TestDestinationOnlyReplicateRendersWithoutSource(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	destination := validReplica("destination-only", namespace, "destination-only-v1")
	profile := destinationOnlyProfile("destination-only", namespace, destination.Name)

	assert.NilError(t, k8sClient.Create(ctx, destination))
	assert.NilError(t, k8sClient.Create(ctx, profile))

	ready := waitForReadyCondition(t, namespace, profile.Name, metav1.ConditionTrue)
	contents := configMapContents(configMapForProfile(t, ctx, &ready))
	assert.Assert(t, strings.Contains(contents, "/var/lib/app/destination-only.db"))
	assert.Assert(t, strings.Contains(contents, "destination-only-v1"))
}

// TestSharedDependencyFailureRecoveryAndDeletionProtection catches resolver
// failures that do not clear readiness or watches that fail to reconcile a
// profile after its missing source appears. It also exercises the real
// validating webhook's protection of shared resources in use by a profile.
func TestSharedDependencyFailureRecoveryAndDeletionProtection(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	graph := newSharedResourceGraph(namespace)
	assert.NilError(t, k8sClient.Create(ctx, graph.firstDestination))
	assert.NilError(t, k8sClient.Create(ctx, graph.firstProfile))

	missing := waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionFalse)
	assert.Equal(t, missing.Status.ConfigMapName, "")
	assert.Equal(t, missing.Status.ConfigHash, "")

	notReadyPod := targetPodWithVolumes("not-ready", namespace, graph.firstProfile.Name,
		corev1.VolumeMount{Name: "database", MountPath: "/var/lib/app"},
	)
	err := k8sClient.Create(ctx, notReadyPod)
	assert.Assert(t, err != nil, "expected Pod admission to reject a profile that is not Ready")
	var denied corev1.Pod
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: notReadyPod.Name}, &denied)
	assert.Assert(t, apierrors.IsNotFound(err), "denied Pod must not be persisted: %v", err)

	assert.NilError(t, k8sClient.Create(ctx, graph.source))
	recovered := waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionTrue)
	assert.Assert(t, recovered.Status.ConfigMapName != "")
	readyPod := targetPodWithVolumes("recovered", namespace, graph.firstProfile.Name,
		corev1.VolumeMount{Name: "database", MountPath: "/var/lib/app"},
	)
	assert.NilError(t, k8sClient.Create(ctx, readyPod))

	sourceErr := k8sClient.Delete(ctx, graph.source)
	assert.Assert(t, apierrors.IsForbidden(sourceErr), "expected referenced source replica deletion to be forbidden: %v", sourceErr)
	assert.ErrorContains(t, sourceErr, graph.source.Name)
	assert.ErrorContains(t, sourceErr, graph.firstProfile.Name)
	var sourceStillExists v1alpha1.LitestreamReplica
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: graph.source.Name}, &sourceStillExists))
}

// TestMissingDestinationReplicaRecovers catches a dependency watch that does
// not reconcile a profile after its missing replication destination appears.
func TestMissingDestinationReplicaRecovers(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	graph := newSharedResourceGraph(namespace)

	assert.NilError(t, k8sClient.Create(ctx, graph.source))
	assert.NilError(t, k8sClient.Create(ctx, graph.firstProfile))

	missing := waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionFalse)
	assert.Equal(t, missing.Status.ConfigMapName, "")
	assert.Equal(t, missing.Status.ConfigHash, "")

	assert.NilError(t, k8sClient.Create(ctx, graph.firstDestination))
	recovered := waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionTrue)
	assert.Assert(t, strings.Contains(configMapContents(configMapForProfile(t, ctx, &recovered)), "destination-one-v1"))
}

// TestInvalidSharedReplicaIsRejectedBeforeProfileRecovers catches a webhook
// that persists an invalid shared replica, and confirms that a profile waiting
// on that reference becomes Ready when a valid replacement is created.
func TestInvalidSharedReplicaIsRejectedBeforeProfileRecovers(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	graph := newSharedResourceGraph(namespace)
	graph.firstProfile.Spec.Databases[0].Restore.ReplicaRef.Name = "recoverable-source"

	assert.NilError(t, k8sClient.Create(ctx, graph.firstDestination))
	assert.NilError(t, k8sClient.Create(ctx, graph.firstProfile))
	waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionFalse)

	invalid := validReplica("recoverable-source", namespace, "unused")
	invalid.Spec.Replica.S3 = nil
	assert.Assert(t, k8sClient.Create(ctx, invalid) != nil, "expected invalid referenced replica to be rejected")

	var absent v1alpha1.LitestreamReplica
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: invalid.Name}, &absent)
	assert.Assert(t, apierrors.IsNotFound(err), "rejected replica must not persist: %v", err)

	assert.NilError(t, k8sClient.Create(ctx, validReplica(invalid.Name, namespace, "recovered-source")))
	waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionTrue)
}

// TestPodAdmissionRequiresCurrentConfigMapAndUsesResolvedDatabaseVolume
// catches admission that accepts stale controller output or chooses a volume
// without considering the inline database path.
func TestPodAdmissionRequiresCurrentConfigMapAndUsesResolvedDatabaseVolume(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	graph := newSharedResourceGraph(namespace)
	createSharedResourceGraph(t, ctx, graph)
	profile := waitForReadyCondition(t, namespace, graph.firstProfile.Name, metav1.ConditionTrue)

	configMap := configMapForProfile(t, ctx, &profile)
	originalData := configMap.DeepCopy().Data
	staleData := make(map[string]string, len(originalData))
	for key := range originalData {
		staleData[key] = "stale"
	}
	replaceImmutableConfigMap(t, ctx, configMap, staleData)
	stalePod := targetPodWithVolumes("stale", namespace, profile.Name,
		corev1.VolumeMount{Name: "parent", MountPath: "/var/lib"},
		corev1.VolumeMount{Name: "database", MountPath: "/var/lib/app"},
	)
	err := k8sClient.Create(ctx, stalePod)
	assert.Assert(t, err != nil, "expected stale ConfigMap admission to be denied")

	var denied corev1.Pod
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: stalePod.Name}, &denied)
	assert.Assert(t, apierrors.IsNotFound(err), "denied Pod must not be persisted: %v", err)

	replaceImmutableConfigMap(t, ctx, configMap, originalData)

	readyPod := targetPodWithVolumes("ready", namespace, profile.Name,
		corev1.VolumeMount{Name: "parent", MountPath: "/var/lib"},
		corev1.VolumeMount{Name: "database", MountPath: "/var/lib/app"},
	)
	assert.NilError(t, k8sClient.Create(ctx, readyPod))

	var persisted corev1.Pod
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: readyPod.Name}, &persisted))
	assert.Equal(t, injectedVolumeName(t, &persisted, webhook.RestoreContainerNamePrefix+"app"), "database")
}

type sharedResourceGraph struct {
	source            *v1alpha1.LitestreamReplica
	firstDestination  *v1alpha1.LitestreamReplica
	secondDestination *v1alpha1.LitestreamReplica
	firstProfile      *v1alpha1.Litestream
	secondProfile     *v1alpha1.Litestream
}

func newSharedResourceGraph(namespace string) *sharedResourceGraph {
	source := validReplica("shared-source", namespace, "shared-source-v1")
	firstDestination := validReplica("destination-one", namespace, "destination-one-v1")
	secondDestination := validReplica("destination-two", namespace, "destination-two-v1")
	return &sharedResourceGraph{
		source:            source,
		firstDestination:  firstDestination,
		secondDestination: secondDestination,
		firstProfile:      sharedProfile("profile-one", namespace, source.Name, firstDestination.Name),
		secondProfile:     sharedProfile("profile-two", namespace, source.Name, secondDestination.Name),
	}
}

func sharedProfile(name, namespace, source, destination string) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app",
			Path: "/var/lib/app/app.db",
			Restore: &v1alpha1.RestoreSpec{
				ReplicaRef: corev1.LocalObjectReference{Name: source},
			},
			Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: corev1.LocalObjectReference{Name: destination}},
		}}},
	}
}

func destinationOnlyProfile(name, namespace, destination string) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name:      "destination-only",
			Path:      "/var/lib/app/destination-only.db",
			Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: corev1.LocalObjectReference{Name: destination}},
		}}},
	}
}

func createSharedResourceGraph(t *testing.T, ctx context.Context, graph *sharedResourceGraph) {
	t.Helper()
	assert.NilError(t, k8sClient.Create(ctx, graph.source))
	assert.NilError(t, k8sClient.Create(ctx, graph.firstDestination))
	assert.NilError(t, k8sClient.Create(ctx, graph.secondDestination))
	assert.NilError(t, k8sClient.Create(ctx, graph.firstProfile))
	assert.NilError(t, k8sClient.Create(ctx, graph.secondProfile))
}

func configMapForProfile(t *testing.T, ctx context.Context, profile *v1alpha1.Litestream) *corev1.ConfigMap {
	t.Helper()
	assert.Assert(t, profile.Status.ConfigMapName != "", "expected profile %q to publish a ConfigMap", profile.Name)
	configMap := &corev1.ConfigMap{}
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: profile.Namespace, Name: profile.Status.ConfigMapName}, configMap))
	return configMap
}

func configMapContents(configMap *corev1.ConfigMap) string {
	contents := make([]string, 0, len(configMap.Data))
	for _, value := range configMap.Data {
		contents = append(contents, value)
	}
	return strings.Join(contents, "\n")
}

func assertLitestreamUnchangedFor(t *testing.T, ctx context.Context, namespace string, before v1alpha1.Litestream, duration time.Duration) {
	t.Helper()
	observationCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-observationCtx.Done():
			return
		default:
		}
		var current v1alpha1.Litestream
		err := k8sClient.Get(observationCtx, types.NamespacedName{Namespace: namespace, Name: before.Name}, &current)
		if err != nil {
			if observationCtx.Err() != nil {
				return
			}
			t.Fatalf("get Litestream %s/%s during negative observation: %v", namespace, before.Name, err)
		}
		assert.Equal(t, current.Status.ConfigHash, before.Status.ConfigHash)
		assert.Equal(t, current.Status.ConfigMapName, before.Status.ConfigMapName)
		select {
		case <-observationCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

func replaceImmutableConfigMap(t *testing.T, ctx context.Context, original *corev1.ConfigMap, data map[string]string) {
	t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		deleteTarget := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: original.Name, Namespace: original.Namespace}}
		err := k8sClient.Delete(ctx, deleteTarget)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("delete ConfigMap %s/%s before replacement: %v", original.Namespace, original.Name, err)
		}

		replacement := original.DeepCopy()
		replacement.ResourceVersion = ""
		replacement.UID = ""
		replacement.Generation = 0
		replacement.CreationTimestamp = metav1.Time{}
		replacement.DeletionTimestamp = nil
		replacement.DeletionGracePeriodSeconds = nil
		replacement.ManagedFields = nil
		replacement.Data = cloneConfigMapData(data)
		replacement.Immutable = ptr.To(true)
		err = k8sClient.Create(ctx, replacement)
		if err == nil {
			return
		}
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("recreate ConfigMap %s/%s: %v", original.Namespace, original.Name, err)
		}
	}
	t.Fatalf("could not replace immutable ConfigMap %s/%s after bounded retries", original.Namespace, original.Name)
}

func cloneConfigMapData(data map[string]string) map[string]string {
	clone := make(map[string]string, len(data))
	for key, value := range data {
		clone[key] = value
	}
	return clone
}

func waitForConfigChange(t *testing.T, namespace string, before v1alpha1.Litestream) v1alpha1.Litestream {
	t.Helper()
	var changed v1alpha1.Litestream
	waitFor(t, "Litestream "+namespace+"/"+before.Name+" ConfigMap update", func(ctx context.Context) (bool, error) {
		changed = currentLitestream(t, ctx, namespace, before.Name)
		return changed.Status.ConfigHash != "" && changed.Status.ConfigHash != before.Status.ConfigHash, nil
	})
	return changed
}

func updateReplicaBucket(t *testing.T, ctx context.Context, name, namespace, bucket string) {
	t.Helper()
	var replica v1alpha1.LitestreamReplica
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &replica))
	replica.Spec.Replica.S3.Bucket = bucket
	assert.NilError(t, k8sClient.Update(ctx, &replica))
}

func targetPodWithVolumes(name, namespace, profile string, mounts ...corev1.VolumeMount) *corev1.Pod {
	volumes := make([]corev1.Volume, 0, len(mounts))
	for _, mount := range mounts {
		volumes = append(volumes, corev1.Volume{Name: mount.Name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: map[string]string{webhook.InjectAnnotation: profile}},
		Spec: corev1.PodSpec{
			Volumes:    volumes,
			Containers: []corev1.Container{{Name: "app", Image: "app:1.0.0", VolumeMounts: mounts}},
		},
	}
}

func injectedVolumeName(t *testing.T, pod *corev1.Pod, containerName string) string {
	t.Helper()
	for _, container := range pod.Spec.InitContainers {
		if container.Name != containerName {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.MountPath == "/var/lib/app" {
				return mount.Name
			}
		}
	}
	t.Fatalf("injected container %q does not mount /var/lib/app", containerName)
	return ""
}
