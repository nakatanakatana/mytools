package webhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func newLitestreamValidator(t *testing.T) *LitestreamValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	return NewLitestreamValidator(scheme)
}

func litestreamAdmissionRequest(t *testing.T, resource *v1alpha1.Litestream, operation admissionv1.Operation) admission.Request {
	t.Helper()
	raw, err := json.Marshal(resource)
	assert.NilError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "00000000-0000-0000-0000-000000000000",
		Operation: operation,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}
