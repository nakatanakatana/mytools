package k8s

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func newFakeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxClaim"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxClaimList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplate"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxTemplateList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxWarmPool"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "extensions.agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxWarmPoolList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "Sandbox"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "SandboxList"}, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList"}, &unstructured.UnstructuredList{})
	return s
}

func newTestClient(t *testing.T, objs ...runtime.Object) (*KubeClient, *k8sfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	var (
		coreObjs []runtime.Object
		dynObjs  []runtime.Object
	)
	for _, o := range objs {
		if _, ok := o.(*unstructured.Unstructured); ok {
			dynObjs = append(dynObjs, o)
		} else {
			coreObjs = append(coreObjs, o)
		}
	}
	core := k8sfake.NewClientset(coreObjs...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		newFakeScheme(),
		map[schema.GroupVersionResource]string{
			SandboxClaimGVR:    "SandboxClaimList",
			SandboxTemplateGVR: "SandboxTemplateList",
			SandboxWarmPoolGVR: "SandboxWarmPoolList",
			SandboxGVR:         "SandboxList",
			VolumeSnapshotGVR:  "VolumeSnapshotList",
		},
		dynObjs...,
	)
	return NewClient("sandbox", nil, core, dyn), core, dyn
}

func TestKubeClient_WarmPoolsClaimsSandboxesCheckpoints(t *testing.T) {
	wp := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
			"kind":       "SandboxWarmPool",
			"metadata":   map[string]any{"name": "opencode-ephemeral-pool", "namespace": "sandbox"},
			"spec": map[string]any{
				"replicas":           int64(2),
				"sandboxTemplateRef": map[string]any{"name": "opencode-ephemeral"},
			},
			"status": map[string]any{
				"replicas":      int64(2),
				"readyReplicas": int64(1),
			},
		},
	}
	tmpl := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
			"kind":       "SandboxTemplate",
			"metadata":   map[string]any{"name": "opencode-persistent", "namespace": "sandbox"},
			"spec": map[string]any{
				"podTemplate": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "opencode-sandbox"}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "opencode"}}},
				},
				"volumeClaimTemplates": []any{
					map[string]any{
						"metadata": map[string]any{"name": "workspace"},
						"spec": map[string]any{
							"storageClassName": "rook-ceph-block",
							"accessModes":      []any{"ReadWriteOnce"},
						},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-1",
			Namespace: "sandbox",
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	client, _, dyn := newTestClient(t, wp, tmpl, pod)
	ctx := t.Context()

	pools, err := client.ListWarmPools(ctx)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	assert.Equal(t, int64(1), pools[0].ReadyReplicas)
	assert.Equal(t, int64(2), pools[0].DesiredReplicas)

	// Create ephemeral claim with TTL.
	deadline := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, client.CreateClaim(ctx, "eph-1", "opencode-ephemeral-pool", &deadline))
	claims, err := client.ListClaims(ctx)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, "eph-1", claims[0].Name)
	assert.Equal(t, "opencode-ephemeral-pool", claims[0].WarmPoolName)
	require.NotNil(t, claims[0].ShutdownTime)

	// Create persistent sandbox forked from checkpoint.
	require.NoError(t, client.CreateSandboxFromTemplate(ctx, CreateSandboxOptions{
		Name:                "dev-1",
		TemplateName:        "opencode-persistent",
		WorkspaceVolumeName: "workspace",
		CheckpointName:      "cp-base",
		ShutdownTime:        &deadline,
	}))

	rawSB, err := dyn.Resource(SandboxGVR).Namespace("sandbox").Get(ctx, "dev-1", metav1.GetOptions{})
	require.NoError(t, err)
	vcts, found, err := unstructured.NestedSlice(rawSB.Object, "spec", "volumeClaimTemplates")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, vcts, 1)
	dsName, _, _ := unstructured.NestedString(vcts[0].(map[string]any), "spec", "dataSource", "name")
	assert.Equal(t, "cp-base", dsName)

	// Suspend and Resume sandbox.
	require.NoError(t, client.SetSandboxOperatingMode(ctx, "dev-1", OperatingModeSuspended))
	sb, err := client.GetSandbox(ctx, "dev-1")
	require.NoError(t, err)
	assert.True(t, sb.Suspended)
	assert.Equal(t, "Suspended", sb.Phase)

	require.NoError(t, client.SetSandboxOperatingMode(ctx, "dev-1", OperatingModeRunning))
	sb, err = client.GetSandbox(ctx, "dev-1")
	require.NoError(t, err)
	assert.False(t, sb.Suspended)

	// Resolve pod by sandbox name.
	podName, err := client.ResolvePodName(ctx, sb)
	require.NoError(t, err)
	assert.Equal(t, "dev-1", podName)

	// Checkpoint create / list / delete.
	require.NoError(t, client.CreateCheckpoint(ctx, "cp-1", "workspace-dev-1", "rook-ceph-rbd-snapshot", "dev-1"))
	cps, err := client.ListCheckpoints(ctx)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	assert.Equal(t, "workspace-dev-1", cps[0].SourcePVC)
	assert.Equal(t, "rook-ceph-rbd-snapshot", cps[0].SnapshotClassName)

	require.NoError(t, client.DeleteCheckpoint(ctx, "cp-1"))
	require.NoError(t, client.DeleteSandbox(ctx, "dev-1"))
	require.NoError(t, client.DeleteClaim(ctx, "eph-1"))
}
