package v1alpha1_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestLitestreamSpecValidate(t *testing.T) {
	for _, tt := range []struct {
		name string
		spec v1alpha1.LitestreamSpec
		want string
	}{
		{"no databases", v1alpha1.LitestreamSpec{}, "spec.databases"},
		{"duplicate binding name", bindings(binding("app"), binding("app")), "spec.databases[1].name"},
		{"duplicate database path", bindings(binding("app"), v1alpha1.DatabaseBinding{Name: "other", Path: "/data/app.db", Replicate: validReplicate()}), "spec.databases[1].path"},
		{"invalid binding name", bindings(binding(strings.Repeat("a", 45))), "spec.databases[0].name"},
		{"relative path", bindings(v1alpha1.DatabaseBinding{Path: "data/app.db"}), "spec.databases[0].path"},
		{"unclean path", bindings(v1alpha1.DatabaseBinding{Path: "/data/../app.db"}), "spec.databases[0].path"},
		{"restore reference required", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Restore: &v1alpha1.RestoreSpec{}}), "spec.databases[0].restore.replicaRef.name"},
		{"no operation", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db"}), "spec.databases[0]"},
		{"clone policy requires source", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", ClonePolicy: v1alpha1.ClonePolicyRequireEmpty, Replicate: validReplicate()}), "spec.databases[0].restore"},
		{"empty replication reference", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Replicate: &v1alpha1.ReplicateSpec{}}), "spec.databases[0].replicate.replicaRef.name"},
		{"invalid replication reference", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("Bad_Name")}}), "spec.databases[0].replicate.replicaRef.name"},
		{"timestamp and txid", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("replica"), Timestamp: "2026-01-01T00:00:00Z", TxID: "01"}}), "spec.databases[0].restore"},
		{"invalid timestamp", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("replica"), Timestamp: "invalid"}}), "spec.databases[0].restore.timestamp"},
		{"invalid txid", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("replica"), TxID: "not-hex"}}), "spec.databases[0].restore.txid"},
		{"negative sync interval", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("replica"), SyncInterval: metav1.Duration{Duration: -time.Second}}}), "spec.databases[0].replicate.syncInterval"},
		{"equal source and destination rejects clone policy", bindings(v1alpha1.DatabaseBinding{Path: "/data/app.db", ClonePolicy: v1alpha1.ClonePolicyRequireEmpty, Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("replica")}, Replicate: validReplicate()}), "spec.databases[0].clonePolicy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if errors := tt.spec.Validate(); !hasField(errors, tt.want) {
				t.Fatalf("Validate() errors = %v, want error for %q", errors, tt.want)
			}
		})
	}

}

func TestLitestreamBindingValidateAcceptsOperations(t *testing.T) {
	for _, tt := range []struct {
		name    string
		binding v1alpha1.DatabaseBinding
	}{
		{"restore only", v1alpha1.DatabaseBinding{Name: "restore", Path: "/data/restore.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("source")}}},
		{"destination only replicate", v1alpha1.DatabaseBinding{Name: "replicate", Path: "/data/replicate.db", Replicate: validReplicate()}},
		{"equal source and destination", v1alpha1.DatabaseBinding{Name: "equal", Path: "/data/equal.db", Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("replica")}, Replicate: validReplicate()}},
		{"distinct source and destination clone", v1alpha1.DatabaseBinding{Name: "clone", Path: "/data/clone.db", ClonePolicy: v1alpha1.ClonePolicyRequireEmpty, Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("source")}, Replicate: validReplicate()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			spec := bindings(tt.binding)
			if errors := spec.Validate(); len(errors) != 0 {
				t.Fatalf("Validate() errors = %v, want no errors", errors)
			}
		})
	}
}

func TestLitestreamReplicaSpecValidateDelegatesToReplica(t *testing.T) {
	spec := v1alpha1.LitestreamReplicaSpec{Replica: v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3}}
	if errors := spec.Validate(); !hasField(errors, "spec.replica") {
		t.Fatalf("Validate() errors = %v, want error for spec.replica", errors)
	}
}

