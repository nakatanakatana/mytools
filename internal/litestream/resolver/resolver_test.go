package resolver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveInlineBindings(t *testing.T) {
	source := replica("team-a", "source", "source-bucket")
	destination := replica("team-a", "destination", "destination-bucket")
	shared := replica("team-a", "shared", "shared-bucket")
	resource := litestreamResource("team-a",
		v1alpha1.DatabaseBinding{
			Name: "restore", Path: "/var/lib/app/restore.db",
			Restore: &v1alpha1.RestoreSpec{ReplicaRef: localRef("source")},
		},
		v1alpha1.DatabaseBinding{
			Name: "clone", Path: "/var/lib/app/clone.db", ClonePolicy: v1alpha1.ClonePolicyRequireEmpty,
			Restore: &v1alpha1.RestoreSpec{
				ReplicaRef: localRef("source"), IfDatabaseExists: v1alpha1.IfDatabaseExistsOverwrite,
				IfReplicaMissing: v1alpha1.IfReplicaMissingFail, IntegrityCheck: v1alpha1.IntegrityCheckFull,
				Timestamp: "2026-08-02T10:00:00Z",
			},
			Replicate: replication("destination"),
		},
		v1alpha1.DatabaseBinding{
			Name: "equal", Path: "/var/lib/app/equal.db",
			Restore: &v1alpha1.RestoreSpec{ReplicaRef: localRef("shared")}, Replicate: replication("shared"),
		},
		v1alpha1.DatabaseBinding{
			Name: "replicate", Path: "/var/lib/app/replicate.db", Replicate: replication("destination"),
		},
	)
	resource.Spec.Image = v1alpha1.ImageSpec{Repository: "ghcr.io/example/litestream", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	resource.Spec.Injection.TargetContainer = "app"

	got, err := resolver.Resolver{Reader: newReader(t, source, destination, shared)}.Resolve(context.Background(), resource)
	assert.NilError(t, err)

	want := litestreamconfig.Input{
		Image: resource.Spec.Image, Injection: resource.Spec.Injection,
		Databases: []litestreamconfig.Database{
			{Name: "restore", Path: "/var/lib/app/restore.db", Restore: &litestreamconfig.Restore{Replica: source.Spec.Replica}},
			{
				Name: "clone", Path: "/var/lib/app/clone.db", Clone: true, ClonePolicy: v1alpha1.ClonePolicyRequireEmpty,
				Restore: &litestreamconfig.Restore{
					Replica: source.Spec.Replica, IfDatabaseExists: v1alpha1.IfDatabaseExistsOverwrite,
					IfReplicaMissing: v1alpha1.IfReplicaMissingFail, IntegrityCheck: v1alpha1.IntegrityCheckFull,
					Timestamp: "2026-08-02T10:00:00Z",
				},
				Replicate: &litestreamconfig.Replicate{Replica: destination.Spec.Replica, SyncInterval: metav1.Duration{Duration: time.Minute}, AutoRecover: true},
			},
			{
				Name: "equal", Path: "/var/lib/app/equal.db",
				Restore:   &litestreamconfig.Restore{Replica: shared.Spec.Replica},
				Replicate: &litestreamconfig.Replicate{Replica: shared.Spec.Replica, SyncInterval: metav1.Duration{Duration: time.Minute}, AutoRecover: true},
			},
			{Name: "replicate", Path: "/var/lib/app/replicate.db", Replicate: &litestreamconfig.Replicate{Replica: destination.Spec.Replica, SyncInterval: metav1.Duration{Duration: time.Minute}, AutoRecover: true}},
		},
	}
	assert.DeepEqual(t, got, want)
}

func TestResolveThenRenderDestinationOnlyUsesDestinationBootstrap(t *testing.T) {
	destination := replica("team-a", "destination", "destination-bucket")
	destination.Spec.Replica.S3.Credentials.SecretAccessKey = &v1alpha1.SecretReference{
		SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: localRef("destination-credentials"), Key: "secret-access-key"},
	}
	resource := litestreamResource("team-a", v1alpha1.DatabaseBinding{
		Name: "app", Path: "/var/lib/app/app.db", Replicate: replication("destination"),
	})

	input, err := resolver.Resolver{Reader: newReader(t, destination)}.Resolve(context.Background(), resource)
	assert.NilError(t, err)
	rendered, err := litestreamconfig.Render(input)
	assert.NilError(t, err)

	assert.Assert(t, input.Databases[0].Restore == nil)
	assert.Assert(t, strings.Contains(rendered.Data["restore-app.yml"], "${LS_APP_DEST_S3_SECRET_ACCESS_KEY}"), rendered.Data["restore-app.yml"])
	assert.Assert(t, !strings.Contains(rendered.Data["restore-app.yml"], "LS_APP_SRC_"), rendered.Data["restore-app.yml"])
	assert.DeepEqual(t, credentialPurposes(rendered.Credentials, "LS_APP_DEST_S3_SECRET_ACCESS_KEY"), []string{"replicate", "restore-app"})
}

