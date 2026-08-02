// Package v1alpha1 contains API Schema definitions for the Litestream v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=litestream.mytools.nakatanakatana.app
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const Group = "litestream.mytools.nakatanakatana.app"

var (
	// GroupVersion identifies the API group and version for Litestream resources.
	GroupVersion = schema.GroupVersion{Group: Group, Version: "v1alpha1"}

	// SchemeBuilder registers Litestream types with a runtime scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds all Litestream API types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&Litestream{}, &LitestreamList{},
		&LitestreamReplica{}, &LitestreamReplicaList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
