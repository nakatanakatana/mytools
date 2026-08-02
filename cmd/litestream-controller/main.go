// Command litestream-controller runs the Litestream controller manager: the
// LitestreamReconciler that renders Litestream resources into ConfigMaps,
// and the /mutate-v1-pod admission webhook that injects Litestream into
// annotated Pods at creation.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/controller"
	webhookpkg "github.com/nakatanakatana/mytools/internal/litestream/webhook"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

const (
	defaultMetricsBindAddress     = ":8080"
	defaultHealthProbeBindAddress = ":8081"
	defaultLitestreamImage        = "litestream/litestream@sha256:f45ca298a567bef6edd23d43429b5f80721473a9a9719e467f11d7888999403e"

	leaderElectionID = "litestream-controller.mytools.nakatanakatana.app"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

// options holds the manager's command-line configuration.
type options struct {
	metricsBindAddress     string
	healthProbeBindAddress string
	leaderElect            bool
	webhookCertPath        string
	defaultLitestreamImage string
}

// parseFlags parses args into options, starting from the manager's default
// configuration, and validates the result.
func parseFlags(args []string) (options, error) {
	opts := options{
		metricsBindAddress:     defaultMetricsBindAddress,
		healthProbeBindAddress: defaultHealthProbeBindAddress,
		defaultLitestreamImage: defaultLitestreamImage,
	}

	fs := flag.NewFlagSet("litestream-controller", flag.ContinueOnError)
	fs.StringVar(&opts.metricsBindAddress, "metrics-bind-address", opts.metricsBindAddress,
		"The address the metrics endpoint binds to.")
	fs.StringVar(&opts.healthProbeBindAddress, "health-probe-bind-address", opts.healthProbeBindAddress,
		"The address the health probe endpoint (/healthz, /readyz) binds to.")
	fs.BoolVar(&opts.leaderElect, "leader-elect", opts.leaderElect,
		"Enable leader election, so that only one manager instance reconciles at a time.")
	fs.StringVar(&opts.webhookCertPath, "webhook-cert-path", opts.webhookCertPath,
		"The directory containing the webhook server's TLS certificate and key, tls.crt and tls.key.")
	fs.StringVar(&opts.defaultLitestreamImage, "default-litestream-image", opts.defaultLitestreamImage,
		"The Litestream image injected into a Pod when its Litestream resource does not override it.")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if err := opts.validate(); err != nil {
		return options{}, err
	}
	return opts, nil
}

// validate rejects configuration the manager must not start with.
func (o options) validate() error {
	if strings.TrimSpace(o.defaultLitestreamImage) == "" {
		return errors.New("--default-litestream-image must not be empty")
	}
	if err := webhookpkg.ValidateImageReference(o.defaultLitestreamImage); err != nil {
		return fmt.Errorf("--default-litestream-image: %w", err)
	}
	return nil
}

// managerOptions translates opts into the manager configuration.
func managerOptions(opts options) ctrl.Options {
	return ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: opts.metricsBindAddress},
		HealthProbeBindAddress: opts.healthProbeBindAddress,
		LeaderElection:         opts.leaderElect,
		LeaderElectionID:       leaderElectionID,
		WebhookServer:          webhook.NewServer(webhook.Options{CertDir: opts.webhookCertPath}),
	}
}

// setupManager wires the reconciler, the admission webhook, and the health
// checks into mgr.
func setupManager(mgr ctrl.Manager, opts options) error {
	reconciler := &controller.LitestreamReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("litestream-controller"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create the litestream controller: %w", err)
	}

	webhookpkg.RegisterHandlers(mgr.GetWebhookServer(), mgr.GetAPIReader(), mgr.GetScheme(), opts.defaultLitestreamImage)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up the healthz check: %w", err)
	}
	if err := registerReadinessChecks(mgr.AddReadyzCheck, mgr.GetWebhookServer().StartedChecker()); err != nil {
		return err
	}
	return nil
}

func registerReadinessChecks(add func(string, healthz.Checker) error, webhookChecker healthz.Checker) error {
	if err := add("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up the readyz check: %w", err)
	}
	if err := add("webhook", webhookChecker); err != nil {
		return fmt.Errorf("unable to set up the webhook readiness check: %w", err)
	}
	return nil
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New())

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions(opts))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: unable to start the manager: %v\n", err)
		os.Exit(1)
	}

	if err := setupManager(mgr, opts); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: problem running the manager: %v\n", err)
		os.Exit(1)
	}
}
