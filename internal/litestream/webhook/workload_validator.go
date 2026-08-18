package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-litestream-workload,mutating=false,failurePolicy=fail,sideEffects=None,groups=apps,resources=deployments;deployments/scale;statefulsets;statefulsets/scale;daemonsets,verbs=create;update,versions=v1,name=vlitestreamworkload.litestream-controller.mytools.nakatanakatana.app,admissionReviewVersions=v1,timeoutSeconds=10

// WorkloadValidator rejects workloads that can create multiple Pods when the
// referenced Litestream resource continuously replicates a database. Every
// such Pod would start a Litestream sidecar, and the sidecars would compete
// for the same destination Replica path.
type WorkloadValidator struct {
	Reader  client.Reader
	Decoder admission.Decoder
}

type workloadInfo struct {
	kind                    string
	name                    string
	template                corev1.PodTemplateSpec
	hasActivePods           bool
	multiplePods            bool
	unsafeDeploymentRollout bool
}

type workloadReaderError struct {
	err error
}

func (e *workloadReaderError) Error() string {
	return e.err.Error()
}

func (e *workloadReaderError) Unwrap() error {
	return e.err
}

// NewWorkloadValidator builds a validating admission handler for app
// workloads that may receive Litestream injection through their Pod template.
func NewWorkloadValidator(reader client.Reader, scheme *runtime.Scheme) *WorkloadValidator {
	return &WorkloadValidator{Reader: reader, Decoder: admission.NewDecoder(scheme)}
}

// Handle validates Deployment, StatefulSet, and DaemonSet CREATE, UPDATE, and
// PATCH requests. Scale subresource requests are resolved against the current
// workload so HPA and kubectl scale cannot bypass the single-writer check.
func (v *WorkloadValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed("only workload create and update requests require validation")
	}

	workload, err := v.decodeWorkload(ctx, req)
	if err != nil {
		var readerErr *workloadReaderError
		if errors.As(err, &readerErr) {
			return admission.Errored(http.StatusInternalServerError, readerErr)
		}
		return admission.Errored(http.StatusBadRequest, err)
	}

	litestreamName := strings.TrimSpace(workload.template.Annotations[InjectAnnotation])
	if litestreamName == "" {
		return admission.Allowed("workload does not request Litestream injection")
	}
	if !workload.hasActivePods && !workload.multiplePods {
		return admission.Allowed("workload has no active Pods")
	}

	litestream := &v1alpha1.Litestream{}
	if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: litestreamName}, litestream); err != nil {
		if apierrors.IsNotFound(err) {
			if !workload.multiplePods && !workload.unsafeDeploymentRollout {
				return admission.Allowed("single-Pod workload may reference a Litestream that is not created yet")
			}
			return admission.Denied(fmt.Sprintf("Litestream %q has not been created; multi-Pod or unsafe-rollout workloads must reference an existing Litestream", litestreamName))
		}
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("get Litestream %q: %w", litestreamName, err))
	}
	if !litestreamReplicates(litestream) {
		return admission.Allowed(fmt.Sprintf("Litestream %q does not configure replication", litestreamName))
	}

	if workload.unsafeDeploymentRollout && workload.hasActivePods && !workload.multiplePods {
		return admission.Denied(fmt.Sprintf(
			"Litestream %q enables replication, but Deployment uses an unsafe rollout; maxSurge must be zero or the strategy must be Recreate to preserve a single writer",
			litestreamName,
		))
	}
	if workload.multiplePods {
		return admission.Denied(fmt.Sprintf(
			"Litestream %q enables replication, but replication requires a single writer; %s would create conflicting Pods",
			litestreamName, workload.kind,
		))
	}
	if workload.hasActivePods {
		currentName := req.Name
		if currentName == "" {
			currentName = workload.name
		}
		conflict, err := v.existingWorkloadConflict(ctx, req.Namespace, litestream, workload.kind, currentName)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if conflict != "" {
			return admission.Denied(conflict)
		}
	}
	return admission.Allowed("workload has a single writer")
}

