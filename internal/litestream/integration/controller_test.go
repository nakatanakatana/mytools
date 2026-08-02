package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/controller"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// validLitestreamCR returns a profile that uses direct replica references
// created by createValidDependencies.
func validLitestreamCR(name, namespace string) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamSpec{
			Databases: []v1alpha1.DatabaseBinding{
				{
					Name: "app",
					Path: "/var/lib/app/app.db",
					Restore: &v1alpha1.RestoreSpec{
						ReplicaRef: corev1.LocalObjectReference{Name: "app-source"},
					},
					Replicate: &v1alpha1.ReplicateSpec{
						ReplicaRef: corev1.LocalObjectReference{Name: "app-destination"},
					},
				},
			},
		},
	}
}

func validReplica(name, namespace, bucket string) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
			Type: v1alpha1.ReplicaTypeS3,
			S3:   &v1alpha1.S3ReplicaSpec{Bucket: bucket, Path: name},
		}},
	}
}

func createValidDependencies(t *testing.T, ctx context.Context, namespace string) {
	t.Helper()
	assert.NilError(t, k8sClient.Create(ctx, validReplica("app-source", namespace, "source-bucket")))
	assert.NilError(t, k8sClient.Create(ctx, validReplica("app-destination", namespace, "destination-bucket")))
}

func resolvedConfig(t *testing.T, ctx context.Context, resource *v1alpha1.Litestream) litestreamconfig.RenderedConfig {
	t.Helper()
	input, err := (resolver.Resolver{Reader: k8sClient}).Resolve(ctx, resource)
	assert.NilError(t, err)
	rendered, err := litestreamconfig.Render(input)
	assert.NilError(t, err)
	return rendered
}

// waitForReadyCondition polls the Litestream resource named name in
// namespace until it reports a Ready condition with the given status, and
// returns the observed resource.
func waitForReadyCondition(t *testing.T, namespace, name string, status metav1.ConditionStatus) v1alpha1.Litestream {
	t.Helper()
	var got v1alpha1.Litestream
	waitFor(t, "Litestream "+namespace+"/"+name+" Ready="+string(status), func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &got); err != nil {
			return false, nil //nolint:nilerr // retry: the reconciler may not have observed the create yet
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, controller.ReadyConditionType)
		return cond != nil && cond.Status == status, nil
	})
	return got
}

func TestReconcilerCreatesConfigMapAndMarksReady(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("app", namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))

	got := waitForReadyCondition(t, namespace, cr.Name, metav1.ConditionTrue)

	cond := meta.FindStatusCondition(got.Status.Conditions, controller.ReadyConditionType)
	assert.Equal(t, cond.Reason, controller.ReasonConfigRendered)
	assert.Equal(t, got.Status.ObservedGeneration, got.Generation)
	assert.Assert(t, got.Status.ConfigMapName != "")

	rendered := resolvedConfig(t, ctx, &got)
	wantName := controller.ConfigMapNameForHash(&got, rendered.Hash)
	assert.Equal(t, got.Status.ConfigMapName, wantName)

	var cm corev1.ConfigMap
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{Name: wantName, Namespace: namespace}, &cm))

	assert.DeepEqual(t, cm.Data, rendered.Data)
	assert.Equal(t, got.Status.ConfigHash, rendered.Hash)

	assert.Equal(t, cm.Labels[controller.LabelManagedBy], controller.ManagedByValue)
	assert.Equal(t, cm.Labels[controller.LabelResourceName], cr.Name)

	assert.Equal(t, len(cm.OwnerReferences), 1)
	owner := cm.OwnerReferences[0]
	assert.Equal(t, owner.Name, cr.Name)
	assert.Equal(t, owner.UID, got.UID)
	assert.Assert(t, owner.Controller != nil && *owner.Controller)
}

func TestReconcilerHandlesMaximumResourceName(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	name := strings.Join([]string{
		strings.Repeat("a", 63),
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
		strings.Repeat("d", 60),
	}, ".")
	cr := validLitestreamCR(name, namespace)
	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))

	got := waitForReadyCondition(t, namespace, name, metav1.ConditionTrue)
	rendered := resolvedConfig(t, ctx, &got)
	var cm corev1.ConfigMap
	assert.NilError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      controller.ConfigMapNameForHash(&got, rendered.Hash),
		Namespace: namespace,
	}, &cm))

	label := cm.Labels[controller.LabelResourceName]
	assert.Assert(t, len(label) <= validation.LabelValueMaxLength)
	assert.Equal(t, len(validation.IsValidLabelValue(label)), 0)
	assert.Equal(t, cm.Annotations[controller.AnnotationResourceName], name)
}
