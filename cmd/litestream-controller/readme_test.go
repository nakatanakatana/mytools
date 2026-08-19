package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// annotationPrefix is the only prefix any annotation key in a sample
// manifest may use, so a copy-pasted sample never teaches an operator to
// key an unrelated annotation.
const annotationPrefix = v1alpha1.Group + "/"

// documentationDecoder builds the same "Kubernetes universal deserializer"
// the API server's own clients use, extended with the Litestream types, so
// every sample (Litestream custom resources, Deployments, StatefulSets) can
// be decoded the way a real client would.
func documentationDecoder(t *testing.T) runtime.Decoder {
	t.Helper()
	return serializer.NewCodecFactory(documentationScheme(t)).UniversalDeserializer()
}

func documentationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	return scheme
}

// sampleManifestPaths returns every sample manifest, failing loudly if the
// directory is missing or empty rather than silently passing zero cases.
func sampleManifestPaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "litestream-controller", "samples", "*.yaml"))
	assert.NilError(t, err)
	assert.Assert(t, len(paths) > 0, "expected at least one manifest under config/litestream-controller/samples")
	return paths
}

type documentationSample struct {
	path   string
	object client.Object
}

// documentationSamples decodes every sample into a client object so sample
// tests exercise the same Kubernetes types the controller uses at runtime.
func documentationSamples(t *testing.T) []documentationSample {
	t.Helper()
	decoder := documentationDecoder(t)
	samples := make([]documentationSample, 0)
	for _, path := range sampleManifestPaths(t) {
		raw, err := os.ReadFile(path)
		assert.NilError(t, err, "read %s", path)

		object, _, err := decoder.Decode(raw, nil, nil)
		assert.NilError(t, err, "decode %s", path)

		clientObject, ok := object.(client.Object)
		assert.Assert(t, ok, "%s: decoded object %T is not a client object", path, object)
		samples = append(samples, documentationSample{path: path, object: clientObject})
	}
	return samples
}

// TestDocumentationSamplesUseNamespacedAnnotations decodes every sample
// manifest through the universal deserializer and asserts that any
// annotation it carries is namespaced under the Litestream API group, so
// copy-pasting a sample never leaves a stray, unnamespaced annotation key.
func TestDocumentationSamplesUseNamespacedAnnotations(t *testing.T) {
	decoder := documentationDecoder(t)

	for _, path := range sampleManifestPaths(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			assert.NilError(t, err)

			object, _, err := decoder.Decode(raw, nil, nil)
			assert.NilError(t, err, "decode %s", path)

			accessor, err := meta.Accessor(object)
			assert.NilError(t, err)

			for key := range accessor.GetAnnotations() {
				assert.Assert(t, strings.HasPrefix(key, annotationPrefix),
					"%s: annotation %q must begin with %q", path, key, annotationPrefix)
			}

			for key := range podTemplateAnnotations(object) {
				assert.Assert(t, strings.HasPrefix(key, annotationPrefix),
					"%s: pod template annotation %q must begin with %q", path, key, annotationPrefix)
			}
		})
	}
}

// TestDocumentationSamplesUseSharedResources proves the sample set teaches
// the inline database model. Each Litestream binding owns its local path and
// direct restore and replication replica references.
func TestDocumentationSamplesUseSharedResources(t *testing.T) {
	samples := documentationSamples(t)
	objectsByFile := make(map[string]client.Object, len(samples))
	for _, sample := range samples {
		objectsByFile[filepath.Base(sample.path)] = sample.object
	}

	for _, path := range sampleManifestPaths(t) {
		raw, err := os.ReadFile(path)
		assert.NilError(t, err, "read %s", path)
		for _, removed := range []string{"databaseRef", "restoreFrom", "LitestreamDatabase"} {
			assert.Assert(t, !strings.Contains(string(raw), removed),
				"%s must not contain the removed %q API", path, removed)
		}
	}

	for _, file := range []string{
		"litestream_replica_source.yaml",
		"litestream_replica_destination.yaml",
		"litestream_replica_clone_destination.yaml",
	} {
		_, ok := objectsByFile[file].(*v1alpha1.LitestreamReplica)
		assert.Assert(t, ok, "%s must decode as LitestreamReplica", file)
	}

	type expectedBinding struct {
		restoreReplica   string
		replicateReplica string
	}
	expectedBindings := map[string]expectedBinding{
		"litestream_v1alpha1_restore_only.yaml": {restoreReplica: "app-db-source"},
		"litestream_v1alpha1_replicate.yaml":    {replicateReplica: "app-db-destination"},
		"litestream_v1alpha1_clone_pr.yaml": {
			restoreReplica:   "app-db-source",
			replicateReplica: "app-db-clone-destination",
		},
	}

	for file, expected := range expectedBindings {
		resource, ok := objectsByFile[file].(*v1alpha1.Litestream)
		assert.Assert(t, ok, "%s must decode as Litestream", file)
		assert.Assert(t, len(resource.Spec.Databases) == 1, "%s must contain one database binding", file)
		binding := resource.Spec.Databases[0]
		assert.Equal(t, binding.Path, "/data/app.db")

		if expected.restoreReplica == "" {
			assert.Assert(t, binding.Restore == nil, "%s must omit restore", file)
		} else {
			assert.Assert(t, binding.Restore != nil, "%s must configure restore", file)
			assert.Equal(t, binding.Restore.ReplicaRef.Name, expected.restoreReplica)
		}

		if expected.replicateReplica == "" {
			assert.Assert(t, binding.Replicate == nil, "%s must omit replicate", file)
		} else {
			assert.Assert(t, binding.Replicate != nil, "%s must configure replicate", file)
			assert.Equal(t, binding.Replicate.ReplicaRef.Name, expected.replicateReplica)
		}
	}

	clone := objectsByFile["litestream_v1alpha1_clone_pr.yaml"].(*v1alpha1.Litestream)
	cloneBinding := clone.Spec.Databases[0]
	assert.Assert(t, cloneBinding.Restore != nil, "clone sample must configure a restore source")
	assert.Assert(t, cloneBinding.Replicate != nil, "clone sample must configure a replication destination")
	assert.Assert(t, cloneBinding.Restore.ReplicaRef.Name != cloneBinding.Replicate.ReplicaRef.Name,
		"clone samples must use distinct source and destination Replica names")
}