func (v *WorkloadValidator) decodeWorkload(ctx context.Context, req admission.Request) (workloadInfo, error) {
	if req.SubResource == "scale" {
		return v.decodeScaleWorkload(ctx, req)
	}

	switch req.Kind.Kind {
	case "Deployment":
		resource := &appsv1.Deployment{}
		if err := v.Decoder.Decode(req, resource); err != nil {
			return workloadInfo{}, err
		}
		return deploymentWorkloadInfo(resource, effectiveReplicas(resource.Spec.Replicas)), nil

	case "StatefulSet":
		resource := &appsv1.StatefulSet{}
		if err := v.Decoder.Decode(req, resource); err != nil {
			return workloadInfo{}, err
		}
		return workloadInfo{
			kind:          "StatefulSet",
			name:          resource.Name,
			template:      resource.Spec.Template,
			hasActivePods: effectiveReplicas(resource.Spec.Replicas) > 0,
			multiplePods:  effectiveReplicas(resource.Spec.Replicas) > 1,
		}, nil

	case "DaemonSet":
		resource := &appsv1.DaemonSet{}
		if err := v.Decoder.Decode(req, resource); err != nil {
			return workloadInfo{}, err
		}
		return workloadInfo{kind: "DaemonSet", name: resource.Name, template: resource.Spec.Template, hasActivePods: true, multiplePods: true}, nil

	default:
		return workloadInfo{}, fmt.Errorf("unsupported workload kind %q", req.Kind.Kind)
	}
}

func (v *WorkloadValidator) decodeScaleWorkload(ctx context.Context, req admission.Request) (workloadInfo, error) {
	scale := &autoscalingv1.Scale{}
	if err := json.Unmarshal(req.Object.Raw, scale); err != nil {
		return workloadInfo{}, err
	}
	key := client.ObjectKey{Namespace: req.Namespace, Name: req.Name}
	switch req.Resource.Resource {
	case "deployments":
		resource := &appsv1.Deployment{}
		if err := v.Reader.Get(ctx, key, resource); err != nil {
			return workloadInfo{}, &workloadReaderError{err: fmt.Errorf("get Deployment %q: %w", req.Name, err)}
		}
		return deploymentScaleWorkloadInfo(resource, scale.Spec.Replicas), nil
	case "statefulsets":
		resource := &appsv1.StatefulSet{}
		if err := v.Reader.Get(ctx, key, resource); err != nil {
			return workloadInfo{}, &workloadReaderError{err: fmt.Errorf("get StatefulSet %q: %w", req.Name, err)}
		}
		return workloadInfo{
			kind:          "StatefulSet",
			name:          resource.Name,
			template:      resource.Spec.Template,
			hasActivePods: scale.Spec.Replicas > 0,
			multiplePods:  scale.Spec.Replicas > 1,
		}, nil
	default:
		return workloadInfo{}, fmt.Errorf("unsupported scale resource %q", req.Resource.Resource)
	}
}

func deploymentWorkloadInfo(resource *appsv1.Deployment, replicas int32) workloadInfo {
	return workloadInfo{
		kind:                    "Deployment",
		name:                    resource.Name,
		template:                resource.Spec.Template,
		hasActivePods:           replicas > 0,
		multiplePods:            replicas > 1,
		unsafeDeploymentRollout: deploymentMayCreateConcurrentPods(resource),
	}
}

func deploymentScaleWorkloadInfo(resource *appsv1.Deployment, replicas int32) workloadInfo {
	return workloadInfo{
		kind:          "Deployment",
		name:          resource.Name,
		template:      resource.Spec.Template,
		hasActivePods: replicas > 0,
		multiplePods:  replicas > 1,
	}
}

