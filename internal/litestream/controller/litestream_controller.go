// Package controller reconciles Litestream custom resources into the
// non-secret ConfigMaps that carry their rendered configuration, and
// reports rendering outcomes through status conditions and events. It never
// reads or writes Secret data, and it never creates or mutates workloads.
package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ReadyConditionType is the status condition reporting whether the
	// latest spec was successfully rendered into the owned ConfigMap.
	ReadyConditionType = "Ready"

	// ReasonConfigRendered is used on the Ready condition and the
	// corresponding event when rendering succeeds.
	ReasonConfigRendered = "ConfigRendered"

	// ReasonInvalidConfiguration is used on the Ready condition and the
	// corresponding event when the spec fails validation or rendering.
	ReasonInvalidConfiguration = "InvalidConfiguration"

	// LabelManagedBy identifies resources owned by this controller.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the LabelManagedBy value this controller sets.
	ManagedByValue = "litestream-controller"

	// LabelResourceName records the owning Litestream resource's name.
	LabelResourceName = "litestream.mytools.nakatanakatana.app/resource-name"
	// AnnotationResourceName records the complete owning resource name when
	// the corresponding label must be shortened to fit Kubernetes limits.
	AnnotationResourceName = "litestream.mytools.nakatanakatana.app/resource-name"
	// LabelResourceUID records the owning Litestream resource's UID.
	LabelResourceUID = "litestream.mytools.nakatanakatana.app/resource-uid"

	litestreamSourceReplicaReferenceIndex      = "litestream.mytools.nakatanakatana.app/source-replica-ref"
	litestreamDestinationReplicaReferenceIndex = "litestream.mytools.nakatanakatana.app/destination-replica-ref"
)

// LitestreamReconciler reconciles a Litestream resource into owned immutable
// ConfigMap revisions containing its rendered, non-secret configuration.
type LitestreamReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Resolver resolver.Resolver
}

// ConfigMapName returns the deterministic name of the ConfigMap owned by
// resource. It is stable across reconciles because it derives from the
// resource's UID, which does not change for the lifetime of the object.
func ConfigMapName(resource *v1alpha1.Litestream) string {
	return configMapName(resource, "")
}

func configMapName(resource *v1alpha1.Litestream, revisionSuffix string) string {
	uid := string(resource.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	const prefix = "litestream-"
	uidSuffix := "-" + uid
	maxResourceNameLength := validation.DNS1123SubdomainMaxLength - len(prefix) - len(uidSuffix) - len(revisionSuffix)
	resourceName := resource.Name
	if len(resourceName) > maxResourceNameLength {
		resourceName = strings.TrimRight(resourceName[:maxResourceNameLength], ".")
	}
	return fmt.Sprintf("%s%s%s%s", prefix, resourceName, uidSuffix, revisionSuffix)
}

// ConfigMapNameForHash returns an immutable revision name for a rendered
// configuration. The resource UID remains part of the name, while the hash
// makes each rendered revision a distinct ConfigMap.
func ConfigMapNameForHash(resource *v1alpha1.Litestream, hash string) string {
	if hash == "" {
		return ConfigMapName(resource)
	}
	return configMapName(resource, "-"+hash)
}

func resourceNameLabel(name string) string {
	if len(name) <= validation.LabelValueMaxLength {
		return name
	}

	const hashLength = 8
	prefixLength := validation.LabelValueMaxLength - 1 - hashLength
	prefix := strings.TrimRight(name[:prefixLength], "-_.")
	hash := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s-%x", prefix, hash[:hashLength/2])
}

// +kubebuilder:rbac:groups=litestream.mytools.nakatanakatana.app,resources=litestreams,verbs=get;list;watch
// +kubebuilder:rbac:groups=litestream.mytools.nakatanakatana.app,resources=litestreams/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=litestream.mytools.nakatanakatana.app,resources=litestreamreplicas,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile renders one Litestream resource's spec and creates its current
// owned ConfigMap revision, then reports the outcome on the resource's status
// conditions. Earlier revisions are retained until owner garbage collection.
func (r *LitestreamReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var resource v1alpha1.Litestream
	if err := r.Get(ctx, req.NamespacedName, &resource); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !resource.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if errs := resource.Spec.Validate(); len(errs) > 0 {
		return ctrl.Result{}, r.markInvalid(ctx, &resource, errs.ToAggregate())
	}

	input, err := r.resolve(ctx, &resource)
	if err != nil {
		if !resolver.IsPermanentError(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.markInvalid(ctx, &resource, err)
	}
	rendered, err := litestreamconfig.Render(input)
	if err != nil {
		return ctrl.Result{}, r.markInvalid(ctx, &resource, err)
	}

	name := ConfigMapNameForHash(&resource, rendered.Hash)
	if err := r.reconcileConfigMap(ctx, &resource, name, rendered); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(&resource, nil, corev1.EventTypeNormal, ReasonConfigRendered, ReasonConfigRendered,
		"rendered configuration into ConfigMap %s (hash %s)", name, rendered.Hash)

	if err := r.markReady(ctx, &resource, name, rendered.Hash); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LitestreamReconciler) resolve(ctx context.Context, resource *v1alpha1.Litestream) (litestreamconfig.Input, error) {
	dependencyResolver := r.Resolver
	if dependencyResolver.Reader == nil {
		dependencyResolver.Reader = r.Client
	}
	return dependencyResolver.Resolve(ctx, resource)
}

func (r *LitestreamReconciler) reconcileConfigMap(
	ctx context.Context, resource *v1alpha1.Litestream, name string, rendered litestreamconfig.RenderedConfig,
) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: resource.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = make(map[string]string, 3)
		}
		if cm.Annotations == nil {
			cm.Annotations = make(map[string]string, 1)
		}
		cm.Labels[LabelManagedBy] = ManagedByValue
		cm.Labels[LabelResourceName] = resourceNameLabel(resource.Name)
		cm.Labels[LabelResourceUID] = string(resource.UID)
		cm.Annotations[AnnotationResourceName] = resource.Name
		cm.Data = rendered.Data
		cm.Immutable = ptr.To(true)
		return controllerutil.SetControllerReference(resource, cm, r.Scheme, controllerutil.WithBlockOwnerDeletion(false))
	})
	return err
}

