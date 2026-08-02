package controller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/controller"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	return scheme
}

// newTestReconciler builds a fake client seeded with objs and a reconciler
// backed by it, sharing one scheme so decoded objects match expected types.
func newTestReconciler(t *testing.T, recorder *events.FakeRecorder, objs ...client.Object) (client.Client, *controller.LitestreamReconciler) {
	t.Helper()
	scheme := newScheme(t)
	objects := append([]client.Object{}, objs...)
	objects = append(objects, defaultReplica())
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Litestream{}).
		WithObjects(objects...).
		Build()
	return fakeClient, &controller.LitestreamReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: recorder,
		Resolver: resolver.Resolver{Reader: fakeClient},
	}
}

func defaultReplica() *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		ObjectMeta: metav1.ObjectMeta{Name: "replica", Namespace: "default"},
		Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
			Type: v1alpha1.ReplicaTypeS3,
			S3:   &v1alpha1.S3ReplicaSpec{Bucket: "example-bucket", Path: "app"},
		}},
	}
}

func render(t *testing.T, reader client.Reader, resource *v1alpha1.Litestream) litestreamconfig.RenderedConfig {
	t.Helper()
	input, err := (resolver.Resolver{Reader: reader}).Resolve(context.Background(), resource)
	assert.NilError(t, err)
	rendered, err := litestreamconfig.Render(input)
	assert.NilError(t, err)
	return rendered
}

func validLitestream(name string, uid types.UID, generation int64) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			UID:        uid,
			Generation: generation,
		},
		Spec: v1alpha1.LitestreamSpec{
			Databases: []v1alpha1.DatabaseBinding{
				{
					Name: "app",
					Path: "/var/lib/app/app.db",
					Replicate: &v1alpha1.ReplicateSpec{
						ReplicaRef: corev1.LocalObjectReference{Name: "replica"},
					},
				},
			},
		},
	}
}

func invalidLitestream(name string, uid types.UID, generation int64) *v1alpha1.Litestream {
	cr := validLitestream(name, uid, generation)
	cr.Spec.Databases = nil
	return cr
}

func reconcileRequest(cr *v1alpha1.Litestream) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}}
}

func TestReconcileCreatesOwnedConfigMap(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 1)
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	rendered := render(t, fakeClient, cr)
	wantName := controller.ConfigMapNameForHash(cr, rendered.Hash)
	assert.Equal(t, wantName, "litestream-app-11111111-"+rendered.Hash)

	var cm corev1.ConfigMap
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: wantName, Namespace: "default"}, &cm))

	assert.DeepEqual(t, cm.Data, rendered.Data)
	assert.Assert(t, cm.Immutable != nil && *cm.Immutable)

	assert.Equal(t, cm.Labels[controller.LabelResourceName], "app")
	assert.Equal(t, cm.Labels[controller.LabelResourceUID], string(cr.UID))
	assert.Equal(t, cm.Labels[controller.LabelManagedBy], controller.ManagedByValue)

	assert.Equal(t, len(cm.OwnerReferences), 1)
	owner := cm.OwnerReferences[0]
	assert.Equal(t, owner.Name, "app")
	assert.Equal(t, owner.UID, cr.UID)
	assert.Equal(t, owner.Kind, "Litestream")
	assert.Assert(t, owner.Controller != nil && *owner.Controller)
	assert.Assert(t, owner.BlockOwnerDeletion != nil && !*owner.BlockOwnerDeletion)

	var got v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &got))
	assert.Equal(t, got.Status.ConfigMapName, wantName)
	assert.Equal(t, got.Status.ConfigHash, rendered.Hash)
	assert.Equal(t, got.Status.ObservedGeneration, int64(1))

	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	assert.Assert(t, cond != nil, "expected a Ready condition")
	assert.Equal(t, cond.Status, metav1.ConditionTrue)
	assert.Equal(t, cond.ObservedGeneration, int64(1))

	assertEventContains(t, recorder, "Normal", "ConfigRendered")
}

