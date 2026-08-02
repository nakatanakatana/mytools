package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LitestreamReplicaSpec defines one remote Litestream replica endpoint.
type LitestreamReplicaSpec struct {
	Replica ReplicaSpec `json:"replica"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.replica.type"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LitestreamReplica is the schema for a shared remote Litestream replica endpoint.
type LitestreamReplica struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec LitestreamReplicaSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// LitestreamReplicaList contains a list of LitestreamReplica resources.
type LitestreamReplicaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LitestreamReplica `json:"items"`
}
