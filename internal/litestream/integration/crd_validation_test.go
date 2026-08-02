package integration_test

import (
	"context"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mismatchedLitestreamReplica sets Type to s3 but configures the file backend
// instead, the exact "replica type/backend mismatch" this file exercises.
func mismatchedLitestreamReplica(name, namespace string) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
			Type: v1alpha1.ReplicaTypeS3,
			File: &v1alpha1.FileReplicaSpec{Path: "/data/replica"},
		}},
	}
}

// TestValidatorRejectsMismatchBeforePersistence verifies that the validating
// admission webhook rejects cross-field-invalid resources before the API
// server can persist them.
func TestValidatorRejectsMismatchBeforePersistence(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)
	cr := mismatchedLitestreamReplica("bad", namespace)
	err := k8sClient.Create(ctx, cr)
	assert.Assert(t, err != nil, "expected the validating webhook to reject the mismatched backend")

	var persisted v1alpha1.LitestreamReplica
	err = k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &persisted)
	assert.Assert(t, apierrors.IsNotFound(err), "rejected resources must not be persisted: %v", err)
}

func TestAPIRejectsWhitespaceOnlyBackendValue(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	replica := validReplica("blank-backend", namespace, "   ")

	err := k8sClient.Create(ctx, replica)
	assert.Assert(t, apierrors.IsInvalid(err), "the CRD should reject whitespace-only required backend values: %v", err)
}

func TestAPIRejectsTaggedImageRepositoryWithDigest(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("tagged-image", namespace)
	cr.Spec.Image = v1alpha1.ImageSpec{
		Repository: "registry.example.com/litestream:latest",
		Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}

	err := k8sClient.Create(ctx, cr)
	assert.Assert(t, apierrors.IsInvalid(err), "the CRD should reject tagged image repositories: %v", err)
}

func TestAPIAcceptsTagWithDigest(t *testing.T) {
	ctx := context.Background()
	namespace := newTestNamespace(t)

	cr := validLitestreamCR("tagged-image-valid", namespace)
	cr.Spec.Image = v1alpha1.ImageSpec{
		Repository: "registry.example.com/litestream",
		Tag:        "latest",
		Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}

	createValidDependencies(t, ctx, namespace)
	assert.NilError(t, k8sClient.Create(ctx, cr))
	got := waitForReadyCondition(t, namespace, cr.Name, metav1.ConditionTrue)
	assert.Equal(t, got.Spec.Image.Tag, "latest")
}