func TestConfigMapNameIsBoundedForMaximumResourceName(t *testing.T) {
	cr := validLitestream(strings.Repeat("a", validation.DNS1123SubdomainMaxLength), types.UID("11111111-2222-3333-4444-555555555555"), 1)

	name := controller.ConfigMapName(cr)

	assert.Assert(t, len(name) <= validation.DNS1123SubdomainMaxLength, "ConfigMap name %q exceeds the DNS subdomain limit", name)
	assert.Equal(t, len(validation.IsDNS1123Subdomain(name)), 0)
	assert.Assert(t, strings.HasSuffix(name, "-11111111"))
}

func TestConfigMapNameRemovesTruncatedResourceNameDot(t *testing.T) {
	uid := types.UID("11111111-2222-3333-4444-555555555555")
	resourceNameLength := validation.DNS1123SubdomainMaxLength - len("litestream-") - len("-11111111")
	resourceName := strings.Repeat("a", resourceNameLength-1) + "." + strings.Repeat("b", 20)
	cr := validLitestream(resourceName, uid, 1)

	name := controller.ConfigMapName(cr)

	assert.Equal(t, len(validation.IsDNS1123Subdomain(name)), 0, "ConfigMap name %q is invalid", name)
}

func TestConfigMapNameForHashIsBounded(t *testing.T) {
	cr := validLitestream(strings.Repeat("a", validation.DNS1123SubdomainMaxLength), types.UID("11111111-2222-3333-4444-555555555555"), 1)
	hash := strings.Repeat("0", 64)

	name := controller.ConfigMapNameForHash(cr, hash)

	assert.Assert(t, len(name) <= validation.DNS1123SubdomainMaxLength, "ConfigMap name %q exceeds the DNS subdomain limit", name)
	assert.Equal(t, len(validation.IsDNS1123Subdomain(name)), 0)
	assert.Assert(t, strings.HasSuffix(name, "-"+hash))
}

func TestConfigMapNameForHashKeepsLongResourceNamesDistinct(t *testing.T) {
	commonName := strings.Join([]string{
		strings.Repeat("a", 63),
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
	}, ".")
	first := validLitestream(commonName+"."+strings.Repeat("d", 60), types.UID("11111111-2222-3333-4444-555555555555"), 1)
	second := validLitestream(commonName+"."+strings.Repeat("e", 60), types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), 1)
	hash := strings.Repeat("0", 64)

	firstName := controller.ConfigMapNameForHash(first, hash)
	secondName := controller.ConfigMapNameForHash(second, hash)

	assert.Assert(t, firstName != secondName, "long resource names must not produce colliding ConfigMap names: %q", firstName)
}

func TestReconcileBoundsLongResourceNameMetadata(t *testing.T) {
	resourceName := strings.Repeat("a", validation.LabelValueMaxLength+10)
	cr := validLitestream(resourceName, types.UID("11111111-2222-3333-4444-555555555555"), 1)
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	rendered := render(t, fakeClient, cr)
	var cm corev1.ConfigMap
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      controller.ConfigMapNameForHash(cr, rendered.Hash),
		Namespace: cr.Namespace,
	}, &cm))

	label := cm.Labels[controller.LabelResourceName]
	assert.Assert(t, len(label) <= validation.LabelValueMaxLength)
	assert.Equal(t, len(validation.IsValidLabelValue(label)), 0)
	assert.Equal(t, cm.Annotations[controller.AnnotationResourceName], resourceName)
}

