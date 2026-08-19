package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
)

func TestWorkloadValidatorRejectsMultiplePodWorkloadsWithReplication(t *testing.T) {
	for _, test := range []struct {
		name     string
		resource client.Object
		kind     string
	}{
		{
			name:     "deployment",
			resource: replicatedDeployment(2),
			kind:     "Deployment",
		},
		{
			name:     "statefulset",
			resource: replicatedStatefulSet(2),
			kind:     "StatefulSet",
		},
		{
			name:     "daemonset",
			resource: replicatedDaemonSet(),
			kind:     "DaemonSet",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := newWorkloadValidator(t, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

			for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
				t.Run(string(operation), func(t *testing.T) {
					response := validator.Handle(t.Context(), workloadAdmissionRequest(t, test.resource, test.kind, operation))

					assert.Assert(t, !response.Allowed, "expected %s with Litestream replication to be denied", test.kind)
					assert.Assert(t, response.Result != nil)
					assert.Assert(t, strings.Contains(response.Result.Message, "replication requires a single writer"), response.Result.Message)
				})
			}
		})
	}
}

func TestWorkloadValidatorAllowsSingleReplicaAndNonReplicatingWorkloads(t *testing.T) {
	for _, test := range []struct {
		name     string
		resource client.Object
		kind     string
		objects  []client.Object
	}{
		{
			name:     "single replica deployment",
			resource: safeReplicatedDeployment(1),
			kind:     "Deployment",
			objects:  []client.Object{readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))},
		},
		{
			name:     "restore-only deployment",
			resource: replicatedDeployment(2),
			kind:     "Deployment",
			objects:  []client.Object{readyResource(t, v1alpha1.InjectionSpec{}, restoreOnlyDatabase("app"))},
		},
		{
			name:     "non-replicating daemonset",
			resource: replicatedDaemonSet(),
			kind:     "DaemonSet",
			objects:  []client.Object{readyResource(t, v1alpha1.InjectionSpec{}, restoreOnlyDatabase("app"))},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := newWorkloadValidator(t, test.objects...)

			response := validator.Handle(t.Context(), workloadAdmissionRequest(t, test.resource, test.kind, admissionv1.Create))

			assert.Assert(t, response.Allowed, "%v", response.Result)
		})
	}
}

func TestWorkloadValidatorRejectsUnresolvedMultiplePodWorkloads(t *testing.T) {
	validator := newWorkloadValidator(t)

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, replicatedDeployment(2), "Deployment", admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected an unresolved Litestream reference to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "has not been created"), response.Result.Message)
}

func TestWorkloadValidatorAllowsInactiveUnsafeDeploymentWithUnresolvedLitestream(t *testing.T) {
	validator := newWorkloadValidator(t)

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, replicatedDeployment(0), "Deployment", admissionv1.Create))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorAllowsUnannotatedAndZeroReplicaWorkloads(t *testing.T) {
	for _, test := range []struct {
		name     string
		resource client.Object
	}{
		{
			name:     "annotation omitted",
			resource: deploymentWithoutInjectionAnnotation(2),
		},
		{
			name:     "annotation blank",
			resource: deploymentWithInjectionAnnotation(2, "   "),
		},
		{
			name:     "replicas omitted",
			resource: deploymentWithInjectionAnnotationPtr(nil),
		},
		{
			name:     "replicas zero",
			resource: deploymentWithInjectionAnnotation(0, testResourceName),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := newWorkloadValidator(t, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

			response := validator.Handle(t.Context(), workloadAdmissionRequest(t, test.resource, "Deployment", admissionv1.Create))

			assert.Assert(t, response.Allowed, "%v", response.Result)
		})
	}
}

func TestWorkloadValidatorRejectsUnsafeDeploymentRollouts(t *testing.T) {
	validator := newWorkloadValidator(t, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, replicatedDeployment(1), "Deployment", admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected a rolling deployment with surge to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "maxSurge"), response.Result.Message)
	assert.Assert(t, strings.Contains(response.Result.Message, "unsafe rollout"), response.Result.Message)
}