func TestLitestreamReplicaSpecValidateBackends(t *testing.T) {
	for _, tt := range []struct {
		name    string
		replica v1alpha1.ReplicaSpec
		want    string
	}{
		{"multiple backends", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3, S3: &v1alpha1.S3ReplicaSpec{}, GCS: &v1alpha1.GCSReplicaSpec{}}, "spec.replica"},
		{"mismatched backend", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3, GCS: &v1alpha1.GCSReplicaSpec{}}, "spec.replica"},
		{"s3 bucket", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3, S3: &v1alpha1.S3ReplicaSpec{Path: "path"}}, "spec.replica.s3.bucket"},
		{"s3 whitespace bucket", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3, S3: &v1alpha1.S3ReplicaSpec{Bucket: "   ", Path: "path"}}, "spec.replica.s3.bucket"},
		{"gcs path", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeGCS, GCS: &v1alpha1.GCSReplicaSpec{Bucket: "bucket"}}, "spec.replica.gcs.path"},
		{"azure container", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeAzure, Azure: &v1alpha1.AzureReplicaSpec{AccountName: "account", Path: "path"}}, "spec.replica.azure.container"},
		{"file path", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeFile, File: &v1alpha1.FileReplicaSpec{}}, "spec.replica.file.path"},
		{"nats url", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeNATS, NATS: &v1alpha1.NATSReplicaSpec{}}, "spec.replica.nats.url"},
		{"oss path", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeOSS, OSS: &v1alpha1.OSSReplicaSpec{Bucket: "bucket"}}, "spec.replica.oss.path"},
		{"sftp user", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeSFTP, SFTP: &v1alpha1.SFTPReplicaSpec{Host: "host", Path: "path"}}, "spec.replica.sftp.user"},
		{"webdav url", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeWebDAV, WebDAV: &v1alpha1.WebDAVReplicaSpec{}}, "spec.replica.webdav.url"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if errors := validateReplicaResource(tt.replica); !hasField(errors, tt.want) {
				t.Fatalf("Validate() errors = %v, want error for %q", errors, tt.want)
			}
		})
	}

	for _, tt := range []struct {
		name    string
		replica v1alpha1.ReplicaSpec
	}{
		{"s3", s3Replica()},
		{"gcs", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeGCS, GCS: &v1alpha1.GCSReplicaSpec{Bucket: "bucket", Path: "path"}}},
		{"azure", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeAzure, Azure: &v1alpha1.AzureReplicaSpec{AccountName: "account", Container: "container", Path: "path"}}},
		{"file", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeFile, File: &v1alpha1.FileReplicaSpec{Path: "/backups/app"}}},
		{"nats", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeNATS, NATS: &v1alpha1.NATSReplicaSpec{URL: "nats://nats.example.com"}}},
		{"oss", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeOSS, OSS: &v1alpha1.OSSReplicaSpec{Bucket: "bucket", Path: "path"}}},
		{"sftp", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeSFTP, SFTP: &v1alpha1.SFTPReplicaSpec{Host: "sftp.example.com", User: "backup", Path: "/backups/app"}}},
		{"webdav", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeWebDAV, WebDAV: &v1alpha1.WebDAVReplicaSpec{URL: "https://webdav.example.com"}}},
	} {
		t.Run("valid "+tt.name, func(t *testing.T) {
			if errors := validateReplicaResource(tt.replica); len(errors) != 0 {
				t.Fatalf("Validate() errors = %v, want no errors", errors)
			}
		})
	}
}