func TestResolveReportsReplicaReferenceErrorsAtDirectFields(t *testing.T) {
	validDestination := replica("team-a", "destination", "destination-bucket")

	for _, tt := range []struct {
		name      string
		resource  *v1alpha1.Litestream
		objects   []client.Object
		wantError string
	}{
		{
			name: "missing source replica",
			resource: litestreamResource("team-a", v1alpha1.DatabaseBinding{
				Name: "app", Path: "/var/lib/app/app.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: localRef("missing")},
			}),
			wantError: `spec.databases[0].restore.replicaRef "missing"`,
		},
		{
			name: "missing destination replica",
			resource: litestreamResource("team-a", v1alpha1.DatabaseBinding{
				Name: "app", Path: "/var/lib/app/app.db", Replicate: replication("missing"),
			}),
			objects:   []client.Object{validDestination},
			wantError: `spec.databases[0].replicate.replicaRef "missing"`,
		},
		{
			name: "replica in another namespace",
			resource: litestreamResource("team-a", v1alpha1.DatabaseBinding{
				Name: "app", Path: "/var/lib/app/app.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: localRef("source")},
			}),
			objects:   []client.Object{replica("team-b", "source", "other-bucket")},
			wantError: `spec.databases[0].restore.replicaRef "source"`,
		},
		{
			name: "invalid source replica",
			resource: litestreamResource("team-a", v1alpha1.DatabaseBinding{
				Name: "app", Path: "/var/lib/app/app.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: localRef("source")},
			}),
			objects:   []client.Object{invalidReplica("team-a", "source")},
			wantError: `spec.databases[0].restore.replicaRef "source": invalid LitestreamReplica`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolver.Resolver{Reader: newReader(t, tt.objects...)}.Resolve(context.Background(), tt.resource)
			assert.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestResolveRejectsInvalidLitestreamSpecBeforeLookup(t *testing.T) {
	for _, tt := range []struct {
		name      string
		binding   v1alpha1.DatabaseBinding
		wantError string
	}{
		{
			name:      "no restore or replicate",
			binding:   v1alpha1.DatabaseBinding{Name: "app", Path: "/var/lib/app/app.db"},
			wantError: "spec.databases[0]",
		},
		{
			name: "clone policy with equal references",
			binding: v1alpha1.DatabaseBinding{
				Name: "app", Path: "/var/lib/app/app.db", ClonePolicy: v1alpha1.ClonePolicyRequireEmpty,
				Restore: &v1alpha1.RestoreSpec{ReplicaRef: localRef("shared")}, Replicate: replication("shared"),
			},
			wantError: "spec.databases[0].clonePolicy",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resource := litestreamResource("team-a", tt.binding)
			_, err := resolver.Resolver{Reader: newReader(t)}.Resolve(context.Background(), resource)
			assert.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestResolveDoesNotReadSecretValues(t *testing.T) {
	destination := replica("team-a", "destination", "destination-bucket")
	destination.Spec.Replica.S3.Credentials.SecretAccessKey = &v1alpha1.SecretReference{
		SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: localRef("credentials"), Key: "secret-access-key"},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "team-a"}, Data: map[string][]byte{"secret-access-key": []byte("must-not-read")}}
	resource := litestreamResource("team-a", v1alpha1.DatabaseBinding{
		Name: "app", Path: "/var/lib/app/app.db", Replicate: replication("destination"),
	})

	reader := rejectSecretReader{Reader: newReader(t, destination, secret)}
	got, err := resolver.Resolver{Reader: reader}.Resolve(context.Background(), resource)
	assert.NilError(t, err)
	assert.DeepEqual(t, got.Databases[0].Replicate.Replica.S3.Credentials.SecretAccessKey, destination.Spec.Replica.S3.Credentials.SecretAccessKey)
}

func litestreamResource(namespace string, databases ...v1alpha1.DatabaseBinding) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Name: "resource", Namespace: namespace},
		Spec:       v1alpha1.LitestreamSpec{Databases: databases},
	}
}

func localRef(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}

func replication(name string) *v1alpha1.ReplicateSpec {
	return &v1alpha1.ReplicateSpec{
		ReplicaRef: localRef(name), SyncInterval: metav1.Duration{Duration: time.Minute}, AutoRecover: true,
	}
}

func replica(namespace, name, bucket string) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{
			Type: v1alpha1.ReplicaTypeS3,
			S3:   &v1alpha1.S3ReplicaSpec{Bucket: bucket, Path: "backups/app"},
		}},
	}
}

func invalidReplica(namespace, name string) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3}},
	}
}

func newReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func credentialPurposes(bindings []litestreamconfig.CredentialBinding, envName string) []string {
	var purposes []string
	for _, binding := range bindings {
		if binding.EnvName == envName {
			purposes = append(purposes, binding.ContainerPurpose)
		}
	}
	return purposes
}

type rejectSecretReader struct {
	client.Reader
}

func (r rejectSecretReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		return fmt.Errorf("resolver must not read Secret %q", key.Name)
	}
	return r.Reader.Get(ctx, key, object, options...)
}
