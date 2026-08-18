package webhook

import "github.com/nakatanakatana/mytools/api/litestream/v1alpha1"

// replicationDestination identifies one remote database destination within a
// namespace. The Replica reference selects the backend root and the database
// path selects the database within that root.
type replicationDestination struct {
	replicaRef string
	path       string
}

func replicationDestinations(resource *v1alpha1.Litestream) []replicationDestination {
	var destinations []replicationDestination
	for _, database := range resource.Spec.Databases {
		if database.Replicate == nil {
			continue
		}
		destinations = append(destinations, replicationDestination{
			replicaRef: database.Replicate.ReplicaRef.Name,
			path:       database.Path,
		})
	}
	return destinations
}

func shareReplicationDestination(first, second *v1alpha1.Litestream) bool {
	seen := make(map[replicationDestination]struct{}, len(replicationDestinations(first)))
	for _, destination := range replicationDestinations(first) {
		seen[destination] = struct{}{}
	}
	for _, destination := range replicationDestinations(second) {
		if _, ok := seen[destination]; ok {
			return true
		}
	}
	return false
}