func TestLitestreamReplicaSpecValidateSecretReferencesAndNATS(t *testing.T) {
	secret := func(name, key string) *v1alpha1.SecretReference {
		return &v1alpha1.SecretReference{SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: objectRef(name), Key: key}}
	}
	for _, tt := range []struct {
		name    string
		replica v1alpha1.ReplicaSpec
		want    string
	}{
		{"empty secret name", s3WithAccessKey(secret("", "access-key")), "spec.replica.s3.credentials.accessKeyID.secretKeyRef.name"},
		{"invalid secret name", s3WithAccessKey(secret("Bad.Secret_Name", "access-key")), "spec.replica.s3.credentials.accessKeyID.secretKeyRef.name"},
		{"empty secret key", s3WithAccessKey(secret("credentials", "")), "spec.replica.s3.credentials.accessKeyID.secretKeyRef.key"},
		{"invalid secret key", s3WithAccessKey(secret("credentials", "access key")), "spec.replica.s3.credentials.accessKeyID.secretKeyRef.key"},
		{"optional gcs file", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeGCS, GCS: &v1alpha1.GCSReplicaSpec{Bucket: "bucket", Path: "path", ServiceAccountJSON: optionalSecret(secret("credentials", "value"))}}, "spec.replica.gcs.serviceAccountJSON.secretKeyRef.optional"},
		{"optional nats credentials file", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeNATS, NATS: &v1alpha1.NATSReplicaSpec{URL: "nats://nats.example.com", Creds: optionalSecret(secret("credentials", "value"))}}, "spec.replica.nats.creds.secretKeyRef.optional"},
		{"optional nats root ca", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeNATS, NATS: &v1alpha1.NATSReplicaSpec{URL: "nats://nats.example.com", RootCAs: []v1alpha1.SecretReference{*optionalSecret(secret("credentials", "value"))}}}, "spec.replica.nats.rootCAs[0].secretKeyRef.optional"},
		{"optional nats environment", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeNATS, NATS: &v1alpha1.NATSReplicaSpec{URL: "nats://nats.example.com", Token: optionalSecret(secret("credentials", "value"))}}, "spec.replica.nats.token.secretKeyRef.optional"},
		{"optional sftp file", v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeSFTP, SFTP: &v1alpha1.SFTPReplicaSpec{Host: "host", User: "user", Path: "path", PrivateKey: optionalSecret(secret("credentials", "value"))}}, "spec.replica.sftp.privateKey.secretKeyRef.optional"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if errors := validateReplicaResource(tt.replica); !hasField(errors, tt.want) {
				t.Fatalf("Validate() errors = %v, want error for %q", errors, tt.want)
			}
		})
	}

	for _, tt := range []struct {
		name string
		set  func(*v1alpha1.NATSReplicaSpec)
		want string
	}{
		{"username without password", func(spec *v1alpha1.NATSReplicaSpec) { spec.Username = secret("credentials", "username") }, "spec.replica.nats.password"},
		{"password without username", func(spec *v1alpha1.NATSReplicaSpec) { spec.Password = secret("credentials", "password") }, "spec.replica.nats.username"},
		{"jwt without seed", func(spec *v1alpha1.NATSReplicaSpec) { spec.JWT = secret("credentials", "jwt") }, "spec.replica.nats.seed"},
		{"seed without jwt", func(spec *v1alpha1.NATSReplicaSpec) { spec.Seed = secret("credentials", "seed") }, "spec.replica.nats.jwt"},
		{"client certificate without key", func(spec *v1alpha1.NATSReplicaSpec) { spec.ClientCert = secret("credentials", "cert") }, "spec.replica.nats.clientKey"},
		{"client key without certificate", func(spec *v1alpha1.NATSReplicaSpec) { spec.ClientKey = secret("credentials", "key") }, "spec.replica.nats.clientCert"},
		{"credentials with username authentication", func(spec *v1alpha1.NATSReplicaSpec) {
			spec.Username = secret("credentials", "username")
			spec.Password = secret("credentials", "password")
			spec.Creds = secret("credentials", "creds")
		}, "spec.replica.nats.creds"},
		{"nkey with token authentication", func(spec *v1alpha1.NATSReplicaSpec) {
			spec.NKey = secret("credentials", "nkey")
			spec.Token = secret("credentials", "token")
		}, "spec.replica.nats.token"},
		{"negative max reconnects", func(spec *v1alpha1.NATSReplicaSpec) { value := -2; spec.MaxReconnects = &value }, "spec.replica.nats.maxReconnects"},
		{"negative reconnect wait", func(spec *v1alpha1.NATSReplicaSpec) { spec.ReconnectWait = &metav1.Duration{Duration: -time.Second} }, "spec.replica.nats.reconnectWait"},
		{"negative timeout", func(spec *v1alpha1.NATSReplicaSpec) { spec.Timeout = &metav1.Duration{Duration: -time.Second} }, "spec.replica.nats.timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			nats := &v1alpha1.NATSReplicaSpec{URL: "nats://nats.example.com"}
			tt.set(nats)
			if errors := validateReplicaResource(v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeNATS, NATS: nats}); !hasField(errors, tt.want) {
				t.Fatalf("Validate() errors = %v, want error for %q", errors, tt.want)
			}
		})
	}

	valid := s3WithAccessKey(secret("external-secret-not-created", "access-key"))
	if errors := validateReplicaResource(valid); len(errors) != 0 {
		t.Fatalf("Validate() errors = %v, want no errors for a syntactically valid Secret reference", errors)
	}
}

