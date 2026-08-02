package controller

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newDependencyTestReconciler(t *testing.T, objects ...client.Object) (client.Client, *LitestreamReconciler) {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Litestream{}).
		WithIndex(&v1alpha1.Litestream{}, litestreamSourceReplicaReferenceIndex, indexLitestreamSourceReplicaReference).
		WithIndex(&v1alpha1.Litestream{}, litestreamDestinationReplicaReferenceIndex, indexLitestreamDestinationReplicaReference).
		WithObjects(objects...).
		Build()

	return fakeClient, &LitestreamReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(20),
		Resolver: resolver.Resolver{Reader: fakeClient},
	}
}

func dependencyReplica(name, namespace, bucket string) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
			Type: v1alpha1.ReplicaTypeS3,
			S3:   &v1alpha1.S3ReplicaSpec{Bucket: bucket, Path: name},
		}},
	}
}

func dependencyProfile(name, namespace, path, source, destination string, uid types.UID) *v1alpha1.Litestream {
	profile := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: uid, Generation: 1},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{{
			Name: "app",
			Path: path,
		}}},
	}
	binding := &profile.Spec.Databases[0]
	if source != "" {
		binding.Restore = &v1alpha1.RestoreSpec{ReplicaRef: corev1.LocalObjectReference{Name: source}}
	}
	if destination != "" {
		binding.Replicate = &v1alpha1.ReplicateSpec{ReplicaRef: corev1.LocalObjectReference{Name: destination}}
	}
	return profile
}

func reconcileDependencyProfile(t *testing.T, r *LitestreamReconciler, profile *v1alpha1.Litestream) v1alpha1.Litestream {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(profile)})
	assert.NilError(t, err)

	var got v1alpha1.Litestream
	assert.NilError(t, r.Get(context.Background(), client.ObjectKeyFromObject(profile), &got))
	return got
}

func TestIndexLitestreamSourceReplicaReference(t *testing.T) {
	resource := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: "profile", Namespace: "team-a"},
		Spec: v1alpha1.LitestreamSpec{Databases: []v1alpha1.DatabaseBinding{
			{Name: "first", Path: "/data/first.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: corev1.LocalObjectReference{Name: "source-b"}}},
			{Name: "second", Path: "/data/second.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: corev1.LocalObjectReference{Name: "source-a"}}},
			{Name: "duplicate", Path: "/data/duplicate.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: corev1.LocalObjectReference{Name: "source-a"}}},
			{Name: "replicate-only", Path: "/data/replicate.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: corev1.LocalObjectReference{Name: "destination"}}},
		}},
	}

	assert.DeepEqual(t, indexLitestreamSourceReplicaReference(resource), []string{"team-a/source-a", "team-a/source-b"})
}

func TestReplicaSourceChangeReconcilesEveryDirectConsumer(t *testing.T) {
	const namespace = "team-a"
	source := dependencyReplica("source", namespace, "source-v1")
	first := dependencyProfile("first", namespace, "/data/first.db", source.Name, "first-destination", "11111111")
	second := dependencyProfile("second", namespace, "/data/second.db", source.Name, "second-destination", "22222222")
	firstDestination := dependencyReplica("first-destination", namespace, "first-v1")
	secondDestination := dependencyReplica("second-destination", namespace, "second-v1")
	fakeClient, r := newDependencyTestReconciler(t, source, first, second, firstDestination, secondDestination)

	firstBefore := reconcileDependencyProfile(t, r, first)
	secondBefore := reconcileDependencyProfile(t, r, second)

	var changedSource v1alpha1.LitestreamReplica
	assert.NilError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(source), &changedSource))
	changedSource.Spec.Replica.S3.Bucket = "source-v2"
	assert.NilError(t, fakeClient.Update(context.Background(), &changedSource))

	assertRequests(t, r.mapReplicaToLitestreams(context.Background(), &changedSource), "team-a/first", "team-a/second")
	firstAfter := reconcileDependencyProfile(t, r, first)
	secondAfter := reconcileDependencyProfile(t, r, second)

	assert.Assert(t, firstAfter.Status.ConfigHash != firstBefore.Status.ConfigHash)
	assert.Assert(t, secondAfter.Status.ConfigHash != secondBefore.Status.ConfigHash)
	assert.Assert(t, firstAfter.Status.ConfigMapName != firstBefore.Status.ConfigMapName)
	assert.Assert(t, secondAfter.Status.ConfigMapName != secondBefore.Status.ConfigMapName)
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: firstAfter.Status.ConfigMapName}, &corev1.ConfigMap{}))
	assert.NilError(t, fakeClient.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: secondAfter.Status.ConfigMapName}, &corev1.ConfigMap{}))
}

