package webhook

import (
	"context"
	"net/http"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-litestream,mutating=false,failurePolicy=fail,sideEffects=None,groups=litestream.mytools.nakatanakatana.app,resources=litestreams,verbs=create;update,versions=v1alpha1,name=vlitestream.litestream-controller.mytools.nakatanakatana.app,admissionReviewVersions=v1,timeoutSeconds=10

// LitestreamValidator rejects cross-field-invalid Litestream bindings before
// they are persisted. Field-level schema validation remains in the CRD; this
// handler owns the rules that require inspecting multiple binding fields
// together. Replica validation and dependency protection belong to
// DependencyValidator.
type LitestreamValidator struct {
	decoder admission.Decoder
}

// NewLitestreamValidator builds a validating admission handler for Litestream
// resources.
func NewLitestreamValidator(scheme *runtime.Scheme) *LitestreamValidator {
	return &LitestreamValidator{decoder: admission.NewDecoder(scheme)}
}

// Handle validates Litestream CREATE and UPDATE requests. Other operations
// are not part of the webhook's rule, but allowing them here keeps the handler
// safe when called directly in tests or by a broader configuration.
func (v *LitestreamValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("only Litestream create and update requests require validation")
	}

	resource := &v1alpha1.Litestream{}
	if err := v.decoder.Decode(req, resource); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if errs := resource.Spec.Validate(); len(errs) > 0 {
		return admission.Denied(errs.ToAggregate().Error())
	}
	return admission.Allowed("Litestream spec is valid")
}
