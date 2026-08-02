package webhook

import (
	"context"
	"encoding/json"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Handler serves the /mutate-v1-pod admission webhook. Only Pod CREATE
// requests are considered for injection: Kubernetes never lets a mutating
// webhook change an existing Pod's containers or volumes, so every other
// operation is allowed unchanged.
type Handler struct {
	mutator *PodMutator
	decoder admission.Decoder
}

// NewHandler builds a Handler that injects the Litestream resources named
// by a Pod's inject annotation, reading them through reader and falling back to
// defaultImage when a resource does not pin its own Litestream image.
func NewHandler(reader client.Reader, scheme *runtime.Scheme, defaultImage string) *Handler {
	return &Handler{
		mutator: &PodMutator{Client: reader, DefaultImage: defaultImage},
		decoder: admission.NewDecoder(scheme),
	}
}

// Handle admits req. A CREATE Pod carrying the inject annotation is
// returned with a JSON patch adding the Litestream resource's containers
// and volumes; an unannotated Pod, and every non-CREATE operation, is
// allowed unchanged. A CREATE Pod whose annotation cannot be honored -
// because the referenced Litestream resource is missing, not Ready, or
// stale - is denied with the reason.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only Pod creation is subject to litestream injection")
	}

	pod := &corev1.Pod{}
	if err := h.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if err := h.mutator.Mutate(ctx, pod); err != nil {
		return admission.Denied(err.Error())
	}

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}
