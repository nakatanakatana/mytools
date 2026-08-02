package litestreamconfig

import (
	"fmt"
	"path/filepath"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
)

// ValidateInput checks invariants that depend on the fully resolved database
// and replica objects. It performs no Kubernetes API lookups.
func ValidateInput(input Input) error {
	names := make(map[string]struct{}, len(input.Databases))
	paths := make(map[string]struct{}, len(input.Databases))

	for _, database := range input.Databases {
		if _, duplicate := names[database.Name]; duplicate {
			return inputError(database.Name, "name", "must be unique among resolved databases")
		}
		names[database.Name] = struct{}{}

		path := filepath.Clean(database.Path)
		if _, duplicate := paths[path]; duplicate {
			return inputError(database.Name, "path", "must be unique among resolved databases")
		}
		paths[path] = struct{}{}

		if input.Injection.Permissions.DirectoryMode != "" && filepath.Dir(path) == "/" {
			return inputError(database.Name, "injection.permissions.directoryMode", "cannot be set when the database path is directly under /")
		}
		if database.Restore == nil && database.Replicate == nil {
			return inputError(database.Name, "database", "must have a restore source or replication destination")
		}
		if database.Clone && (database.Restore == nil || database.Replicate == nil) {
			return inputError(database.Name, "database", "clone requires both a restore source and replication destination")
		}
		if database.ClonePolicy != "" && !database.Clone {
			return inputError(database.Name, "clonePolicy", "requires distinct restore source and replication destination")
		}
	}

	return validateGCSCredentials(input.Databases)
}

func validateGCSCredentials(databases []Database) error {
	var firstCredential string
	haveFirstCredential := false

	for _, database := range databases {
		if database.Clone &&
			isGCSRestore(database.Restore) &&
			isGCSReplica(database.Replicate) &&
			gcsCredentialIdentity(database.Restore.Replica.GCS) != gcsCredentialIdentity(database.Replicate.Replica.GCS) {
			return inputError(database.Name, "replicate.replica.gcs.serviceAccountJSON", "clone source and destination GCS replicas must use the same serviceAccountJSON SecretKeyRef or both use ambient credentials")
		}

		if !isGCSReplica(database.Replicate) {
			continue
		}

		credential := gcsCredentialIdentity(database.Replicate.Replica.GCS)
		if !haveFirstCredential {
			firstCredential = credential
			haveFirstCredential = true
			continue
		}
		if credential != firstCredential {
			return inputError(database.Name, "replicate.replica.gcs.serviceAccountJSON", "all GCS replication databases must use the same serviceAccountJSON SecretKeyRef or all use ambient credentials")
		}
	}
	return nil
}

func isGCSReplica(replica *Replicate) bool {
	return replica != nil && replica.Replica.Type == v1alpha1.ReplicaTypeGCS && replica.Replica.GCS != nil
}

func isGCSRestore(replica *Restore) bool {
	return replica != nil && replica.Replica.Type == v1alpha1.ReplicaTypeGCS && replica.Replica.GCS != nil
}

func gcsCredentialIdentity(spec *v1alpha1.GCSReplicaSpec) string {
	if spec == nil || spec.ServiceAccountJSON == nil {
		return "ambient"
	}
	return spec.ServiceAccountJSON.SecretKeyRef.Name + "\x00" + spec.ServiceAccountJSON.SecretKeyRef.Key
}

func inputError(databaseName, field, message string) error {
	return fmt.Errorf("litestreamconfig: database %q field %q: %s", databaseName, field, message)
}