func (r *LitestreamReconciler) markReady(ctx context.Context, resource *v1alpha1.Litestream, configMapName, hash string) error {
	base := resource.DeepCopy()

	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               ReadyConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonConfigRendered,
		ObservedGeneration: resource.Generation,
		Message:            "litestream configuration rendered successfully",
	})
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.ConfigMapName = configMapName
	resource.Status.ConfigHash = hash

	return r.Status().Patch(ctx, resource, client.MergeFrom(base))
}

func (r *LitestreamReconciler) markInvalid(ctx context.Context, resource *v1alpha1.Litestream, cause error) error {
	base := resource.DeepCopy()

	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               ReadyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInvalidConfiguration,
		ObservedGeneration: resource.Generation,
		Message:            cause.Error(),
	})
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.ConfigMapName = ""
	resource.Status.ConfigHash = ""

	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		return err
	}

	r.Recorder.Eventf(resource, nil, corev1.EventTypeWarning, ReasonInvalidConfiguration, ReasonInvalidConfiguration, "%s", cause.Error())
	return nil
}

func namespacedReferenceKey(namespace, name string) string {
	if name == "" {
		return ""
	}
	return namespace + "/" + name
}

func indexLitestreamSourceReplicaReference(object client.Object) []string {
	resource, ok := object.(*v1alpha1.Litestream)
	if !ok {
		return nil
	}
	keys := make(map[string]struct{}, len(resource.Spec.Databases))
	for _, binding := range resource.Spec.Databases {
		if binding.Restore == nil {
			continue
		}
		if key := namespacedReferenceKey(resource.Namespace, binding.Restore.ReplicaRef.Name); key != "" {
			keys[key] = struct{}{}
		}
	}
	return sortedKeys(keys)
}

func indexLitestreamDestinationReplicaReference(object client.Object) []string {
	resource, ok := object.(*v1alpha1.Litestream)
	if !ok {
		return nil
	}
	keys := make(map[string]struct{}, len(resource.Spec.Databases))
	for _, binding := range resource.Spec.Databases {
		if binding.Replicate == nil {
			continue
		}
		if key := namespacedReferenceKey(resource.Namespace, binding.Replicate.ReplicaRef.Name); key != "" {
			keys[key] = struct{}{}
		}
	}
	return sortedKeys(keys)
}

func sortedKeys(keys map[string]struct{}) []string {
	result := make([]string, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func (r *LitestreamReconciler) mapReplicaToLitestreams(ctx context.Context, object client.Object) []reconcile.Request {
	replica, ok := object.(*v1alpha1.LitestreamReplica)
	if !ok {
		return nil
	}
	key := namespacedReferenceKey(replica.Namespace, replica.Name)
	requests := newRequestSet(r.litestreamRequests(ctx, litestreamSourceReplicaReferenceIndex, key))
	for _, request := range r.litestreamRequests(ctx, litestreamDestinationReplicaReferenceIndex, key) {
		requests.add(request)
	}
	return requests.requests()
}

func (r *LitestreamReconciler) litestreamRequests(ctx context.Context, field, key string) []reconcile.Request {
	if key == "" {
		return nil
	}
	var resources v1alpha1.LitestreamList
	if err := r.List(ctx, &resources, client.MatchingFields{field: key}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list Litestream dependency consumers", "field", field, "key", key)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(resources.Items))
	for i := range resources.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&resources.Items[i])})
	}
	return requests
}

type requestSet map[client.ObjectKey]reconcile.Request

func newRequestSet(requests []reconcile.Request) requestSet {
	set := make(requestSet, len(requests))
	for _, request := range requests {
		set.add(request)
	}
	return set
}

func (s requestSet) add(request reconcile.Request) {
	s[request.NamespacedName] = request
}

func (s requestSet) requests() []reconcile.Request {
	keys := make([]client.ObjectKey, 0, len(s))
	for key := range s {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace == keys[j].Namespace {
			return keys[i].Name < keys[j].Name
		}
		return keys[i].Namespace < keys[j].Namespace
	})
	requests := make([]reconcile.Request, 0, len(keys))
	for _, key := range keys {
		requests = append(requests, s[key])
	}
	return requests
}

// SetupWithManager wires the reconciler into mgr, watching Litestream
// resources (on spec changes only) and the ConfigMaps they own.
func (r *LitestreamReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Litestream{}, litestreamSourceReplicaReferenceIndex, indexLitestreamSourceReplicaReference); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Litestream{}, litestreamDestinationReplicaReferenceIndex, indexLitestreamDestinationReplicaReference); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Litestream{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.ConfigMap{}).
		Watches(&v1alpha1.LitestreamReplica{}, handler.EnqueueRequestsFromMapFunc(r.mapReplicaToLitestreams)).
		Named("litestream").
		Complete(r)
}
