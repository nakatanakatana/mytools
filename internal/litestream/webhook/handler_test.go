package webhook

import (
	"encoding/json"
	"net/http"
	"net/url"
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
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestRegisterHandlersRegistersStableAdmissionPaths(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	server := crwebhook.NewServer(crwebhook.Options{})
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()

	RegisterHandlers(server, reader, scheme, testDefaultImage)

	for _, path := range []string{
		"/mutate-v1-pod",
		"/validate-litestream",
		"/validate-litestreamreplica",
	} {
		t.Run(path, func(t *testing.T) {
			_, pattern := server.WebhookMux().Handler(&http.Request{URL: &url.URL{Path: path}})
			assert.Equal(t, pattern, path)
		})
	}
}

func TestHandlerPatchesCreatedAnnotatedPods(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	handler := newHandler(t, resource)

	resp := handler.Handle(t.Context(), podRequest(t, targetPod(), admissionv1.Create))

	assert.Assert(t, resp.Allowed, "%v", resp.Result)
	assert.Assert(t, len(resp.Patches) > 0, "a CREATE request for an annotated Pod must return a JSON patch")
}

func TestHandlerAllowsUnannotatedPodsUnchanged(t *testing.T) {
	handler := newHandler(t)
	pod := targetPod()
	delete(pod.Annotations, InjectAnnotation)

	resp := handler.Handle(t.Context(), podRequest(t, pod, admissionv1.Create))

	assert.Assert(t, resp.Allowed, "%v", resp.Result)
	assert.Equal(t, len(resp.Patches), 0, "an unannotated Pod must not be patched")
}

func TestHandlerAllowsNonCreateOperationsUnchanged(t *testing.T) {
	for _, operation := range []admissionv1.Operation{admissionv1.Update, admissionv1.Delete} {
		t.Run(string(operation), func(t *testing.T) {
			resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
			handler := newHandler(t, resource)

			resp := handler.Handle(t.Context(), podRequest(t, targetPod(), operation))

			assert.Assert(t, resp.Allowed, "%v", resp.Result)
			assert.Equal(t, len(resp.Patches), 0, "%s requests must not be mutated", operation)
		})
	}
}

func TestHandlerDeniesReferencesThatCannotBeInjected(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod)
		err   string
	}{
		{
			name: "missing resource",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Name = "other"
				return resource, targetPod()
			},
			err: testResourceName,
		},
		{
			name: "stale ready condition",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Generation++
				return resource, targetPod()
			},
			err: "stale",
		},
		{
			name: "resource is not ready",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Status.Conditions[0].Status = metav1.ConditionFalse
				return resource, targetPod()
			},
			err: "not Ready",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource, pod := test.build(t)
			handler := newHandler(t, resource)

			resp := handler.Handle(t.Context(), podRequest(t, pod, admissionv1.Create))

			assert.Assert(t, !resp.Allowed, "expected the request to be denied")
			assert.Assert(t, resp.Result != nil)
			assert.Assert(t, strings.Contains(resp.Result.Message, test.err), resp.Result.Message)
		})
	}
}

func newHandler(t *testing.T, objects ...client.Object) *Handler {
	t.Helper()
	objects = appendDefaultDependencies(objects...)
	objects = appendRenderedConfigMaps(t, objects...)
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return NewHandler(c, scheme, testDefaultImage)
}

func podRequest(t *testing.T, pod *corev1.Pod, operation admissionv1.Operation) admission.Request {
	t.Helper()
	pod = pod.DeepCopy()
	pod.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
	raw, err := json.Marshal(pod)
	assert.NilError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "00000000-0000-0000-0000-000000000000",
		Operation: operation,
		Namespace: pod.Namespace,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}
