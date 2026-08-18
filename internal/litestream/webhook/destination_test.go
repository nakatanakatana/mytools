package webhook

import (
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShareReplicationDestination(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  *v1alpha1.Litestream
		second *v1alpha1.Litestream
		want   bool
	}{
		{
			name:   "same Replica and database path",
			first:  litestreamWithDestination("replica", "/data/app.db"),
			second: litestreamWithDestination("replica", "/data/app.db"),
			want:   true,
		},
		{
			name:   "different database paths on the same Replica",
			first:  litestreamWithDestination("replica", "/data/app.db"),
			second: litestreamWithDestination("replica", "/data/other.db"),
			want:   false,
		},
		{
			name:   "different Replicas with the same database path",
			first:  litestreamWithDestination("replica-a", "/data/app.db"),
			second: litestreamWithDestination("replica-b", "/data/app.db"),
			want:   false,
		},
		{
			name: "one of multiple database bindings overlaps",
			first: litestreamWithDestinations(
				v1alpha1.DatabaseBinding{
					Name:      "app",
					Path:      "/data/app.db",
					Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("app-replica")},
				},
				v1alpha1.DatabaseBinding{
					Name:      "other",
					Path:      "/data/other.db",
					Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("other-replica")},
				},
			),
			second: litestreamWithDestination("other-replica", "/data/other.db"),
			want:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, shareReplicationDestination(test.first, test.second), test.want)
		})
	}
}

func litestreamWithDestination(replica, path string) *v1alpha1.Litestream {
	return litestreamWithDestinations(v1alpha1.DatabaseBinding{
		Name:      "app",
		Path:      path,
		Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef(replica)},
	})
}

func litestreamWithDestinations(databases ...v1alpha1.DatabaseBinding) *v1alpha1.Litestream {
	return &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace},
		Spec:       v1alpha1.LitestreamSpec{Databases: databases},
	}
}
