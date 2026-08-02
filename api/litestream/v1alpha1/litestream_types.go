package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClonePolicy determines how a clone treats existing destination replica data.
type ClonePolicy string

const (
	ClonePolicyResumeOrCreate ClonePolicy = "resume-or-create"
	ClonePolicyRequireEmpty   ClonePolicy = "require-empty"
)

// IfDatabaseExistsPolicy determines how restore handles an existing database.
type IfDatabaseExistsPolicy string

const (
	IfDatabaseExistsSkip      IfDatabaseExistsPolicy = "skip"
	IfDatabaseExistsFail      IfDatabaseExistsPolicy = "fail"
	IfDatabaseExistsOverwrite IfDatabaseExistsPolicy = "overwrite"
)

// IfReplicaMissingPolicy determines how restore handles a missing replica.
type IfReplicaMissingPolicy string

const (
	IfReplicaMissingSkip IfReplicaMissingPolicy = "skip"
	IfReplicaMissingFail IfReplicaMissingPolicy = "fail"
)

// IntegrityCheck determines the level of verification performed during restore.
type IntegrityCheck string

const (
	IntegrityCheckNone  IntegrityCheck = "none"
	IntegrityCheckQuick IntegrityCheck = "quick"
	IntegrityCheckFull  IntegrityCheck = "full"
)

// LitestreamSpec defines the desired state of a Litestream resource.
type LitestreamSpec struct {
	Image     ImageSpec     `json:"image,omitempty"`
	Injection InjectionSpec `json:"injection,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Databases []DatabaseBinding `json:"databases"`
}

// ImageSpec overrides the controller's default Litestream image.
// +kubebuilder:validation:XValidation:rule="((!has(self.repository) || size(self.repository) == 0) && (!has(self.tag) || size(self.tag) == 0) && (!has(self.digest) || size(self.digest) == 0)) || (has(self.digest) && size(self.digest) > 0 && (!has(self.repository) || size(self.repository) == 0 || (!self.repository.matches('(^|/)[^/]*:[^/]*$') && !self.repository.contains('@'))))",message="image overrides must use an immutable digest and an untagged repository"
type ImageSpec struct {
	Repository string `json:"repository,omitempty"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`
	Tag string `json:"tag,omitempty"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest,omitempty"`

	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// InjectionSpec configures the Pod fragments injected for this resource.
type InjectionSpec struct {
	TargetContainer string `json:"targetContainer,omitempty"`
	Volume          string `json:"volume,omitempty"`

	ExtraVolumeMounts []corev1.VolumeMount        `json:"extraVolumeMounts,omitempty"`
	Resources         corev1.ResourceRequirements `json:"resources,omitempty"`

	// PodSecurityContext supplies the fsGroup that lets the application and
	// Litestream share the database. Only fsGroup and fsGroupChangePolicy
	// are honored, and fsGroupChangePolicy requires fsGroup; the remaining
	// fields would apply to the whole Pod, so setting any of them is
	// rejected at admission.
	PodSecurityContext       *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`
	ContainerSecurityContext *corev1.SecurityContext    `json:"containerSecurityContext,omitempty"`
	Permissions              PermissionsSpec            `json:"permissions,omitempty"`
}

// PermissionsSpec optionally configures group ownership and permissions after
// restore. Empty modes preserve the existing permissions.
type PermissionsSpec struct {
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	DirectoryMode string `json:"directoryMode,omitempty"`

	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	DatabaseMode string `json:"databaseMode,omitempty"`
}

// DatabaseBinding configures how a Litestream resource uses an inline local SQLite path.
// The operation is inferred from this binding's optional restore.replicaRef source
// and optional replicate.replicaRef destination.
type DatabaseBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=44
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// +kubebuilder:validation:Enum=resume-or-create;require-empty
	ClonePolicy ClonePolicy `json:"clonePolicy,omitempty"`

	Restore   *RestoreSpec   `json:"restore,omitempty"`
	Replicate *ReplicateSpec `json:"replicate,omitempty"`
}

// RestoreSpec describes the replica source and restore behavior before the Pod starts.
type RestoreSpec struct {
	ReplicaRef corev1.LocalObjectReference `json:"replicaRef"`

	// +kubebuilder:validation:Enum=skip;fail;overwrite
	// +kubebuilder:default:=skip
	IfDatabaseExists IfDatabaseExistsPolicy `json:"ifDatabaseExists,omitempty"`

	// +kubebuilder:validation:Enum=skip;fail
	// +kubebuilder:default:=skip
	IfReplicaMissing IfReplicaMissingPolicy `json:"ifReplicaMissing,omitempty"`

	// +kubebuilder:validation:Enum=none;quick;full
	// +kubebuilder:default:=quick
	IntegrityCheck IntegrityCheck `json:"integrityCheck,omitempty"`

	Timestamp string `json:"timestamp,omitempty"`
	TxID      string `json:"txid,omitempty"`
}

// ReplicateSpec describes continuous replication to a replica.
type ReplicateSpec struct {
	ReplicaRef corev1.LocalObjectReference `json:"replicaRef"`

	SyncInterval metav1.Duration `json:"syncInterval,omitempty"`
	AutoRecover  bool            `json:"autoRecover,omitempty"`
}

// LitestreamStatus defines the observed state of a Litestream resource.
type LitestreamStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	ConfigMapName      string             `json:"configMapName,omitempty"`
	ConfigHash         string             `json:"configHash,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="ConfigMap",type="string",JSONPath=".status.configMapName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Litestream is the Schema for Litestream configuration resources.
type Litestream struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LitestreamSpec   `json:"spec,omitempty"`
	Status LitestreamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LitestreamList contains a list of Litestream resources.
type LitestreamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Litestream `json:"items"`
}
