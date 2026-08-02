package integration_test

import (
	"context"
	"testing"

	webhookpkg "github.com/nakatanakatana/mytools/internal/litestream/webhook"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// targetPod is a Pod annotated to request injection of resourceName,
// mounting a "data" volume wide enough to hold the CR's database path.
func targetPod(name, namespace, resourceName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{webhookpkg.InjectAnnotation: resourceName},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			Containers: []corev1.Container{{
				Name:         "app",
				Image:        "app:1.0.0",
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/app"}},
			}},
		},
	}
}

// TestAdmissionWebhookMutatesPodReferencingReadyResource creates a real
// Litestream CR, waits for the reconciler to mark it Ready, then persists
// a Pod referencing it through the API server. The API server dispatches the
// real AdmissionReview to the manager's TLS webhook server, so this covers
// registration, TLS, routing, and the returned mutation.
func TestAdmissionWebhookMutatesPodReferencingReadyResource(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("app", namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))
	waitForReadyCondition(t, namespace, cr.Name, metav1.ConditionTrue)

	pod := targetPod("app", namespace, cr.Name)
	assert.NilError(t, k8sClient.Create(ctx, pod))

	var persisted corev1.Pod
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, &persisted))

	var foundRestore, foundSidecar bool
	for _, c := range persisted.Spec.InitContainers {
		switch c.Name {
		case webhookpkg.RestoreContainerNamePrefix + "app":
			foundRestore = true
		case webhookpkg.ReplicateContainerName:
			foundSidecar = true
			assert.Assert(t, c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways,
				"the replication sidecar must be a native sidecar")
		}
	}
	assert.Assert(t, foundRestore, "expected a restore init container for database %q", "app")
	assert.Assert(t, foundSidecar, "expected a replication sidecar since the database replicates")

	found := false
	for _, v := range persisted.Spec.Volumes {
		if v.Name == webhookpkg.ConfigVolumeName {
			found = true
			assert.Assert(t, v.ConfigMap != nil)
		}
	}
	assert.Assert(t, found, "expected the generated ConfigMap to be mounted")
}

// TestAdmissionWebhookDeniesPodReferencingMissingResource confirms the
// admission server denies, rather than silently allowing, a Pod whose
// annotation names a Litestream resource that does not exist.
func TestAdmissionWebhookDeniesPodReferencingMissingResource(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	pod := targetPod("app", namespace, "does-not-exist")
	err := k8sClient.Create(ctx, pod)
	assert.Assert(t, err != nil, "expected the admission webhook to deny the request")

	var persisted corev1.Pod
	err = k8sClient.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: namespace}, &persisted)
	assert.Assert(t, apierrors.IsNotFound(err), "denied Pods must not be persisted: %v", err)
}
