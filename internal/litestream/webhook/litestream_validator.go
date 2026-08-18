package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	reader  client.Reader
}

// NewLitestreamValidator builds a validating admission handler for Litestream
// resources.
func NewLitestreamValidator(reader client.Reader, scheme *runtime.Scheme) *LitestreamValidator {
	return &LitestreamValidator{decoder: admission.NewDecoder(scheme), reader: reader}
}

// Handle validates Litestream CREATE and UPDATE requests. Other operations
// are not part of the webhook's rule, but allowing them here keeps the handler
// safe when called directly in tests or by a broader configuration.
func (v *LitestreamValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
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
	if litestreamReplicates(resource) {
		conflict, err := v.existingWorkloadConflict(ctx, resource)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if conflict != "" {
			return admission.Denied(conflict)
		}
	}
	return admission.Allowed("Litestream spec is valid")
}

func (v *LitestreamValidator) existingWorkloadConflict(ctx context.Context, resource *v1alpha1.Litestream) (string, error) {
	if v.reader == nil {
		return "", fmt.Errorf("validate Litestream replication: workload reader is not configured")
	}

	workloads, err := listWorkloads(ctx, v.reader, resource.Namespace)
	if err != nil {
		return "", err
	}
	var matchingWorkloads []workloadInfo
	for _, workload := range workloads {
		if conflict := workloadSafetyConflict(workload, resource.Name); conflict != "" {
			return conflict, nil
		}
		existingName := strings.TrimSpace(workload.template.Annotations[InjectAnnotation])
		if !workload.hasActivePods || existingName == "" {
			continue
		}
		if existingName == resource.Name {
			matchingWorkloads = append(matchingWorkloads, workload)
			continue
		}

		existing := &v1alpha1.Litestream{}
		if err := v.reader.Get(ctx, client.ObjectKey{Namespace: resource.Namespace, Name: existingName}, existing); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("get Litestream %q: %w", existingName, err)
		}
		if litestreamReplicates(existing) && shareReplicationDestination(resource, existing) {
			return workloadDestinationConflict(workload, resource.Name), nil
		}
	}
	if len(matchingWorkloads) > 1 {
		return fmt.Sprintf("Litestream %q enables replication, but multiple workloads (%s and %s) already use the same destination Replica", resource.Name, workloadReference(matchingWorkloads[0]), workloadReference(matchingWorkloads[1])), nil
	}
	return "", nil
}

func workloadReference(workload workloadInfo) string {
	return fmt.Sprintf("%s %q", workload.kind, workload.name)
}