func TestReconcileUpdatesConfigMapOnSpecChange(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 1)
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	beforeRendered := render(t, fakeClient, cr)
	wantName := controller.ConfigMapNameForHash(cr, beforeRendered.Hash)
	var before corev1.ConfigMap
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: wantName, Namespace: "default"}, &before))

	var toUpdate v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &toUpdate))
	toUpdate.Spec.Databases[0].Replicate.AutoRecover = true
	toUpdate.Generation = 2
	assert.NilError(t, fakeClient.Update(context.Background(), &toUpdate))

	_, err = r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	rendered := render(t, fakeClient, &toUpdate)
	newName := controller.ConfigMapNameForHash(&toUpdate, rendered.Hash)
	var after corev1.ConfigMap
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: newName, Namespace: "default"}, &after))
	assert.Assert(t, before.Data["replicate.yml"] != after.Data["replicate.yml"])
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: wantName, Namespace: "default"}, &corev1.ConfigMap{}),
		"the previous immutable ConfigMap revision must remain available for concurrently admitted Pods")

	var got v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &got))
	assert.Assert(t, got.Status.ConfigHash != "")
	assert.Equal(t, got.Status.ConfigHash, rendered.Hash)
	assert.Equal(t, got.Status.ObservedGeneration, int64(2))
}

func TestReconcileRetainsAllOwnedConfigMapRevisionsWithoutPods(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 2)
	staleName := controller.ConfigMapNameForHash(cr, strings.Repeat("0", 64))
	undiscoveredName := controller.ConfigMapNameForHash(cr, strings.Repeat("1", 64))
	cr.Status.ConfigMapName = staleName

	owner := metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "Litestream",
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: ptr.To(true),
	}
	stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: staleName, Namespace: cr.Namespace,
		Labels:          map[string]string{controller.LabelResourceUID: string(cr.UID)},
		OwnerReferences: []metav1.OwnerReference{owner},
	}}
	undiscovered := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: undiscoveredName, Namespace: cr.Namespace,
		Labels:          map[string]string{controller.LabelResourceUID: string(cr.UID)},
		OwnerReferences: []metav1.OwnerReference{owner},
	}}
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr, stale, undiscovered)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	for _, name := range []string{staleName, undiscoveredName} {
		assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: cr.Namespace}, &corev1.ConfigMap{}),
			"owned ConfigMap revision %q must be retained even before a concurrently admitted Pod is visible", name)
	}
}

func TestReconcilePreservesConfigMapRevisionReferencedByPod(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 2)
	staleName := controller.ConfigMapNameForHash(cr, strings.Repeat("0", 64))

	owner := metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "Litestream",
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: ptr.To(true),
	}
	stale := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: staleName, Namespace: cr.Namespace,
		Labels:          map[string]string{controller.LabelResourceUID: string(cr.UID)},
		OwnerReferences: []metav1.OwnerReference{owner},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-pod", Namespace: cr.Namespace},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "litestream-config",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: staleName},
				}},
			}},
			Containers: []corev1.Container{{Name: "app", Image: "example.com/app:1.0"}},
		},
	}
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr, stale, pod)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{
		Name: staleName, Namespace: cr.Namespace,
	}, &corev1.ConfigMap{}), "a ConfigMap referenced by an existing Pod must be retained")
}

func TestReconcileInvalidSpecSetsReadyFalse(t *testing.T) {
	cr := invalidLitestream("bad", types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), 3)
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	wantName := controller.ConfigMapName(cr)
	var cm corev1.ConfigMap
	getErr := fakeClient.Get(context.Background(), types.NamespacedName{Name: wantName, Namespace: "default"}, &cm)
	assert.Assert(t, apierrors.IsNotFound(getErr), "expected no ConfigMap to be created for an invalid spec")

	var got v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	assert.Assert(t, cond != nil, "expected a Ready condition")
	assert.Equal(t, cond.Status, metav1.ConditionFalse)
	assert.Equal(t, cond.Reason, "InvalidConfiguration")
	assert.Equal(t, cond.ObservedGeneration, int64(3))
	assert.Equal(t, got.Status.ObservedGeneration, int64(3))

	assertEventContains(t, recorder, "Warning", "InvalidConfiguration")
}