// podTemplateAnnotations returns the Pod template annotations of a
// Deployment or StatefulSet sample, and nil for any other kind. Those
// annotations are what the mutating webhook actually reads, so they need
// the same check as the object's own annotations.
func podTemplateAnnotations(object runtime.Object) map[string]string {
	switch workload := object.(type) {
	case *appsv1.Deployment:
		return workload.Spec.Template.Annotations
	case *appsv1.StatefulSet:
		return workload.Spec.Template.Annotations
	default:
		return nil
	}
}

// TestDocumentationLitestreamSamplesRenderARestoreScript proves the claim
// deployment.yaml and statefulset.yaml rely on: every database, in every
// supported mode, renders a restore script into the ConfigMap (and
// therefore gets a restore init container injected by the webhook before
// the application container starts) — including plain "replicate" mode
// with no explicit "restore:" block, which restores from its own
// destination replica instead (conditional on that replica already
// having data; see deployment.yaml's comment for the exact condition).
// If that ever stopped being true, deployment.yaml's emptyDir would never
// be repopulated from the replica on reschedule at all.
func TestDocumentationLitestreamSamplesRenderARestoreScript(t *testing.T) {
	samples := documentationSamples(t)
	objects := make([]client.Object, 0, len(samples))
	for _, sample := range samples {
		objects = append(objects, sample.object)
	}
	reader := fake.NewClientBuilder().WithScheme(documentationScheme(t)).WithObjects(objects...).Build()

	for _, sample := range samples {
		resource, ok := sample.object.(*v1alpha1.Litestream)
		if !ok {
			continue
		}

		t.Run(filepath.Base(sample.path), func(t *testing.T) {
			input, err := (resolver.Resolver{Reader: reader}).Resolve(context.Background(), resource)
			assert.NilError(t, err)
			rendered, err := litestreamconfig.Render(input)
			assert.NilError(t, err)
			assert.Assert(t, len(rendered.Credentials) > 0,
				"%s: rendered configuration must preserve SecretKeySelector credential bindings", sample.path)
			for _, credential := range rendered.Credentials {
				assert.Assert(t, credential.SecretKeyRef.Name != "", "%s: credential Secret name must be retained", sample.path)
				assert.Assert(t, credential.SecretKeyRef.Key != "", "%s: credential Secret key must be retained", sample.path)
			}

			for _, database := range input.Databases {
				_, hasRestoreScript := rendered.Data[litestreamconfig.RestoreScriptFileName(database.Name)]
				assert.Assert(t, hasRestoreScript,
					"%s: database %q must render a restore script, so a Pod using it can safely use an ephemeral volume",
					sample.path, database.Name)
			}
		})
	}
}

// readOperationsReadme reads the operator-facing README for the
// litestream-controller command.
func readOperationsReadme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("README.md")
	assert.NilError(t, err)
	return string(raw)
}

// TestDocumentationReadmeCoversRequiredOperations asserts that the
// operator-facing README documents every operational behavior an
// installer must know about before running this controller in a cluster.
func TestDocumentationReadmeCoversRequiredOperations(t *testing.T) {
	readme := strings.Join(strings.Fields(readOperationsReadme(t)), " ")

	required := []string{
		"ghcr.io/nakatanakatana/litestream-controller",
		"v0.1.0",
		"digest: sha256:<multi-platform-manifest-digest>",
		"imagePullPolicy",
		"existing Pods",
		"rollout restart",
		"replicas: 1",
		"failurePolicy",
		"namespaceSelector",
		"litestream.mytools.nakatanakatana.app/injection=enabled",
		"`scale` subresources",
		"maxSurge: 0",
		"restore-only Litestream",
		"single-writer validation fail-open",
		"Label a workload namespace",
		"Secret",
		"fsGroup",
		"resume-or-create",
		"LitestreamReplica",
		"inline `path`",
		"restore.replicaRef",
		"same namespace",
		"Ready=False",
		"deletion is rejected",
		"all consuming Litestream resources",
		"new ConfigMap revision",
		"does not read Secret values",
		"must not have concurrent writers",
		"litestream_replica_clone_destination.yaml",
	}
	for _, want := range required {
		assert.Assert(t, strings.Contains(readme, want), "README.md must mention %q", want)
	}
}

func TestDocumentationReadmeDoesNotUseRemovedInlineReplicaField(t *testing.T) {
	readme := readOperationsReadme(t)
	assert.Assert(t, !strings.Contains(readme, "`replicate.replica`"),
		"README.md must not describe the removed inline replicate.replica field")
}

// TestDocumentationReadmeDocumentsRemoteCleanup asserts that the README
// warns that uninstalling the controller never deletes remote replica data,
// and tells the operator how to remove it themselves.
func TestDocumentationReadmeDocumentsRemoteCleanup(t *testing.T) {
	readme := readOperationsReadme(t)

	required := []string{
		"does not delete any remote replica data",
		"delete the objects at that replica's path directly in the backend",
	}
	for _, want := range required {
		assert.Assert(t, strings.Contains(readme, want), "README.md must document remote cleanup: %q", want)
	}
}
