package integration_test

import (
	"context"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	webhookpkg "github.com/nakatanakatana/mytools/internal/litestream/webhook"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestAdmissionWebhookDeniesPodUsingSameDestinationThroughDifferentLitestream(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	first := validLitestreamCR("first", namespace)
	second := validLitestreamCR("second", namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, first))
	assert.NilError(t, k8sClient.Create(ctx, second))
	waitForReadyCondition(t, namespace, first.Name, metav1.ConditionTrue)
	waitForReadyCondition(t, namespace, second.Name, metav1.ConditionTrue)

	firstPod := targetPod("first", namespace, first.Name)
	assert.NilError(t, k8sClient.Create(ctx, firstPod))

	secondPod := targetPod("second", namespace, second.Name)
	err := k8sClient.Create(ctx, secondPod)
	assert.Assert(t, err != nil, "expected a Pod using the same destination through another Litestream to be denied")

	var persisted corev1.Pod
	err = k8sClient.Get(ctx, types.NamespacedName{Name: secondPod.Name, Namespace: namespace}, &persisted)
	assert.Assert(t, apierrors.IsNotFound(err), "denied Pod must not be persisted: %v", err)
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

// TestAdmissionWebhookDeniesReplicatedMultiPodWorkloads prevents multiple
// Litestream sidecars from writing the same destination Replica path.
func TestAdmissionWebhookDeniesReplicatedMultiPodWorkloads(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("app", namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))

	replicas := int32(2)
	workload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{webhookpkg.InjectAnnotation: cr.Name},
					Labels:      map[string]string{"app": "app"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0.0"}}},
			},
		},
	}

	err := k8sClient.Create(ctx, workload)
	assert.Assert(t, err != nil, "expected a replicated workload to be denied")
	assert.ErrorContains(t, err, "replication requires a single writer")

	var persisted appsv1.Deployment
	err = k8sClient.Get(ctx, types.NamespacedName{Name: workload.Name, Namespace: namespace}, &persisted)
	assert.Assert(t, apierrors.IsNotFound(err), "denied workload must not be persisted: %v", err)
}

func TestAdmissionWebhookDeniesSecondWorkloadUsingSameLitestream(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("app", namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))

	newWorkload := func(name string) *appsv1.Deployment {
		replicas := int32(1)
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{webhookpkg.InjectAnnotation: cr.Name},
						Labels:      map[string]string{"app": name},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0.0"}}},
				},
			},
		}
	}

	assert.NilError(t, k8sClient.Create(ctx, newWorkload("first")))
	second := newWorkload("second")
	err := k8sClient.Create(ctx, second)
	assert.Assert(t, err != nil, "expected a second workload using the same Litestream to be denied")
	assert.ErrorContains(t, err, "already uses the same destination Replica")
}

func TestAdmissionWebhookDeniesReplicatedScaleSubresource(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("app", namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))
	waitForReadyCondition(t, namespace, cr.Name, metav1.ConditionTrue)

	replicas := int32(1)
	workload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{webhookpkg.InjectAnnotation: cr.Name},
					Labels:      map[string]string{"app": "app"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0.0"}}},
			},
		},
	}
	assert.NilError(t, k8sClient.Create(ctx, workload))

	scale := &autoscalingv1.Scale{
		TypeMeta:   metav1.TypeMeta{APIVersion: "autoscaling/v1", Kind: "Scale"},
		ObjectMeta: metav1.ObjectMeta{Name: workload.Name, Namespace: namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: 2},
	}
	err := k8sClient.SubResource("scale").Update(ctx, workload, client.WithSubResourceBody(scale))
	assert.Assert(t, err != nil, "expected a scale request to multiple Pods to be denied")
	assert.ErrorContains(t, err, "single writer")

	var persisted appsv1.Deployment
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Name: workload.Name, Namespace: namespace}, &persisted))
	assert.Equal(t, *persisted.Spec.Replicas, int32(1))
}

func TestAdmissionWebhookDeniesEnablingReplicationWithExistingMultiplePodWorkload(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name:    "app",
			Path:    "/var/lib/app/app.db",
			Restore: &v1alpha1.RestoreSpec{ReplicaRef: corev1.LocalObjectReference{Name: "app-source"}},
		}}},
	}
	assert.NilError(t, k8sClient.Create(ctx, validReplica("app-source", namespace, "source-bucket")))
	assert.NilError(t, k8sClient.Create(ctx, cr))
	waitForReadyCondition(t, namespace, cr.Name, metav1.ConditionTrue)

	replicas := int32(2)
	workload := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{webhookpkg.InjectAnnotation: cr.Name},
					Labels:      map[string]string{"app": "app"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0.0"}}},
			},
		},
	}
	assert.NilError(t, k8sClient.Create(ctx, workload))

	var update v1alpha1.Litestream
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name, Namespace: namespace}, &update))
	update.Spec.Databases[0].Replicate = &v1alpha1.ReplicateSpec{ReplicaRef: corev1.LocalObjectReference{Name: "app-destination"}}
	err := k8sClient.Update(ctx, &update)
	assert.Assert(t, err != nil, "expected enabling replication to be denied")
	assert.ErrorContains(t, err, "existing workload")

	var persisted v1alpha1.Litestream
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name, Namespace: namespace}, &persisted))
	assert.Assert(t, persisted.Spec.Databases[0].Replicate == nil)
}
