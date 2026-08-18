package webhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestLitestreamValidatorRejectsCrossFieldValidationErrors(t *testing.T) {
	validator := newLitestreamValidator(t)
	resource := &v1alpha1.Litestream{
		TypeMeta: metav1.TypeMeta{Kind: "Litestream", APIVersion: v1alpha1.GroupVersion.String()},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app", Path: "/data/app.db", ClonePolicy: v1alpha1.ClonePolicyRequireEmpty,
		}}},
	}

	response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, resource, admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected an invalid Litestream spec to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "restore and replicate are required"), response.Result.Message)
}

func TestLitestreamValidatorAllowsValidCreateAndNonWriteOperations(t *testing.T) {
	validator := newLitestreamValidator(t)
	resource := &v1alpha1.Litestream{
		TypeMeta: metav1.TypeMeta{Kind: "Litestream", APIVersion: v1alpha1.GroupVersion.String()},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app", Path: "/data/app.db",
			Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("replica")},
		}}},
	}

	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update, admissionv1.Delete} {
		t.Run(string(operation), func(t *testing.T) {
			response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, resource, operation))
			assert.Assert(t, response.Allowed, "%v", response.Result)
		})
	}
}

func TestLitestreamValidatorRejectsReplicationWithExistingMultiplePodWorkload(t *testing.T) {
	resource := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: testResourceName, Namespace: testNamespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app", Path: "/data/app.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("replica")},
		}}},
	}
	validator := newLitestreamValidator(t, replicatedDeployment(2))

	response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, resource, admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected replication to be denied when an existing workload creates multiple Pods")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "existing workload"), response.Result.Message)
	assert.Assert(t, strings.Contains(response.Result.Message, `"app"`), response.Result.Message)
}

func TestLitestreamValidatorAllowsReplicationWithSafeExistingWorkload(t *testing.T) {
	resource := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: testResourceName, Namespace: testNamespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app", Path: "/data/app.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("replica")},
		}}},
	}
	validator := newLitestreamValidator(t, safeReplicatedDeployment(1))

	response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, resource, admissionv1.Update))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestLitestreamValidatorRejectsReplicationWithMultipleExistingWorkloads(t *testing.T) {
	first := safeReplicatedDeployment(1)
	first.Name = "first"
	second := safeReplicatedDeployment(1)
	second.Name = "second"
	resource := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: testResourceName, Namespace: testNamespace},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app", Path: "/data/app.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("replica")},
		}}},
	}
	validator := newLitestreamValidator(t, first, second)

	response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, resource, admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected replication to be denied when multiple workloads share a Litestream")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "multiple workloads"), response.Result.Message)
}

func TestLitestreamValidatorRejectsSameDestinationAcrossDifferentLitestreams(t *testing.T) {
	existingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource.Name = "existing-db"
	existingResource.Status.ConfigMapName = ""
	existing := safeReplicatedDeployment(1)
	existing.Name = "existing"
	existing.Spec.Template.Annotations[InjectAnnotation] = existingResource.Name

	incomingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	incomingResource.Name = testResourceName
	incomingResource.Status.ConfigMapName = ""

	validator := newLitestreamValidator(t, existingResource, existing)

	response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, incomingResource, admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected a destination used by another Litestream to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "same destination Replica"), response.Result.Message)
}

func TestLitestreamValidatorAllowsDifferentDatabasePathsOnSameReplica(t *testing.T) {
	existingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource.Name = "existing-db"
	existingResource.Spec.Databases[0].Path = "/var/lib/app/other.db"
	existingResource.Status.ConfigMapName = ""
	existing := safeReplicatedDeployment(1)
	existing.Name = "existing"
	existing.Spec.Template.Annotations[InjectAnnotation] = existingResource.Name

	incomingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	incomingResource.Name = "incoming-db"
	incomingResource.Status.ConfigMapName = ""

	validator := newLitestreamValidator(t, existingResource, existing)

	response := validator.Handle(t.Context(), litestreamAdmissionRequest(t, incomingResource, admissionv1.Create))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func newLitestreamValidator(t *testing.T, objects ...client.Object) *LitestreamValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, appsv1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return NewLitestreamValidator(reader, scheme)
}

func litestreamAdmissionRequest(t *testing.T, resource *v1alpha1.Litestream, operation admissionv1.Operation) admission.Request {
	t.Helper()
	raw, err := json.Marshal(resource)
	assert.NilError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "00000000-0000-0000-0000-000000000000",
		Operation: operation,
		Name:      resource.Name,
		Namespace: resource.Namespace,
		Kind:      metav1.GroupVersionKind{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Kind: "Litestream"},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}