func TestDependencyMapsUseNamespacedReplicaReferences(t *testing.T) {
	const namespace = "team-a"
	source := dependencyReplica("source", namespace, "source")
	destination := dependencyReplica("destination", namespace, "destination")
	first := dependencyProfile("first", namespace, "/data/first.db", source.Name, "first-destination", "11111111")
	second := dependencyProfile("second", namespace, "/data/second.db", source.Name, "second-destination", "22222222")
	direct := dependencyProfile("direct", namespace, "/data/direct.db", "", destination.Name, "33333333")
	otherNamespaceProfile := dependencyProfile("other", "team-b", "/data/other.db", source.Name, destination.Name, "44444444")
	_, r := newDependencyTestReconciler(t, source, destination, first, second, direct, otherNamespaceProfile)

	assertRequests(t, r.mapReplicaToLitestreams(context.Background(), destination), "team-a/direct")
	assertRequests(t, r.mapReplicaToLitestreams(context.Background(), source), "team-a/first", "team-a/second")
}

func TestReplicaSourceAndDestinationChangeDeduplicatesConsumers(t *testing.T) {
	replica := dependencyReplica("shared", "team-a", "shared")
	profile := dependencyProfile("app", "team-a", "/data/app.db", replica.Name, replica.Name, "11111111")
	_, r := newDependencyTestReconciler(t, replica, profile)

	assertRequests(t, r.mapReplicaToLitestreams(context.Background(), replica), "team-a/app")
}

func TestReconcileMissingSourceReplicaMarksNotReadyWithoutConfigMap(t *testing.T) {
	profile := dependencyProfile("app", "team-a", "/data/app.db", "missing", "destination", "11111111")
	destination := dependencyReplica("destination", "team-a", "destination")
	fakeClient, r := newDependencyTestReconciler(t, profile, destination)

	got := reconcileDependencyProfile(t, r, profile)
	condition := meta.FindStatusCondition(got.Status.Conditions, ReadyConditionType)
	assert.Assert(t, condition != nil)
	assert.Equal(t, condition.Status, metav1.ConditionFalse)
	assert.Equal(t, condition.Reason, ReasonInvalidConfiguration)
	assert.Equal(t, got.Status.ConfigMapName, "")
	assert.Equal(t, got.Status.ConfigHash, "")

	var configMaps corev1.ConfigMapList
	assert.NilError(t, fakeClient.List(context.Background(), &configMaps, client.InNamespace(profile.Namespace)))
	assert.Equal(t, len(configMaps.Items), 0)
	assert.Assert(t, apierrors.IsNotFound(fakeClient.Get(context.Background(), types.NamespacedName{Namespace: profile.Namespace, Name: ConfigMapName(profile)}, &corev1.ConfigMap{})))
}

func TestReconcileRetriesTransientReplicaReadFailure(t *testing.T) {
	profile := dependencyProfile("app", "team-a", "/data/app.db", "source", "", "11111111")
	source := dependencyReplica("source", "team-a", "source")
	fakeClient, r := newDependencyTestReconciler(t, profile, source)
	r.Resolver.Reader = failingReader{
		Reader: fakeClient,
		key:    client.ObjectKeyFromObject(source),
		err:    errors.New("temporary replica read failure"),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(profile)})

	assert.ErrorContains(t, err, "temporary replica read failure")
}

type failingReader struct {
	client.Reader
	key client.ObjectKey
	err error
}

func (r failingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == r.key {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func assertRequests(t *testing.T, requests []ctrl.Request, want ...string) {
	t.Helper()
	got := make([]string, 0, len(requests))
	for _, request := range requests {
		got = append(got, request.Namespace+"/"+request.Name)
	}
	sort.Strings(got)
	sort.Strings(want)
	assert.DeepEqual(t, got, want)
}