func (v *WorkloadValidator) existingWorkloadConflict(ctx context.Context, namespace string, litestream *v1alpha1.Litestream, currentKind, currentName string) (string, error) {
	workloads, err := listWorkloads(ctx, v.Reader, namespace)
	if err != nil {
		return "", err
	}
	for _, workload := range workloads {
		if workload.kind == currentKind && workload.name == currentName {
			continue
		}
		litestreamName := litestream.Name
		if conflict := workloadSafetyConflict(workload, litestreamName); conflict != "" {
			return conflict, nil
		}
		existingName := strings.TrimSpace(workload.template.Annotations[InjectAnnotation])
		if !workload.hasActivePods || existingName == "" {
			continue
		}

		existing := &v1alpha1.Litestream{}
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: existingName}, existing); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("get Litestream %q: %w", existingName, err)
		}
		if litestreamReplicates(existing) && shareReplicationDestination(litestream, existing) {
			return workloadDestinationConflict(workload, litestreamName), nil
		}
	}
	return "", nil
}

func listWorkloads(ctx context.Context, reader client.Reader, namespace string) ([]workloadInfo, error) {
	var workloads []workloadInfo

	deployments := &appsv1.DeploymentList{}
	if err := reader.List(ctx, deployments, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Deployments in namespace %q: %w", namespace, err)
	}
	for i := range deployments.Items {
		workloads = append(workloads, deploymentWorkloadInfo(&deployments.Items[i], effectiveReplicas(deployments.Items[i].Spec.Replicas)))
	}

	statefulSets := &appsv1.StatefulSetList{}
	if err := reader.List(ctx, statefulSets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list StatefulSets in namespace %q: %w", namespace, err)
	}
	for i := range statefulSets.Items {
		replicas := effectiveReplicas(statefulSets.Items[i].Spec.Replicas)
		workloads = append(workloads, workloadInfo{
			kind:          "StatefulSet",
			name:          statefulSets.Items[i].Name,
			template:      statefulSets.Items[i].Spec.Template,
			hasActivePods: replicas > 0,
			multiplePods:  replicas > 1,
		})
	}

	daemonSets := &appsv1.DaemonSetList{}
	if err := reader.List(ctx, daemonSets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list DaemonSets in namespace %q: %w", namespace, err)
	}
	for i := range daemonSets.Items {
		workloads = append(workloads, workloadInfo{
			kind:          "DaemonSet",
			name:          daemonSets.Items[i].Name,
			template:      daemonSets.Items[i].Spec.Template,
			hasActivePods: true,
			multiplePods:  true,
		})
	}
	return workloads, nil
}

func workloadSafetyConflict(workload workloadInfo, litestreamName string) string {
	if strings.TrimSpace(workload.template.Annotations[InjectAnnotation]) != litestreamName {
		return ""
	}
	if workload.multiplePods {
		return fmt.Sprintf("Litestream %q enables replication, but existing workload %s %q creates multiple Pods with the same destination Replica", litestreamName, workload.kind, workload.name)
	}
	if workload.hasActivePods && workload.unsafeDeploymentRollout {
		return fmt.Sprintf("Litestream %q enables replication, but existing Deployment %q uses a rolling update with maxSurge and can create concurrent Pods", litestreamName, workload.name)
	}
	return ""
}

func workloadDestinationConflict(workload workloadInfo, litestreamName string) string {
	return fmt.Sprintf("Litestream %q enables replication, but existing %s %q already uses the same destination Replica and database path", litestreamName, workload.kind, workload.name)
}

func effectiveReplicas(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

func deploymentMayCreateConcurrentPods(resource *appsv1.Deployment) bool {
	if resource.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		return false
	}
	if resource.Spec.Strategy.RollingUpdate == nil || resource.Spec.Strategy.RollingUpdate.MaxSurge == nil {
		return true
	}
	maxSurge := resource.Spec.Strategy.RollingUpdate.MaxSurge
	if maxSurge.Type == intstr.Int {
		return maxSurge.IntVal > 0
	}
	return strings.TrimSpace(maxSurge.StrVal) != "0%" && strings.TrimSpace(maxSurge.StrVal) != "0"
}

func litestreamReplicates(resource *v1alpha1.Litestream) bool {
	for _, database := range resource.Spec.Databases {
		if database.Replicate != nil {
			return true
		}
	}
	return false
}
