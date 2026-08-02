package webhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestDependencyValidatorValidatesReplicaStructureWithoutResolvingReferences(t *testing.T) {
	for _, test := range []struct {
		name      string
		resource  runtime.Object
		operation admissionv1.Operation
		allowed   bool
		errorText string
	}{
		{
			name:      "allows replica update without checking external resources",
			resource:  dependencyValidatorReplica("replica", "team-a"),
			operation: admissionv1.Update,
			allowed:   true,
		},
		{
			name: "rejects replica create with an invalid backend",
			resource: &v1alpha1.LitestreamReplica{
				ObjectMeta: metav1.ObjectMeta{Name: "replica", Namespace: "team-a"},
				Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
					Type: v1alpha1.ReplicaTypeS3,
				}},
			},
			operation: admissionv1.Create,
			errorText: "spec.replica",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := newDependencyValidator(t)

			response := validator.Handle(t.Context(), dependencyAdmissionRequest(t, test.resource, test.operation))

			assert.Equal(t, response.Allowed, test.allowed, "%v", response.Result)
			if test.errorText != "" {
				assert.Assert(t, response.Result != nil)
				assert.Assert(t, strings.Contains(response.Result.Message, test.errorText), response.Result.Message)
			}
		})
	}
}

func TestDependencyValidatorProtectsDirectReplicaDeletion(t *testing.T) {
	source := dependencyValidatorReplica("source", "team-a")
	destination := dependencyValidatorReplica("destination", "team-a")
	unreferenced := dependencyValidatorReplica("unreferenced", "team-a")
	consumer := dependencyValidatorConsumer("api", "team-a", source.Name, destination.Name)
	validator := newDependencyValidator(t, consumer)

	for _, test := range []struct {
		name     string
		resource *v1alpha1.LitestreamReplica
		allowed  bool
	}{
		{name: "denies a restore source", resource: source},
		{name: "denies a replicate destination", resource: destination},
		{name: "allows an unreferenced replica", resource: unreferenced, allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := validator.Handle(t.Context(), dependencyDeleteRequest(t, test.resource))

			assert.Equal(t, response.Allowed, test.allowed, "%v", response.Result)
			if !test.allowed {
				assert.Assert(t, response.Result != nil)
				assert.Assert(t, strings.Contains(response.Result.Message, "api"), response.Result.Message)
			}
		})
	}
}

func TestDependencyValidatorIgnoresSameNamedReplicaReferenceInAnotherNamespace(t *testing.T) {
	replica := dependencyValidatorReplica("source", "team-a")
	otherNamespaceConsumer := dependencyValidatorConsumer("api", "team-b", replica.Name, "destination")
	validator := newDependencyValidator(t, otherNamespaceConsumer)

	response := validator.Handle(t.Context(), dependencyDeleteRequest(t, replica))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func newDependencyValidator(t *testing.T, objects ...client.Object) *DependencyValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &DependencyValidator{Reader: reader, Decoder: admission.NewDecoder(scheme)}
}

func dependencyAdmissionRequest(t *testing.T, resource runtime.Object, operation admissionv1.Operation) admission.Request {
	t.Helper()
	raw, err := json.Marshal(resource)
	assert.NilError(t, err)
	object := resource.(client.Object)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "00000000-0000-0000-0000-000000000000",
		Operation: operation,
		Namespace: object.GetNamespace(),
		Kind:      dependencyKind(resource),
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func dependencyDeleteRequest(t *testing.T, resource runtime.Object) admission.Request {
	t.Helper()
	raw, err := json.Marshal(resource)
	assert.NilError(t, err)
	object := resource.(client.Object)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "00000000-0000-0000-0000-000000000000",
		Operation: admissionv1.Delete,
		Namespace: object.GetNamespace(),
		Kind:      dependencyKind(resource),
		OldObject: runtime.RawExtension{Raw: raw},
	}}
}

func dependencyKind(resource runtime.Object) metav1.GroupVersionKind {
	switch resource.(type) {
	case *v1alpha1.LitestreamReplica:
		return metav1.GroupVersionKind{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Kind: "LitestreamReplica"}
	default:
		panic("unsupported dependency test resource")
	}
}

func dependencyValidatorReplica(name, namespace string) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		TypeMeta:   metav1.TypeMeta{Kind: "LitestreamReplica", APIVersion: v1alpha1.GroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
			Type: v1alpha1.ReplicaTypeS3,
			S3:   &v1alpha1.S3ReplicaSpec{Bucket: "bucket", Path: name},
		}},
	}
}

func dependencyValidatorConsumer(name, namespace, source, destination string) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name:    "app",
			Path:    "/data/app.db",
			Restore: &v1alpha1.RestoreSpec{ReplicaRef: corev1.LocalObjectReference{Name: source}},
			Replicate: &v1alpha1.ReplicateSpec{
				ReplicaRef: corev1.LocalObjectReference{Name: destination},
			},
		}}},
	}
}
