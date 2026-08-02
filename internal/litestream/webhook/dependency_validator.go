package webhook

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-litestreamreplica,mutating=false,failurePolicy=fail,sideEffects=None,groups=litestream.mytools.nakatanakatana.app,resources=litestreamreplicas,verbs=create;update;delete,versions=v1alpha1,name=vlitestreamreplica.litestream-controller.mytools.nakatanakatana.app,admissionReviewVersions=v1,timeoutSeconds=10

// DependencyValidator validates LitestreamReplica resources and prevents their
// deletion while same-namespace Litestream resources consume them. References
// are intentionally not resolved on create or update so users can apply
// related resources in any order.
type DependencyValidator struct {
	Reader  client.Reader
	Decoder admission.Decoder
}

// NewDependencyValidator builds a validating admission handler for
// LitestreamReplica resources.
func NewDependencyValidator(reader client.Reader, scheme *runtime.Scheme) *DependencyValidator {
	return &DependencyValidator{Reader: reader, Decoder: admission.NewDecoder(scheme)}
}

// Handle validates CREATE and UPDATE requests structurally, and protects
// LitestreamReplica resources from DELETE requests while same-namespace
// Litestream resources reference them.
func (v *DependencyValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	return v.handleReplica(ctx, req)
}

func (v *DependencyValidator) handleReplica(ctx context.Context, req admission.Request) admission.Response {
	resource := &v1alpha1.LitestreamReplica{}
	if req.Operation == admissionv1.Delete {
		if err := v.Decoder.DecodeRaw(req.OldObject, resource); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		return v.protectReplicaDeletion(ctx, req.Namespace, resource)
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("only LitestreamReplica create, update, and delete requests require validation")
	}
	if err := v.Decoder.Decode(req, resource); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if errs := resource.Spec.Validate(); len(errs) > 0 {
		return admission.Denied(errs.ToAggregate().Error())
	}
	return admission.Allowed("LitestreamReplica spec is valid")
}

func (v *DependencyValidator) protectReplicaDeletion(ctx context.Context, namespace string, replica *v1alpha1.LitestreamReplica) admission.Response {
	consumers, err := v.listConsumers(ctx, namespace)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	blocked := make([]string, 0)
	for _, consumer := range consumers.Items {
		for _, binding := range consumer.Spec.Databases {
			directSource := binding.Restore != nil && binding.Restore.ReplicaRef.Name == replica.Name
			directDestination := binding.Replicate != nil && binding.Replicate.ReplicaRef.Name == replica.Name
			if directSource || directDestination {
				blocked = append(blocked, consumer.Name)
				break
			}
		}
	}
	return allowDeletionUnlessReferenced("LitestreamReplica", replica.Name, blocked)
}

func (v *DependencyValidator) listConsumers(ctx context.Context, namespace string) (*v1alpha1.LitestreamList, error) {
	consumers := &v1alpha1.LitestreamList{}
	if err := v.Reader.List(ctx, consumers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Litestream consumers in namespace %q: %w", namespace, err)
	}
	return consumers, nil
}

func allowDeletionUnlessReferenced(kind, name string, consumers []string) admission.Response {
	if len(consumers) == 0 {
		return admission.Allowed(kind + " is not referenced by any Litestream resource")
	}
	sort.Strings(consumers)
	return admission.Denied(fmt.Sprintf("cannot delete %s %q: referenced by Litestream resources %s", kind, name, strings.Join(consumers, ", ")))
}
