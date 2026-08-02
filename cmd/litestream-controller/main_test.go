package main

import (
	"net/http"
	"testing"

	"gotest.tools/v3/assert"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func TestParseFlagsAppliesDefaults(t *testing.T) {
	opts, err := parseFlags(nil)
	assert.NilError(t, err)

	assert.Equal(t, opts.metricsBindAddress, ":8080")
	assert.Equal(t, opts.healthProbeBindAddress, ":8081")
	assert.Equal(t, opts.leaderElect, false)
	assert.Equal(t, opts.webhookCertPath, "")
	assert.Assert(t, opts.defaultLitestreamImage != "", "a default litestream image must be configured out of the box")
}

func TestParseFlagsOverridesDefaults(t *testing.T) {
	opts, err := parseFlags([]string{
		"--metrics-bind-address", ":9090",
		"--health-probe-bind-address", ":9091",
		"--leader-elect",
		"--webhook-cert-path", "/tmp/certs",
		"--default-litestream-image", "example.com/litestream@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	})
	assert.NilError(t, err)

	assert.Equal(t, opts.metricsBindAddress, ":9090")
	assert.Equal(t, opts.healthProbeBindAddress, ":9091")
	assert.Equal(t, opts.leaderElect, true)
	assert.Equal(t, opts.webhookCertPath, "/tmp/certs")
	assert.Equal(t, opts.defaultLitestreamImage, "example.com/litestream@sha256:0000000000000000000000000000000000000000000000000000000000000000")
}

func TestParseFlagsRejectsMutableDefaultImage(t *testing.T) {
	_, err := parseFlags([]string{"--default-litestream-image", "example.com/litestream:1.0.0"})
	assert.ErrorContains(t, err, "sha256 digest")
}

func TestParseFlagsAcceptsTaggedDefaultImageWithDigest(t *testing.T) {
	image := "example.com/litestream:latest@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	opts, err := parseFlags([]string{"--default-litestream-image", image})
	assert.NilError(t, err)
	assert.Equal(t, opts.defaultLitestreamImage, image)
}

func TestParseFlagsRejectsAnEmptyDefaultImage(t *testing.T) {
	_, err := parseFlags([]string{"--default-litestream-image", ""})
	assert.ErrorContains(t, err, "default-litestream-image")
}

func TestParseFlagsRejectsAWhitespaceOnlyDefaultImage(t *testing.T) {
	_, err := parseFlags([]string{"--default-litestream-image", "   "})
	assert.ErrorContains(t, err, "default-litestream-image")
}

func TestManagerOptionsWireTheConfiguredAddresses(t *testing.T) {
	opts, err := parseFlags([]string{
		"--metrics-bind-address", ":9090",
		"--health-probe-bind-address", ":9091",
		"--webhook-cert-path", "/tmp/certs",
	})
	assert.NilError(t, err)

	managerOpts := managerOptions(opts)
	assert.Equal(t, managerOpts.Metrics.BindAddress, ":9090")
	assert.Equal(t, managerOpts.HealthProbeBindAddress, ":9091")
	assert.Assert(t, managerOpts.WebhookServer != nil)
}

func TestRegisterReadinessChecksIncludesWebhook(t *testing.T) {
	var names []string
	add := func(name string, _ healthz.Checker) error {
		names = append(names, name)
		return nil
	}

	err := registerReadinessChecks(add, func(_ *http.Request) error { return nil })
	assert.NilError(t, err)
	assert.DeepEqual(t, names, []string{"readyz", "webhook"})
}