func TestWorkloadValidatorAllowsDeploymentWithStringZeroMaxSurge(t *testing.T) {
	workload := replicatedDeployment(1)
	maxSurge := intstr.FromString("0")
	workload.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{MaxSurge: &maxSurge}
	validator := newWorkloadValidator(t, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, workload, "Deployment", admissionv1.Create))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorAllowsSafeDeploymentRollouts(t *testing.T) {
	validator := newWorkloadValidator(t, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, safeReplicatedDeployment(1), "Deployment", admissionv1.Create))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorRejectsScaleSubresource(t *testing.T) {
	for _, test := range []struct {
		name     string
		workload client.Object
		resource string
	}{
		{
			name:     "deployment",
			workload: safeReplicatedDeployment(1),
			resource: "deployments",
		},
		{
			name:     "statefulset",
			workload: replicatedStatefulSet(1),
			resource: "statefulsets",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := newWorkloadValidator(t, test.workload, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

			response := validator.Handle(t.Context(), workloadScaleAdmissionRequest(t, test.workload, test.resource, admissionv1.Update, 2))

			assert.Assert(t, !response.Allowed, "expected scaling to multiple Pods to be denied")
			assert.Assert(t, response.Result != nil)
			assert.Assert(t, strings.Contains(response.Result.Message, "single writer"), response.Result.Message)
		})
	}
}

func TestWorkloadValidatorAllowsScaleDownWithRollingDeployment(t *testing.T) {
	workload := replicatedDeployment(1)
	validator := newWorkloadValidator(t, workload, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadScaleAdmissionRequest(t, workload, "deployments", admissionv1.Update, 0))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorAllowsScaleUpToOneWithRollingDeployment(t *testing.T) {
	workload := replicatedDeployment(0)
	validator := newWorkloadValidator(t, workload, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadScaleAdmissionRequest(t, workload, "deployments", admissionv1.Update, 1))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorRejectsExistingWorkloadUsingSameReplicatingLitestream(t *testing.T) {
	existing := safeReplicatedDeployment(1)
	existing.Name = "existing"
	incoming := safeReplicatedDeployment(1)
	incoming.Name = "incoming"
	validator := newWorkloadValidator(t, existing, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, incoming, "Deployment", admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected a second workload using the same Litestream to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "existing"), response.Result.Message)
}

func TestWorkloadValidatorRejectsSameDestinationAcrossDifferentLitestreams(t *testing.T) {
	existingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource.Name = "existing-db"
	existingResource.Status.ConfigMapName = ""
	existing := safeReplicatedDeployment(1)
	existing.Name = "existing"
	existing.Spec.Template.Annotations[InjectAnnotation] = existingResource.Name

	incomingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	incomingResource.Name = "incoming-db"
	incomingResource.Status.ConfigMapName = ""
	incoming := safeReplicatedDeployment(1)
	incoming.Name = "incoming"
	incoming.Spec.Template.Annotations[InjectAnnotation] = incomingResource.Name

	validator := newWorkloadValidator(t, existing, existingResource, incomingResource)

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, incoming, "Deployment", admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected the same destination used through different Litestreams to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "same destination Replica"), response.Result.Message)
}

func TestWorkloadValidatorAllowsDifferentDatabasePathsOnSameReplica(t *testing.T) {
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
	incoming := safeReplicatedDeployment(1)
	incoming.Name = "incoming"
	incoming.Spec.Template.Annotations[InjectAnnotation] = incomingResource.Name

	validator := newWorkloadValidator(t, existing, existingResource, incomingResource)

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, incoming, "Deployment", admissionv1.Create))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorRejectsScaleUpWhenExistingWorkloadUsesSameReplicatingLitestream(t *testing.T) {
	existing := safeReplicatedDeployment(1)
	existing.Name = "existing"
	incoming := safeReplicatedDeployment(0)
	incoming.Name = "incoming"
	validator := newWorkloadValidator(t, existing, incoming, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadScaleAdmissionRequest(t, incoming, "deployments", admissionv1.Update, 1))

	assert.Assert(t, !response.Allowed, "expected scaling a second workload using the same Litestream to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "existing"), response.Result.Message)
}

func TestWorkloadValidatorAllowsUpdatingExistingWorkloadUsingSameReplicatingLitestream(t *testing.T) {
	workload := safeReplicatedDeployment(1)
	validator := newWorkloadValidator(t, workload, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, workload, "Deployment", admissionv1.Update))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func TestWorkloadValidatorRejectsDifferentKindUsingSameReplicatingLitestream(t *testing.T) {
	existing := replicatedStatefulSet(1)
	incoming := safeReplicatedDeployment(1)
	validator := newWorkloadValidator(t, existing, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, incoming, "Deployment", admissionv1.Create))

	assert.Assert(t, !response.Allowed, "expected a different workload kind using the same Litestream to be denied")
	assert.Assert(t, response.Result != nil)
	assert.Assert(t, strings.Contains(response.Result.Message, "StatefulSet"), response.Result.Message)
}

