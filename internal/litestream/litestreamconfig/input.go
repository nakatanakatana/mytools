package litestreamconfig

import (
	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Input is the fully resolved, non-secret configuration used to render one
// Litestream resource. Kubernetes resource lookup happens before rendering.
type Input struct {
	Image     v1alpha1.ImageSpec
	Injection v1alpha1.InjectionSpec
	Databases []Database
}

// Database is one resolved database binding.
type Database struct {
	Name string
	Path string
	// Clone is derived by the resolver when source and destination references
	// are both present and point to different Replica resources.
	Clone       bool
	ClonePolicy v1alpha1.ClonePolicy
	Restore     *Restore
	Replicate   *Replicate
}

// Restore is the source replica and policy used before an application starts.
type Restore struct {
	Replica          v1alpha1.ReplicaSpec
	IfDatabaseExists v1alpha1.IfDatabaseExistsPolicy
	IfReplicaMissing v1alpha1.IfReplicaMissingPolicy
	IntegrityCheck   v1alpha1.IntegrityCheck
	Timestamp        string
	TxID             string
}

// Replicate is the destination replica and continuous replication settings.
type Replicate struct {
	Replica      v1alpha1.ReplicaSpec
	SyncInterval metav1.Duration
	AutoRecover  bool
}
