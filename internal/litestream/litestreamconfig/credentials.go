// Package litestreamconfig renders a typed v1alpha1.Litestream resource into
// non-secret ConfigMap data (Litestream YAML and restore scripts) plus the
// stable Secret bindings the webhook must project into Pods. It never reads
// or emits Secret values.
package litestreamconfig

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// CredentialBinding describes one Secret value that must be projected into an
// injected container, either as an environment variable, a mounted file, or
// both (for tools that require a file path supplied via an environment
// variable, such as GOOGLE_APPLICATION_CREDENTIALS).
type CredentialBinding struct {
	ContainerPurpose string
	EnvName          string
	SecretKeyRef     corev1.SecretKeySelector
	FileMountPath    string
}

const (
	// SecretMountDir is where the webhook must project file-backed
	// credentials: CredentialBinding.FileMountPath values live under it.
	//
	// It is a sibling of ConfigMountDir rather than a subdirectory of it.
	// A volume mounted inside the ConfigMap mount would have to be created
	// under a read-only bind mount, which the container runtime can reject
	// with EROFS at container creation.
	SecretMountDir = "/etc/litestream-secrets"

	credentialRoleSource      = "SRC"
	credentialRoleDestination = "DEST"

	// ReplicateContainerPurpose marks the bindings the replication sidecar
	// needs.
	ReplicateContainerPurpose = "replicate"

	restoreContainerPurposePrefix = "restore-"
)

// RestoreContainerPurpose marks the bindings databaseName's restore
// container needs.
func RestoreContainerPurpose(databaseName string) string {
	return restoreContainerPurposePrefix + databaseName
}

// credentialBuilder accumulates CredentialBinding values while rendering one
// backend-specific replica block for one database and one role (source or
// destination).
type credentialBuilder struct {
	databaseName string
	role         string
	backend      string
	purpose      string
	bindings     []CredentialBinding
}

func newCredentialBuilder(databaseName, role, backend, purpose string) *credentialBuilder {
	return &credentialBuilder{databaseName: databaseName, role: role, backend: backend, purpose: purpose}
}

// env renders an environment-variable-backed credential. It returns the
// "${ENV_NAME}" placeholder to embed in generated YAML, or "" when ref is nil
// (the field is left unset so Litestream falls back to Workload Identity or
// similar ambient credentials).
func (b *credentialBuilder) env(field string, ref *v1alpha1.SecretReference) string {
	if ref == nil {
		return ""
	}
	name := b.envName(field)
	b.bindings = append(b.bindings, CredentialBinding{
		ContainerPurpose: b.purpose,
		EnvName:          name,
		SecretKeyRef:     ref.SecretKeyRef,
	})
	return "${" + name + "}"
}

// file renders a file-backed credential (e.g. a GCS service account JSON key
// or an SFTP private key). It returns the fixed mount path to embed in
// generated YAML, or "" when ref is nil.
func (b *credentialBuilder) file(filename string, ref *v1alpha1.SecretReference) string {
	if ref == nil {
		return ""
	}
	path := b.fileMountPath(filename)
	b.bindings = append(b.bindings, CredentialBinding{
		ContainerPurpose: b.purpose,
		SecretKeyRef:     ref.SecretKeyRef,
		FileMountPath:    path,
	})
	return path
}

// fileWithEnv renders a file-backed credential that a tool additionally
// expects to discover through an environment variable holding the mount
// path (for example GOOGLE_APPLICATION_CREDENTIALS). It returns the mount
// path, or "" when ref is nil.
func (b *credentialBuilder) fileWithEnv(envVar, filename string, ref *v1alpha1.SecretReference) string {
	if ref == nil {
		return ""
	}
	path := b.fileMountPath(filename)
	b.bindings = append(b.bindings, CredentialBinding{
		ContainerPurpose: b.purpose,
		EnvName:          envVar,
		SecretKeyRef:     ref.SecretKeyRef,
		FileMountPath:    path,
	})
	return path
}

// fileList renders an ordered list of file-backed credentials sharing a
// filename prefix (for example NATS root CA certificates).
func (b *credentialBuilder) fileList(filenamePrefix string, refs []v1alpha1.SecretReference) []string {
	if len(refs) == 0 {
		return nil
	}
	paths := make([]string, 0, len(refs))
	for i := range refs {
		filename := fmt.Sprintf("%s-%d", filenamePrefix, i)
		paths = append(paths, b.file(filename, &refs[i]))
	}
	return paths
}

// rebind returns a copy of the accumulated bindings retargeted to a
// different ContainerPurpose. It is used when the same credential must be
// projected into more than one injected container, for example a replicate
// destination that is also used as the implicit restore source.
func (b *credentialBuilder) rebind(purpose string) []CredentialBinding {
	rebound := make([]CredentialBinding, len(b.bindings))
	for i, binding := range b.bindings {
		binding.ContainerPurpose = purpose
		rebound[i] = binding
	}
	return rebound
}

func (b *credentialBuilder) envName(field string) string {
	return envName(b.databaseName, b.role, b.backend, field)
}

func (b *credentialBuilder) fileMountPath(filename string) string {
	return fileMountPath(b.databaseName, b.role, b.backend, filename)
}

// envName builds a stable, collision-resistant environment variable name
// from the database name, credential role (source/destination), backend
// type, and field purpose. For example: LS_APP_DEST_S3_SECRET_ACCESS_KEY.
func envName(databaseName, role, backend, field string) string {
	return strings.Join([]string{"LS", sanitizeIdentifier(databaseName), role, backend, field}, "_")
}

// fileMountPath builds a stable mount path for file-backed credentials,
// namespaced by database, role, and backend so that concurrent databases and
// roles never collide.
func fileMountPath(databaseName, role, backend, filename string) string {
	dir := strings.ToLower(sanitizeIdentifier(databaseName)) + "-" + strings.ToLower(role)
	return SecretMountDir + "/" + dir + "/" + strings.ToLower(backend) + "-" + filename
}

func sanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// sortCredentialBindings orders bindings deterministically by container
// purpose, environment variable name, file mount path, and Secret selector,
// so that Render output (and the revision hash derived from it) is stable
// across runs.
func sortCredentialBindings(bindings []CredentialBinding) {
	sort.Slice(bindings, func(i, j int) bool {
		a, b := bindings[i], bindings[j]
		if a.ContainerPurpose != b.ContainerPurpose {
			return a.ContainerPurpose < b.ContainerPurpose
		}
		if a.EnvName != b.EnvName {
			return a.EnvName < b.EnvName
		}
		if a.FileMountPath != b.FileMountPath {
			return a.FileMountPath < b.FileMountPath
		}
		if a.SecretKeyRef.Name != b.SecretKeyRef.Name {
			return a.SecretKeyRef.Name < b.SecretKeyRef.Name
		}
		if a.SecretKeyRef.Key != b.SecretKeyRef.Key {
			return a.SecretKeyRef.Key < b.SecretKeyRef.Key
		}
		return optionalSortValue(a.SecretKeyRef.Optional) < optionalSortValue(b.SecretKeyRef.Optional)
	})
}

func optionalSortValue(optional *bool) int {
	if optional == nil {
		return 0
	}
	if !*optional {
		return 1
	}
	return 2
}
