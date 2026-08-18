package webhook

import (
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
)

// RegisterHandlers registers the Litestream admission handlers on server.
// Keeping registration here lets production and integration environments use
// the same webhook paths and handler construction.
func RegisterHandlers(server crwebhook.Server, reader client.Reader, scheme *runtime.Scheme, defaultImage string) {
	server.Register(
		"/mutate-v1-pod",
		&crwebhook.Admission{Handler: NewHandler(reader, scheme, defaultImage)},
	)
	server.Register(
		"/validate-litestream",
		&crwebhook.Admission{Handler: NewLitestreamValidator(reader, scheme)},
	)
	server.Register(
		"/validate-litestream-workload",
		&crwebhook.Admission{Handler: NewWorkloadValidator(reader, scheme)},
	)
	dependencyValidator := NewDependencyValidator(reader, scheme)
	server.Register(
		"/validate-litestreamreplica",
		&crwebhook.Admission{Handler: dependencyValidator},
	)
}