func TestWorkloadValidatorReturnsInternalServerErrorWhenScaleWorkloadCannotBeRead(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NilError(t, appsv1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	validator := NewWorkloadValidator(failingWorkloadReader{err: fmt.Errorf("backend unavailable")}, scheme)
	workload := replicatedDeployment(1)

	response := validator.Handle(t.Context(), workloadScaleAdmissionRequest(t, workload, "deployments", admissionv1.Update, 1))

	assert.Assert(t, !response.Allowed, "expected a scale read failure to be rejected")
	assert.Assert(t, response.Result != nil)
	assert.Equal(t, response.Result.Code, int32(http.StatusInternalServerError))
}

func TestWorkloadValidatorAllowsDeleteOperations(t *testing.T) {
	validator := newWorkloadValidator(t, readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")))
	resource := replicatedDeployment(2)

	response := validator.Handle(t.Context(), workloadAdmissionRequest(t, resource, "Deployment", admissionv1.Delete))

	assert.Assert(t, response.Allowed, "%v", response.Result)
}

func newWorkloadValidator(t *testing.T, objects ...client.Object) *WorkloadValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, appsv1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return NewWorkloadValidator(reader, scheme)
}

type failingWorkloadReader struct {
	client.Reader
	err error
}

func (r failingWorkloadReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func workloadAdmissionRequest(t *testing.T, resource client.Object, kind string, operation admissionv1.Operation) admission.Request {
	t.Helper()
	raw, err := json.Marshal(resource)
	assert.NilError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "00000000-0000-0000-0000-000000000000",
		Operation: operation,
		Namespace: resource.GetNamespace(),
		Kind:      metav1.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind},
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func workloadScaleAdmissionRequest(t *testing.T, resource client.Object, resourceName string, operation admissionv1.Operation, replicas int32) admission.Request {
	t.Helper()
	raw, err := json.Marshal(&autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: resource.GetName(), Namespace: resource.GetNamespace()},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	})
	assert.NilError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:         "00000000-0000-0000-0000-000000000000",
		Operation:   operation,
		Namespace:   resource.GetNamespace(),
		Name:        resource.GetName(),
		Kind:        metav1.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"},
		Resource:    metav1.GroupVersionResource{Group: "apps", Version: "v1", Resource: resourceName},
		SubResource: "scale",
		Object:      runtime.RawExtension{Raw: raw},
	}}
}

func replicatedDeployment(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: replicatedPodTemplate(),
		},
	}
}

func safeReplicatedDeployment(replicas int32) *appsv1.Deployment {
	resource := replicatedDeployment(replicas)
	resource.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	return resource
}

func deploymentWithoutInjectionAnnotation(replicas int32) *appsv1.Deployment {
	resource := replicatedDeployment(replicas)
	delete(resource.Spec.Template.Annotations, InjectAnnotation)
	return resource
}

func deploymentWithInjectionAnnotation(replicas int32, annotation string) *appsv1.Deployment {
	resource := replicatedDeployment(replicas)
	resource.Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
	resource.Spec.Template.Annotations[InjectAnnotation] = annotation
	return resource
}

func deploymentWithInjectionAnnotationPtr(replicas *int32) *appsv1.Deployment {
	resource := safeReplicatedDeployment(1)
	resource.Spec.Replicas = replicas
	return resource
}

func replicatedStatefulSet(replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testNamespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: "app",
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template:    replicatedPodTemplate(),
		},
	}
}

func replicatedDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: testNamespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: replicatedPodTemplate(),
		},
	}
}

func replicatedPodTemplate() corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{InjectAnnotation: testResourceName},
			Labels:      map[string]string{"app": "app"},
		},
		Spec: corev1.PodSpec{},
	}
}
