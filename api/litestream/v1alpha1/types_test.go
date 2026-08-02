package v1alpha1_test

import (
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"gotest.tools/v3/assert"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestAddToSchemeRegistersLitestreamResources(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	for _, test := range []struct {
		object runtime.Object
		kind   string
	}{
		{&v1alpha1.Litestream{}, "Litestream"},
		{&v1alpha1.LitestreamReplica{}, "LitestreamReplica"},
	} {
		gvks, _, err := scheme.ObjectKinds(test.object)
		assert.NilError(t, err)
		assert.DeepEqual(t, gvks, []schema.GroupVersionKind{{Group: "litestream.mytools.nakatanakatana.app", Version: "v1alpha1", Kind: test.kind}})
	}
}
