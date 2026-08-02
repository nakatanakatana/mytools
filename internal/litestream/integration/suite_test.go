// Package integration_test exercises the Litestream controller and
// admission webhook against a real API server started by envtest. It
// covers behavior the fake-client unit tests cannot: schema defaulting by
// the API server, status subresource semantics, and the reconciler
// actually running off a watched cache.
package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/controller"
	webhookpkg "github.com/nakatanakatana/mytools/internal/litestream/webhook"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// testDefaultImage is the image the admission handler falls back to when a
// Litestream resource does not pin its own.
const testDefaultImage = "litestream/litestream@sha256:f45ca298a567bef6edd23d43429b5f80721473a9a9719e467f11d7888999403e"

// pollInterval and pollTimeout bound every wait in this suite: 10 seconds
// is generous for envtest's in-process API server and reconciler, while
// still failing fast on a genuinely broken harness.
const (
	pollInterval = 100 * time.Millisecond
	pollTimeout  = 10 * time.Second
)

var (
	testEnv    *envtest.Environment
	testScheme *runtime.Scheme

	// k8sClient talks to the envtest API server directly, uncached, so
	// tests observe writes (including status subresource writes made by
	// the reconciler through its own, separate cached client) without
	// waiting on any local cache to warm.
	k8sClient client.Client

	namespaceCounter int
)

func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration suite setup failed:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runSuite starts envtest and a cancellable manager running the real
// LitestreamReconciler, waits for its cache to sync, runs every test in m,
// then tears the manager and envtest down.
func runSuite(m *testing.M) (int, error) {
	testScheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		return 0, fmt.Errorf("add client-go scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(testScheme); err != nil {
		return 0, fmt.Errorf("add v1alpha1 scheme: %w", err)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../config/litestream-controller/crd/bases"},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			Paths: []string{"../../../config/litestream-controller/webhook/manifests.yaml"},
		},
	}

	restConfig, err := testEnv.Start()
	if err != nil {
		return 0, fmt.Errorf("start envtest: %w", err)
	}
	defer func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			fmt.Fprintln(os.Stderr, "stop envtest:", stopErr)
		}
	}()

	k8sClient, err = client.New(restConfig, client.Options{Scheme: testScheme})
	if err != nil {
		return 0, fmt.Errorf("build direct client: %w", err)
	}
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    testEnv.WebhookInstallOptions.LocalServingHost,
			Port:    testEnv.WebhookInstallOptions.LocalServingPort,
			CertDir: testEnv.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		return 0, fmt.Errorf("build manager: %w", err)
	}

	reconciler := &controller.LitestreamReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("litestream-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return 0, fmt.Errorf("wire reconciler into manager: %w", err)
	}
	webhookpkg.RegisterHandlers(mgr.GetWebhookServer(), mgr.GetAPIReader(), mgr.GetScheme(), testDefaultImage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	managerDone := make(chan error, 1)
	go func() { managerDone <- mgr.Start(ctx) }()

	syncCtx, syncCancel := context.WithTimeout(ctx, pollTimeout)
	defer syncCancel()
	if err := wait.PollUntilContextTimeout(syncCtx, pollInterval, pollTimeout, true, func(context.Context) (bool, error) {
		return mgr.GetWebhookServer().StartedChecker()(nil) == nil, nil
	}); err != nil {
		return 0, fmt.Errorf("webhook server did not start within %s: %w", pollTimeout, err)
	}
	if !mgr.GetCache().WaitForCacheSync(syncCtx) {
		return 0, fmt.Errorf("manager cache did not sync within %s", pollTimeout)
	}

	code := m.Run()

	cancel()
	select {
	case <-managerDone:
	case <-time.After(pollTimeout):
		fmt.Fprintln(os.Stderr, "manager did not stop within", pollTimeout)
	}

	return code, nil
}

// newTestNamespace creates a uniquely named Namespace for one test case so
// unrelated cases never observe each other's objects, and returns its name.
// envtest runs no namespace-lifecycle controller, so namespaces are never
// deleted; the whole API server is discarded when the suite ends.
func newTestNamespace(t *testing.T) string {
	t.Helper()
	namespaceCounter++
	name := fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), namespaceCounter)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace %q: %v", name, err)
	}
	return name
}

// waitFor polls cond, bounded by pollTimeout, and fails the test if cond
// never returns true. A transient error from cond (e.g. NotFound while the
// reconciler has not yet run) is retried rather than failing immediately.
func waitFor(t *testing.T, description string, cond func(ctx context.Context) (bool, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, pollTimeout, true, cond)
	if err != nil {
		t.Fatalf("timed out waiting for %s: %v", description, err)
	}
}

// currentLitestream reads one profile through the direct envtest client.
func currentLitestream(t *testing.T, ctx context.Context, namespace, name string) v1alpha1.Litestream {
	t.Helper()
	var resource v1alpha1.Litestream
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &resource); err != nil {
		t.Fatalf("get Litestream %s/%s: %v", namespace, name, err)
	}
	return resource
}
