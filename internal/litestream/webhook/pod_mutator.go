// Package webhook injects Litestream restore init containers and a
// replication sidecar into annotated Pods. Its MutatingWebhookConfiguration
// uses a match condition to send only Pods with an inject annotation that is
// non-empty after trimming whitespace to this webhook. It reads only Ready
// Litestream resources and the non-secret configuration the controller
// rendered for them; Secret values are resolved by the kubelet, never by the
// webhook.
//
// Injected Pods require Kubernetes 1.29 or later: replication runs as a
// native sidecar. See ReplicateContainerName.
//
// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=fail,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod.litestream-controller.mytools.nakatanakatana.app,admissionReviewVersions=v1,timeoutSeconds=10
package webhook

import (
	"context"
	"fmt"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/controller"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodMutator injects the Litestream resource named by a Pod's inject
// annotation into that Pod.
type PodMutator struct {
	Client       client.Reader
	DefaultImage string
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list

// Mutate injects the databases of the annotated Litestream resource into
// pod. Pods without the annotation are left alone. Every check runs against
// a copy, so a Pod that cannot be injected is returned unmodified alongside
// an error explaining why.
func (m *PodMutator) Mutate(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("cannot inject litestream: a Pod is required")
	}
	name := strings.TrimSpace(pod.GetAnnotations()[InjectAnnotation])
	if name == "" {
		return nil
	}
	if pod.Namespace == "" {
		return fmt.Errorf("cannot inject litestream: the namespace of Pod %q is unknown", pod.Name)
	}
	if pod.Spec.OS != nil && pod.Spec.OS.Name == corev1.Windows {
		return fmt.Errorf("cannot inject litestream: the litestream image cannot run in a windows Pod")
	}

	resource := &v1alpha1.Litestream{}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: name}
	if err := m.Client.Get(ctx, key, resource); err != nil {
		return fmt.Errorf("cannot inject litestream: annotation %q references Litestream %q: %w", InjectAnnotation, name, err)
	}

	input, err := (resolver.Resolver{Reader: m.Client}).Resolve(ctx, resource)
	if err != nil {
		return fmt.Errorf("cannot inject litestream: %w", err)
	}
	rendered, err := renderReadyResource(resource, input)
	if err != nil {
		return err
	}
	if err := verifyConfigMap(ctx, m.Client, resource, rendered); err != nil {
		return err
	}
	if err := m.validateReplicationConflict(ctx, pod, resource); err != nil {
		return err
	}
	target, err := ResolveTarget(pod, input)
	if err != nil {
		return err
	}

	injected := pod.DeepCopy()
	injection, err := buildInjection(injected, input, rendered, target, configMapName(resource), m.DefaultImage)
	if err != nil {
		return err
	}
	if err := applyInjection(injected, injection); err != nil {
		return err
	}

	*pod = *injected
	return nil
}

func (m *PodMutator) validateReplicationConflict(ctx context.Context, pod *corev1.Pod, resource *v1alpha1.Litestream) error {
	if !litestreamReplicates(resource) {
		return nil
	}

	pods := &corev1.PodList{}
	if err := m.Client.List(ctx, pods, client.InNamespace(pod.Namespace)); err != nil {
		return fmt.Errorf("cannot inject litestream: list Pods in namespace %q: %w", pod.Namespace, err)
	}
	for i := range pods.Items {
		existing := &pods.Items[i]
		if existing.Name == pod.Name || !podMayWrite(existing) {
			continue
		}
		existingName := strings.TrimSpace(existing.Annotations[InjectAnnotation])
		if existingName == "" {
			continue
		}
		if existingName == resource.Name {
			return fmt.Errorf("cannot inject litestream: existing Pod %q already uses Litestream %q with replication", existing.Name, resource.Name)
		}

		existingResource := &v1alpha1.Litestream{}
		if err := m.Client.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: existingName}, existingResource); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("cannot inject litestream: get Litestream %q for existing Pod %q: %w", existingName, existing.Name, err)
		}
		if litestreamReplicates(existingResource) && shareReplicationDestination(resource, existingResource) {
			return fmt.Errorf("cannot inject litestream: existing Pod %q already uses the same destination Replica and database path", existing.Name)
		}
	}
	return nil
}

func podMayWrite(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

func verifyConfigMap(ctx context.Context, reader client.Reader, resource *v1alpha1.Litestream, rendered litestreamconfig.RenderedConfig) error {
	name := configMapName(resource)
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: name}, configMap); err != nil {
		return fmt.Errorf("cannot inject litestream: ConfigMap %q is unavailable: %w", name, err)
	}
	if !metav1.IsControlledBy(configMap, resource) {
		return fmt.Errorf("cannot inject litestream: ConfigMap %q is not controlled by Litestream %q", name, resource.Name)
	}
	if !equality.Semantic.DeepEqual(configMap.Data, rendered.Data) {
		return fmt.Errorf("cannot inject litestream: ConfigMap %q does not match rendered configuration", name)
	}
	return nil
}

// renderReadyResource re-renders the configuration of a resource whose
// latest spec the controller has already published, and confirms that the
// rendering matches the ConfigMap the injected Pod will mount.
func renderReadyResource(resource *v1alpha1.Litestream, input litestreamconfig.Input) (litestreamconfig.RenderedConfig, error) {
	ready := meta.FindStatusCondition(resource.Status.Conditions, controller.ReadyConditionType)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return litestreamconfig.RenderedConfig{}, fmt.Errorf(
			"cannot inject litestream: Litestream %q is not Ready", resource.Name,
		)
	}
	if ready.ObservedGeneration != 0 && ready.ObservedGeneration != resource.Generation {
		return litestreamconfig.RenderedConfig{}, fmt.Errorf(
			"cannot inject litestream: the Ready condition of Litestream %q is stale (observed generation %d, generation %d)",
			resource.Name, ready.ObservedGeneration, resource.Generation,
		)
	}

	rendered, err := litestreamconfig.Render(input)
	if err != nil {
		return litestreamconfig.RenderedConfig{}, fmt.Errorf("cannot inject litestream: %w", err)
	}
	if resource.Status.ConfigHash == "" {
		return litestreamconfig.RenderedConfig{}, fmt.Errorf(
			"cannot inject litestream: Litestream %q published config hash is empty", resource.Name,
		)
	}
	if published := resource.Status.ConfigHash; published != rendered.Hash {
		return litestreamconfig.RenderedConfig{}, fmt.Errorf(
			"cannot inject litestream: Litestream %q published config hash %s, but the webhook rendered %s",
			resource.Name, published, rendered.Hash,
		)
	}
	return rendered, nil
}

// configMapName prefers the name the controller reported over the one
// derived from the resource, so injection follows the ConfigMap that
// actually exists.
func configMapName(resource *v1alpha1.Litestream) string {
	if resource.Status.ConfigMapName != "" {
		return resource.Status.ConfigMapName
	}
	return controller.ConfigMapName(resource)
}