func TestReconcileValidToInvalidClearsStatusAndRetainsConfigMap(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 1)
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	rendered := render(t, fakeClient, cr)
	wantName := controller.ConfigMapNameForHash(cr, rendered.Hash)
	var cm corev1.ConfigMap
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: wantName, Namespace: "default"}, &cm))

	var ready v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &ready))
	assert.Equal(t, ready.Status.ConfigMapName, wantName)
	assert.Assert(t, ready.Status.ConfigHash != "")

	var toUpdate v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &toUpdate))
	toUpdate.Spec.Databases = nil
	toUpdate.Generation = 2
	assert.NilError(t, fakeClient.Update(context.Background(), &toUpdate))

	recorder = events.NewFakeRecorder(10)
	r.Recorder = recorder

	_, err = r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: wantName, Namespace: "default"}, &cm),
		"an existing Pod must retain its immutable ConfigMap revision when the Litestream resource becomes invalid")

	var got v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &got))

	cond := meta.FindStatusCondition(got.Status.Conditions, "Ready")
	assert.Assert(t, cond != nil, "expected a Ready condition")
	assert.Equal(t, cond.Status, metav1.ConditionFalse)
	assert.Equal(t, cond.Reason, "InvalidConfiguration")
	assert.Equal(t, cond.ObservedGeneration, int64(2))
	assert.Equal(t, got.Status.ObservedGeneration, int64(2))
	assert.Equal(t, got.Status.ConfigMapName, "")
	assert.Equal(t, got.Status.ConfigHash, "")

	assertEventContains(t, recorder, "Warning", "InvalidConfiguration")
}

func TestReconcileInvalidSpecPreservesUnownedConfigMap(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 1)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      controller.ConfigMapName(cr),
		Namespace: cr.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "Litestream",
			Name:       "other",
			UID:        types.UID("99999999-8888-7777-6666-555555555555"),
		}},
	}}
	cr.Spec.Databases = nil
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr, cm)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)
	assert.NilError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}))
}

func TestReconcileInvalidSpecWithMissingConfigMapSucceeds(t *testing.T) {
	cr := invalidLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 1)
	recorder := events.NewFakeRecorder(10)
	_, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)
}

func TestReconcileMissingResourceIsIgnored(t *testing.T) {
	recorder := events.NewFakeRecorder(10)
	_, r := newTestReconciler(t, recorder)

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing", Namespace: "default"}})
	assert.NilError(t, err)
	assert.Equal(t, result, ctrl.Result{})
}

func TestReconcileIgnoresDeletingResource(t *testing.T) {
	cr := validLitestream("app", types.UID("11111111-2222-3333-4444-555555555555"), 1)
	deletionTimestamp := metav1.Now()
	cr.DeletionTimestamp = &deletionTimestamp
	cr.Finalizers = []string{"litestream.mytools.nakatanaka.app/finalizer"}
	recorder := events.NewFakeRecorder(10)
	fakeClient, r := newTestReconciler(t, recorder, cr)

	_, err := r.Reconcile(context.Background(), reconcileRequest(cr))
	assert.NilError(t, err)

	var cm corev1.ConfigMap
	getErr := fakeClient.Get(context.Background(), types.NamespacedName{Name: controller.ConfigMapName(cr), Namespace: cr.Namespace}, &cm)
	assert.Assert(t, apierrors.IsNotFound(getErr), "expected no ConfigMap while Litestream is deleting")

	var got v1alpha1.Litestream
	assert.NilError(t, fakeClient.Get(context.Background(), reconcileRequest(cr).NamespacedName, &got))
	assert.Equal(t, got.Status.ConfigMapName, "")
	assert.Equal(t, got.Status.ConfigHash, "")
}

func assertEventContains(t *testing.T, recorder *events.FakeRecorder, eventType, reason string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		assert.Assert(t, strings.Contains(event, eventType), event)
		assert.Assert(t, strings.Contains(event, reason), event)
	default:
		t.Fatalf("expected a %s %s event to be recorded", eventType, reason)
	}
}
