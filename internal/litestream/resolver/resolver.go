// Package resolver resolves LitestreamReplica resources into the pure input
// consumed by rendering and injection.
package resolver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolver resolves all non-secret shared resource references for one
// Litestream resource. Reader is the only Kubernetes API boundary.
type Resolver struct {
	Reader client.Reader
}

type permanentError struct {
	err error
}

func (e *permanentError) Error() string {
	return e.err.Error()
}

func (e *permanentError) Unwrap() error {
	return e.err
}

// IsPermanentError reports whether err describes a missing or invalid
// dependency that will not be fixed by retrying the same API read.
func IsPermanentError(err error) bool {
	var target *permanentError
	return errors.As(err, &target)
}

func permanent(err error) error {
	return &permanentError{err: err}
}

// Resolve returns the renderer input for resource. All references are looked
// up in resource's namespace, even when an object with the same name exists in
// another namespace.
func (r Resolver) Resolve(ctx context.Context, resource *v1alpha1.Litestream) (litestreamconfig.Input, error) {
	if resource == nil {
		return litestreamconfig.Input{}, fmt.Errorf("resolve Litestream: resource is required")
	}
	if errs := resource.Spec.Validate(); len(errs) > 0 {
		return litestreamconfig.Input{}, permanent(fmt.Errorf("resolve Litestream %q: invalid spec: %w", resource.Name, errs.ToAggregate()))
	}
	if r.Reader == nil {
		return litestreamconfig.Input{}, fmt.Errorf("resolve Litestream %q: reader is required", resource.Name)
	}

	input := litestreamconfig.Input{
		Image:     resource.Spec.Image,
		Injection: resource.Spec.Injection,
		Databases: make([]litestreamconfig.Database, 0, len(resource.Spec.Databases)),
	}
	for i, binding := range resource.Spec.Databases {
		resolved := litestreamconfig.Database{
			Name:        binding.Name,
			Path:        binding.Path,
			ClonePolicy: binding.ClonePolicy,
		}

		if binding.Restore != nil {
			restoreField := fmt.Sprintf("spec.databases[%d].restore.replicaRef", i)
			source, err := r.replica(ctx, resource.Namespace, binding.Restore.ReplicaRef.Name, restoreField)
			if err != nil {
				return litestreamconfig.Input{}, err
			}
			resolved.Restore = restore(source.Spec.Replica, binding.Restore)
		}

		if binding.Replicate != nil {
			destination, err := r.destinationReplica(ctx, resource.Namespace, binding, i)
			if err != nil {
				return litestreamconfig.Input{}, err
			}
			resolved.Replicate = replicate(destination.Spec.Replica, binding.Replicate)
			if binding.Restore != nil && binding.Restore.ReplicaRef.Name != binding.Replicate.ReplicaRef.Name {
				resolved.Clone = true
			}
		}

		input.Databases = append(input.Databases, resolved)
	}
	return input, nil
}

func (r Resolver) destinationReplica(ctx context.Context, namespace string, binding v1alpha1.DatabaseBinding, index int) (*v1alpha1.LitestreamReplica, error) {
	field := fmt.Sprintf("spec.databases[%d].replicate.replicaRef", index)
	if binding.Replicate == nil {
		return nil, fmt.Errorf("%s: replicate is required", field)
	}
	return r.replica(ctx, namespace, binding.Replicate.ReplicaRef.Name, field)
}

func (r Resolver) replica(ctx context.Context, namespace, name, field string) (*v1alpha1.LitestreamReplica, error) {
	replica := &v1alpha1.LitestreamReplica{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, replica); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, permanent(fmt.Errorf("%s %q: LitestreamReplica not found in namespace %q", field, name, namespace))
		}
		return nil, fmt.Errorf("%s %q: get LitestreamReplica: %w", field, name, err)
	}
	if errs := replica.Spec.Validate(); len(errs) > 0 {
		return nil, permanent(fmt.Errorf("%s %q: invalid LitestreamReplica: %w", field, name, errs.ToAggregate()))
	}
	return replica, nil
}

func restore(replica v1alpha1.ReplicaSpec, policy *v1alpha1.RestoreSpec) *litestreamconfig.Restore {
	restore := &litestreamconfig.Restore{Replica: replica}
	if policy != nil {
		restore.IfDatabaseExists = policy.IfDatabaseExists
		restore.IfReplicaMissing = policy.IfReplicaMissing
		restore.IntegrityCheck = policy.IntegrityCheck
		restore.Timestamp = policy.Timestamp
		restore.TxID = policy.TxID
	}
	return restore
}

func replicate(replica v1alpha1.ReplicaSpec, spec *v1alpha1.ReplicateSpec) *litestreamconfig.Replicate {
	result := &litestreamconfig.Replicate{Replica: replica}
	if spec != nil {
		result.SyncInterval = spec.SyncInterval
		result.AutoRecover = spec.AutoRecover
	}
	return result
}