func TestLitestreamSpecValidateImageAndPermissions(t *testing.T) {
	for _, tt := range []struct {
		name string
		spec v1alpha1.LitestreamSpec
		want string
	}{
		{"invalid directory mode", withPermissions("755", ""), "spec.injection.permissions.directoryMode"},
		{"invalid database mode", withPermissions("", "0648"), "spec.injection.permissions.databaseMode"},
		{"mutable image", withImage(v1alpha1.ImageSpec{Tag: "0.5.15"}), "spec.image"},
		{"invalid image digest", withImage(v1alpha1.ImageSpec{Digest: "sha256:not-a-digest"}), "spec.image.digest"},
		{"tagged repository", withImage(v1alpha1.ImageSpec{Repository: "registry.example.com/litestream:latest", Digest: validImageDigest}), "spec.image.repository"},
		{"malformed repository", withImage(v1alpha1.ImageSpec{Repository: "registry.example.com//litestream", Digest: validImageDigest}), "spec.image.repository"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if errors := tt.spec.Validate(); !hasField(errors, tt.want) {
				t.Fatalf("Validate() errors = %v, want error for %q", errors, tt.want)
			}
		})
	}
	for _, image := range []v1alpha1.ImageSpec{
		{Repository: "registry.example.com:5000/litestream", Digest: validImageDigest},
		{Repository: "registry.example.com/litestream", Tag: "latest", Digest: validImageDigest},
	} {
		if errors := validateLitestream(withImage(image)); len(errors) != 0 {
			t.Fatalf("Validate() errors = %v, want no errors", errors)
		}
	}
	clone := withClonePolicy(withRestore(binding("app")))
	clone.Restore.ReplicaRef = objectRef("source")
	if errors := validateLitestream(bindings(clone)); len(errors) != 0 {
		t.Fatalf("Validate() errors = %v, want no errors for clone policy with a destination", errors)
	}
}

func objectRef(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}
func replicaResource(replica v1alpha1.ReplicaSpec) v1alpha1.LitestreamReplicaSpec {
	return v1alpha1.LitestreamReplicaSpec{Replica: replica}
}
func validateReplicaResource(replica v1alpha1.ReplicaSpec) field.ErrorList {
	resource := replicaResource(replica)
	return resource.Validate()
}
func validateLitestream(spec v1alpha1.LitestreamSpec) field.ErrorList {
	return spec.Validate()
}
func s3Replica() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{Type: v1alpha1.ReplicaTypeS3, S3: &v1alpha1.S3ReplicaSpec{Bucket: "bucket", Path: "path"}}
}
func s3WithAccessKey(accessKey *v1alpha1.SecretReference) v1alpha1.ReplicaSpec {
	replica := s3Replica()
	replica.S3.Credentials.AccessKeyID = accessKey
	return replica
}
func optionalSecret(secret *v1alpha1.SecretReference) *v1alpha1.SecretReference {
	optional := true
	secret.SecretKeyRef.Optional = &optional
	return secret
}
func bindings(databases ...v1alpha1.DatabaseBinding) v1alpha1.LitestreamSpec {
	return v1alpha1.LitestreamSpec{Databases: databases}
}
func binding(name string) v1alpha1.DatabaseBinding {
	return v1alpha1.DatabaseBinding{Name: name, Path: "/data/" + name + ".db", Replicate: validReplicate()}
}
func withRestore(binding v1alpha1.DatabaseBinding) v1alpha1.DatabaseBinding {
	binding.Restore = &v1alpha1.RestoreSpec{ReplicaRef: objectRef("replica")}
	return binding
}
func withClonePolicy(binding v1alpha1.DatabaseBinding) v1alpha1.DatabaseBinding {
	binding.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	return binding
}
func validReplicate() *v1alpha1.ReplicateSpec {
	return &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("replica")}
}
func withPermissions(directoryMode, databaseMode string) v1alpha1.LitestreamSpec {
	spec := bindings(binding("app"))
	spec.Injection.Permissions.DirectoryMode = directoryMode
	spec.Injection.Permissions.DatabaseMode = databaseMode
	return spec
}
func withImage(image v1alpha1.ImageSpec) v1alpha1.LitestreamSpec {
	spec := bindings(binding("app"))
	spec.Image = image
	return spec
}
func hasField(errors field.ErrorList, want string) bool {
	for _, err := range errors {
		if err.Field == want {
			return true
		}
	}
	return false
}

const validImageDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
