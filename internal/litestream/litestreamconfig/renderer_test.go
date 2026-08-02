package litestreamconfig_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRenderBackends(t *testing.T) {
	for _, tt := range []struct {
		name       string
		replica    v1alpha1.ReplicaSpec
		goldenFile string
		// wantEnv is asserted against the rendered YAML text. Backends whose
		// credentials are delivered only through a mounted file (no YAML
		// field references them) leave this empty and are instead checked
		// against got.Credentials below.
		wantEnv string
	}{
		{"s3", backendS3(), "s3.golden", "${LS_APP_DEST_S3_SECRET_ACCESS_KEY}"},
		{"gcs", backendGCS(), "gcs.golden", ""},
		{"azure", backendAzure(), "azure.golden", "${LS_APP_DEST_AZURE_ACCOUNT_KEY}"},
		{"file", backendFile(), "file.golden", "type: file"},
		{"nats", backendNATS(), "nats.golden", "${LS_APP_DEST_NATS_PASSWORD}"},
		{"oss", backendOSS(), "oss.golden", "${LS_APP_DEST_OSS_ACCESS_KEY_SECRET}"},
		{"sftp", backendSFTP(), "sftp.golden", "${LS_APP_DEST_SFTP_PASSWORD}"},
		{"webdav", backendWebDAV(), "webdav.golden", "${LS_APP_DEST_WEBDAV_PASSWORD}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resource := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app.db", tt.replica))

			got, err := litestreamconfig.Render(resource)
			assert.NilError(t, err)

			golden.Assert(t, got.Data["replicate.yml"], tt.goldenFile)
			assert.Assert(t, !strings.Contains(got.Data["replicate.yml"], "actual-secret-value"))
			if tt.wantEnv != "" {
				assert.Assert(t, strings.Contains(got.Data["replicate.yml"], tt.wantEnv), got.Data["replicate.yml"])
			}
		})
	}
}

func TestRenderGCSUsesFileMountedServiceAccount(t *testing.T) {
	resource := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app.db", backendGCS()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, !strings.Contains(got.Data["replicate.yml"], "service-account"))

	var found bool
	for _, c := range got.Credentials {
		if c.EnvName == "GOOGLE_APPLICATION_CREDENTIALS" {
			found = true
			assert.Equal(t, c.FileMountPath, "/etc/litestream-secrets/app-dest/gcs-service-account.json")
			assert.Equal(t, c.SecretKeyRef.Name, "destination-gcs")
			assert.Equal(t, c.SecretKeyRef.Key, "service-account.json")
		}
	}
	assert.Assert(t, found, "expected a GOOGLE_APPLICATION_CREDENTIALS credential binding")
}

func TestRenderRestoreOnly(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	_, hasReplicate := got.Data["replicate.yml"]
	assert.Assert(t, !hasReplicate, "restore-only databases must not produce a replicate.yml")

	golden.Assert(t, got.Data["restore-app.yml"], "restore-only-config.golden")
	golden.Assert(t, got.Data["restore-app.sh"], "restore-only-script.golden")

	wantEnv := "${LS_APP_SRC_S3_ACCESS_KEY_ID}"
	assert.Assert(t, strings.Contains(got.Data["restore-app.yml"], wantEnv))
	assert.Assert(t, !strings.Contains(got.Data["restore-app.yml"], "actual-secret-value"))
}

func TestRenderRestoreQuotesPointInTimeFlags(t *testing.T) {
	db := restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3())
	db.Restore.Timestamp = "2026-01-01T00:00:00Z"
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	script := got.Data["restore-app.sh"]
	assert.Assert(t, strings.Contains(script, "'-timestamp' '2026-01-01T00:00:00Z'"), script)
}

func TestRenderOverwriteKeepsExistingDatabaseWhenReplicaIsMissing(t *testing.T) {
	databaseDir := t.TempDir()
	databasePath := filepath.Join(databaseDir, "app.db")
	assert.NilError(t, os.WriteFile(databasePath, []byte("original"), 0o600))
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		assert.NilError(t, os.WriteFile(databasePath+suffix, []byte("sidecar"), 0o600))
	}

	db := restoreOnlyDatabase("app", databasePath, backendS3())
	db.Restore.IfDatabaseExists = v1alpha1.IfDatabaseExistsOverwrite
	db.Restore.IfReplicaMissing = v1alpha1.IfReplicaMissingSkip
	got, err := litestreamconfig.Render(litestreamInput(db))
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ] && [ \"$2\" = -dry-run ]; then\n" +
		"  printf 'no matching backup files available\\n' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"if [ \"$1\" = restore ]; then\n" +
		"  output=\"\"\n" +
		"  previous=\"\"\n" +
		"  for arg do\n" +
		"    if [ \"$previous\" = -o ]; then output=\"$arg\"; fi\n" +
		"    previous=\"$arg\"\n" +
		"  done\n" +
		"  if [ \"$output\" = \"\" ]; then output=\"$previous\"; fi\n" +
		"  if [ \"$output\" = \"$EXPECTED_DATABASE\" ]; then\n" +
		"    rm -f \"$output\"\n" +
		"  fi\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 99\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data[litestreamconfig.RestoreScriptFileName("app")]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"EXPECTED_DATABASE="+databasePath,
	)
	output, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(output))

	contents, err := os.ReadFile(databasePath)
	assert.NilError(t, err)
	assert.Equal(t, string(contents), "original")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		contents, err := os.ReadFile(databasePath + suffix)
		assert.NilError(t, err)
		assert.Equal(t, string(contents), "sidecar")
	}
}

func TestRenderOverwriteRestoresThroughTemporaryOutput(t *testing.T) {
	databaseDir := t.TempDir()
	databasePath := filepath.Join(databaseDir, "app.db")
	assert.NilError(t, os.WriteFile(databasePath, []byte("original"), 0o600))
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		assert.NilError(t, os.WriteFile(databasePath+suffix, []byte("stale-sidecar"), 0o600))
	}

	db := restoreOnlyDatabase("app", databasePath, backendS3())
	db.Restore.IfDatabaseExists = v1alpha1.IfDatabaseExistsOverwrite
	db.Restore.IfReplicaMissing = v1alpha1.IfReplicaMissingFail
	got, err := litestreamconfig.Render(litestreamInput(db))
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ]; then\n" +
		"  output=\"\"\n" +
		"  previous=\"\"\n" +
		"  for arg do\n" +
		"    if [ \"$previous\" = -o ]; then output=\"$arg\"; fi\n" +
		"    previous=\"$arg\"\n" +
		"  done\n" +
		"  if [ \"$output\" = \"\" ] || [ \"$output\" = \"$EXPECTED_DATABASE\" ]; then\n" +
		"    printf 'restore must use a temporary output path\\n' >&2\n" +
		"    exit 42\n" +
		"  fi\n" +
		"  case \"$output\" in\n" +
		"    \"$EXPECTED_DIRECTORY\"/.litestream-restore-*) ;;\n" +
		"    *) printf 'unexpected output path: %s\\n' \"$output\" >&2; exit 43 ;;\n" +
		"  esac\n" +
		"  printf 'restored' > \"$output\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 99\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data[litestreamconfig.RestoreScriptFileName("app")]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"EXPECTED_DATABASE="+databasePath,
		"EXPECTED_DIRECTORY="+databaseDir,
	)
	output, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(output))

	contents, err := os.ReadFile(databasePath)
	assert.NilError(t, err)
	assert.Equal(t, string(contents), "restored")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(databasePath + suffix)
		assert.Assert(t, os.IsNotExist(err), "restored database must not retain stale SQLite sidecar %s", suffix)
	}
}

func TestRenderReplicateImplicitRestore(t *testing.T) {
	resource := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	golden.Assert(t, got.Data["restore-app.yml"], "replicate-implicit-restore-config.golden")
	golden.Assert(t, got.Data["restore-app.sh"], "replicate-implicit-restore-script.golden")

	// The destination credential must be usable both by the sidecar
	// (purpose "replicate") and by the implicit restore init container
	// (purpose "restore-app").
	purposes := credentialPurposesFor(got.Credentials, "LS_APP_DEST_S3_SECRET_ACCESS_KEY")
	sort.Strings(purposes)
	assert.DeepEqual(t, purposes, []string{"replicate", "restore-app"})
}

func TestRenderReplicateExplicitRestore(t *testing.T) {
	db := litestreamconfig.Database{
		Name: "app",
		Path: "/var/lib/app/app.db",
		Restore: &litestreamconfig.Restore{
			Replica:          backendGCS(),
			IfDatabaseExists: v1alpha1.IfDatabaseExistsSkip,
			IfReplicaMissing: v1alpha1.IfReplicaMissingSkip,
			IntegrityCheck:   v1alpha1.IntegrityCheckQuick,
		},
		Replicate: &litestreamconfig.Replicate{Replica: backendS3()},
	}
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, strings.Contains(got.Data["restore-app.yml"], "type: gs"))
	assert.Assert(t, strings.Contains(got.Data["replicate.yml"], "type: s3"))
	assert.Assert(t, !strings.Contains(got.Data["restore-app.yml"], "type: s3"))
}

func TestRenderCloneSeparatesSourceAndDestination(t *testing.T) {
	db := cloneDatabase("app", "/var/lib/app/app.db", backendGCS(), backendS3())
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	golden.Assert(t, got.Data["restore-app-source.yml"], "clone-source-config.golden")
	golden.Assert(t, got.Data["restore-app-destination.yml"], "clone-destination-config.golden")
	golden.Assert(t, got.Data["replicate.yml"], "clone-replicate-config.golden")
	golden.Assert(t, got.Data["restore-app.sh"], "clone-script.golden")

	assert.Assert(t, strings.Contains(got.Data["restore-app-source.yml"], "type: gs"))
	assert.Assert(t, !strings.Contains(got.Data["restore-app-destination.yml"], "type: gs"))
	assert.Assert(t, strings.Contains(got.Data["restore-app-destination.yml"], "type: s3"))

	// replicate.yml carries only the destination, never the base source.
	assert.Assert(t, strings.Contains(got.Data["replicate.yml"], "type: s3"))
	assert.Assert(t, !strings.Contains(got.Data["replicate.yml"], "type: gs"))

	script := got.Data["restore-app.sh"]
	destIdx := strings.Index(script, "restore-app-destination.yml")
	srcIdx := strings.Index(script, "restore-app-source.yml")
	assert.Assert(t, destIdx >= 0 && srcIdx >= 0 && destIdx < srcIdx, "script must restore from the destination before the base source")
}

func TestRenderReplicateAutoRecover(t *testing.T) {
	db := replicateOnlyDatabase("app", "/var/lib/app/app.db", backendS3())
	db.Replicate.AutoRecover = true
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(got.Data["replicate.yml"], "auto-recover: true"))
}

func TestRenderCloneRequireEmptyPreflight(t *testing.T) {
	db := cloneDatabase("app", "/var/lib/app/app.db", backendGCS(), backendS3())
	db.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	golden.Assert(t, got.Data["restore-app.sh"], "clone-require-empty-script.golden")

	script := got.Data["restore-app.sh"]
	preflightIdx := strings.Index(script, "litestream restore -dry-run -json")
	destIdx := strings.Index(script, "restore-app-destination.yml")
	assert.Assert(t, preflightIdx >= 0 && preflightIdx < destIdx, "the require-empty preflight must run before any restore attempt")
	assert.Assert(t, strings.Contains(script, "exit 1"))
}

func TestRenderRequireEmptyUsesFormatAwareRestorePlan(t *testing.T) {
	db := cloneDatabase("app", "/var/lib/app/app.db", backendGCS(), backendS3())
	db.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	script := got.Data["restore-app.sh"]
	assert.Assert(t, strings.Contains(script, "litestream restore -dry-run -json -if-replica-exists"), script)
	assert.Assert(t, !strings.Contains(script, "litestream ltx"), script)
}

func TestRenderRequireEmptyPreflightFailsClosed(t *testing.T) {
	databaseDir := t.TempDir()
	db := cloneDatabase("app", filepath.Join(databaseDir, "app.db"), backendGCS(), backendS3())
	db.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ]; then\n" +
		"  printf 'credential failure\\n' >&2\n" +
		"  exit 42\n" +
		"fi\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	assert.Assert(t, err != nil, "require-empty preflight must fail closed on restore plan errors; output: %s", output)
}

func TestRenderRequireEmptyRejectsLegacyDataFromRestorePlan(t *testing.T) {
	databaseDir := t.TempDir()
	db := cloneDatabase("app", filepath.Join(databaseDir, "app.db"), backendGCS(), backendS3())
	db.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ] && [ \"$2\" = -dry-run ]; then\n" +
		"  printf '{\"files\":[{\"name\":\"legacy-snapshot\"}]}\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = ltx ]; then\n" +
		"  printf '[]\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf 'unexpected restore invocation\\n' >&2\n" +
		"exit 99\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	assert.Assert(t, err != nil, "require-empty must reject a non-empty format-aware restore plan")
	assert.Assert(t, strings.Contains(string(output), "already contains data"), string(output))
}

func TestRenderRequireEmptyPreflightAcceptsEmptyRestorePlan(t *testing.T) {
	databaseDir := t.TempDir()
	db := cloneDatabase("app", filepath.Join(databaseDir, "app.db"), backendGCS(), backendS3())
	db.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ] && [ \"$2\" = -dry-run ]; then\n" +
		"  printf '{\"files\":[]}\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = restore ]; then\n" +
		"  for last do :; done\n" +
		"  : > \"$last\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf 'unexpected command\\n' >&2\n" +
		"exit 99\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(output))
}

func TestRenderRequireEmptyPreflightTreatsMissingBackupsAsEmpty(t *testing.T) {
	databaseDir := t.TempDir()
	db := cloneDatabase("app", filepath.Join(databaseDir, "app.db"), backendGCS(), backendS3())
	db.ClonePolicy = v1alpha1.ClonePolicyRequireEmpty
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ] && [ \"$2\" = -dry-run ]; then\n" +
		"  printf 'no matching backup files available\\n' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"if [ \"$1\" = restore ]; then\n" +
		"  for last do :; done\n" +
		"  : > \"$last\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf 'unexpected command\\n' >&2\n" +
		"exit 99\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(output))
}

func TestRenderCredentialsAreSorted(t *testing.T) {
	db := cloneDatabase("app", "/var/lib/app/app.db", backendGCS(), backendS3())
	resource := litestreamInput(db)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	sorted := append([]litestreamconfig.CredentialBinding(nil), got.Credentials...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.ContainerPurpose != b.ContainerPurpose {
			return a.ContainerPurpose < b.ContainerPurpose
		}
		if a.EnvName != b.EnvName {
			return a.EnvName < b.EnvName
		}
		return a.FileMountPath < b.FileMountPath
	})
	assert.DeepEqual(t, got.Credentials, sorted)
}

func TestRenderHashIsDeterministicAndSensitive(t *testing.T) {
	resource := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))

	first, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	second, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	assert.Equal(t, first.Hash, second.Hash)
	assert.Equal(t, len(first.Hash), 64)

	changed := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app2.db", backendS3()))
	third, err := litestreamconfig.Render(changed)
	assert.NilError(t, err)
	assert.Assert(t, first.Hash != third.Hash)
}

func TestRenderHashIncludesCredentialBindingSelectors(t *testing.T) {
	resource := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))

	first, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	second, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	assert.Equal(t, first.Hash, second.Hash)

	changed := resource
	changed.Databases = append([]litestreamconfig.Database(nil), resource.Databases...)
	changed.Databases[0].Replicate = &litestreamconfig.Replicate{Replica: resource.Databases[0].Replicate.Replica}
	changed.Databases[0].Replicate.Replica.S3 = resource.Databases[0].Replicate.Replica.S3.DeepCopy()
	changed.Databases[0].Replicate.Replica.S3.Credentials.SecretAccessKey = secretRef("rotated-s3", "secret-access-key")

	third, err := litestreamconfig.Render(changed)
	assert.NilError(t, err)
	assert.DeepEqual(t, third.Data, first.Data)
	assert.Assert(t, first.Hash != third.Hash, "a credential selector-only change must create a new revision hash")
}

func TestRenderRejectsInvalidResolvedInput(t *testing.T) {
	for _, tt := range []struct {
		name      string
		input     litestreamconfig.Input
		wantName  string
		wantField string
	}{
		{
			name: "mixed GCS replication credentials",
			input: litestreamInput(
				replicateOnlyDatabase("app", "/var/lib/app/app.db", backendGCSWithServiceAccount("first-gcs")),
				replicateOnlyDatabase("orders", "/var/lib/orders/orders.db", backendGCSWithServiceAccount("second-gcs")),
			),
			wantName:  "orders",
			wantField: "replicate.replica.gcs.serviceAccountJSON",
		},
		{
			name:      "clone source and destination GCS credentials differ",
			input:     litestreamInput(cloneDatabase("app", "/var/lib/app/app.db", backendGCSWithServiceAccount("source-gcs"), backendGCSWithServiceAccount("destination-gcs"))),
			wantName:  "app",
			wantField: "replicate.replica.gcs.serviceAccountJSON",
		},
		{
			name: "directory permissions for root-level database",
			input: func() litestreamconfig.Input {
				input := litestreamInput(restoreOnlyDatabase("app", "/app.db", backendS3()))
				input.Injection.Permissions.DirectoryMode = "0750"
				return input
			}(),
			wantName:  "app",
			wantField: "injection.permissions.directoryMode",
		},
		{
			name: "duplicate resolved name",
			input: litestreamInput(
				restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()),
				replicateOnlyDatabase("app", "/var/lib/other/app.db", backendS3()),
			),
			wantName:  "app",
			wantField: "name",
		},
		{
			name: "duplicate resolved path",
			input: litestreamInput(
				restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()),
				replicateOnlyDatabase("orders", "/var/lib/app/app.db", backendS3()),
			),
			wantName:  "orders",
			wantField: "path",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := litestreamconfig.Render(tt.input)
			assert.ErrorContains(t, err, `database "`+tt.wantName+`"`)
			assert.ErrorContains(t, err, tt.wantField)
		})
	}
}

func TestRenderAcceptsAmbientGCSCredentials(t *testing.T) {
	resource := litestreamInput(
		replicateOnlyDatabase("app", "/var/lib/app/app.db", backendGCSWithServiceAccount("")),
		replicateOnlyDatabase("orders", "/var/lib/orders/orders.db", backendGCSWithServiceAccount("")),
		cloneDatabase("report", "/var/lib/report/report.db", backendGCSWithServiceAccount(""), backendGCSWithServiceAccount("")),
	)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	assert.Assert(t, got.Data["replicate.yml"] != "")
	assert.Equal(t, len(got.Credentials), 0)
}

func TestRenderKeepsResolvedSourceAndDestinationScoped(t *testing.T) {
	sharedSource := backendGCSWithServiceAccount("source-gcs")
	resource := litestreamInput(
		explicitRestoreReplicateDatabase("app", "/var/lib/app/app.db", sharedSource, backendS3()),
		explicitRestoreReplicateDatabase("orders", "/var/lib/orders/orders.db", sharedSource, backendS3WithPath("environments/pr-123/orders")),
	)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, strings.Contains(got.Data["restore-app.yml"], "/var/lib/app/app.db"))
	assert.Assert(t, strings.Contains(got.Data["restore-orders.yml"], "/var/lib/orders/orders.db"))
	assert.Assert(t, strings.Contains(got.Data["replicate.yml"], "/var/lib/app/app.db"))
	assert.Assert(t, strings.Contains(got.Data["replicate.yml"], "/var/lib/orders/orders.db"))

	for _, binding := range got.Credentials {
		switch binding.SecretKeyRef.Name {
		case "source-gcs":
			assert.Assert(t, strings.HasPrefix(binding.ContainerPurpose, "restore-"), binding)
		case "destination-s3":
			assert.Equal(t, binding.ContainerPurpose, litestreamconfig.ReplicateContainerPurpose)
		}
	}

	changedSource := backendGCSWithBucket("source-gcs", "changed-source-bucket")
	sourceChanged := litestreamInput(
		explicitRestoreReplicateDatabase("app", "/var/lib/app/app.db", changedSource, backendS3()),
		explicitRestoreReplicateDatabase("orders", "/var/lib/orders/orders.db", changedSource, backendS3WithPath("environments/pr-123/orders")),
	)
	sourceRendered, err := litestreamconfig.Render(sourceChanged)
	assert.NilError(t, err)
	assert.Assert(t, got.Hash != sourceRendered.Hash)

	destinationChanged := litestreamInput(
		explicitRestoreReplicateDatabase("app", "/var/lib/app/app.db", backendGCSWithServiceAccount("source-gcs"), backendS3WithPath("environments/pr-123/changed")),
		explicitRestoreReplicateDatabase("orders", "/var/lib/orders/orders.db", backendGCSWithServiceAccount("source-gcs"), backendS3WithPath("environments/pr-123/orders")),
	)
	destinationRendered, err := litestreamconfig.Render(destinationChanged)
	assert.NilError(t, err)
	assert.Assert(t, got.Hash != destinationRendered.Hash)
}

func TestRenderRejectsUnsafeDatabasePath(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/var/lib/app/app\n.db", backendS3()))

	_, err := litestreamconfig.Render(resource)
	assert.ErrorContains(t, err, "unsafe shell argument")
}

func TestRenderMultipleDatabases(t *testing.T) {
	resource := litestreamInput(
		restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()),
		replicateOnlyDatabase("orders", "/var/lib/orders/orders.db", backendGCS()),
	)

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, got.Data["restore-app.yml"] != "")
	assert.Assert(t, got.Data["restore-app.sh"] != "")
	assert.Assert(t, got.Data["restore-orders.yml"] != "")
	assert.Assert(t, got.Data["restore-orders.sh"] != "")
	assert.Assert(t, strings.Contains(got.Data["replicate.yml"], "/var/lib/orders/orders.db"))
	assert.Assert(t, !strings.Contains(got.Data["replicate.yml"], "/var/lib/app/app.db"))
}

func TestRenderOmittedPermissionsPreserveExistingModes(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, !strings.Contains(got.Data["restore-app.sh"], "chmod"), got.Data["restore-app.sh"])
}

func TestRenderRootDatabasePathDoesNotChmodRootDirectory(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/app.db", backendS3()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, !strings.Contains(got.Data["restore-app.sh"], "chmod 2770 '/'"))
}

func TestRenderRestoreFailsWhenPermissionChangesFail(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", filepath.Join(t.TempDir(), "app.db"), backendS3()))
	resource.Injection.Permissions = v1alpha1.PermissionsSpec{
		DirectoryMode: "0750",
		DatabaseMode:  "0640",
	}

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	litestream := "#!/bin/sh\n" +
		"if [ \"$1\" = restore ]; then\n" +
		"  for last do :; done\n" +
		"  : > \"$last\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 99\n"
	assert.NilError(t, os.WriteFile(litestreamPath, []byte(litestream), 0o755))
	chmodPath := filepath.Join(binDir, "chmod")
	assert.NilError(t, os.WriteFile(chmodPath, []byte("#!/bin/sh\nexit 42\n"), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	assert.Assert(t, err != nil, "restore must fail when configured permission changes fail; output: %s", output)
	assert.Assert(t, strings.Contains(string(output), "unable to apply Litestream permissions"), output)
}

func TestRenderRestoreSkipsPermissionChangesWhenDatabaseIsMissing(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", filepath.Join(t.TempDir(), "missing", "app.db"), backendS3()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	binDir := t.TempDir()
	litestreamPath := filepath.Join(binDir, "litestream")
	assert.NilError(t, os.WriteFile(litestreamPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	cmd := exec.Command("/bin/sh", scriptPath)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	assert.NilError(t, err, string(output))
}

func TestRenderWebDAVSeparatesURLAndPath(t *testing.T) {
	resource := litestreamInput(replicateOnlyDatabase("app", "/var/lib/app/app.db", backendWebDAV()))

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	config := got.Data["replicate.yml"]
	assert.Assert(t, strings.Contains(config, "webdav-url: https://webdav.example.com"), config)
	assert.Assert(t, strings.Contains(config, "path: backups/app"), config)
}

func TestRenderCustomPermissions(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))
	resource.Injection.Permissions = v1alpha1.PermissionsSpec{
		DirectoryMode: "0750",
		DatabaseMode:  "0640",
	}

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)

	assert.Assert(t, strings.Contains(got.Data["restore-app.sh"], "chmod 0640"))
	assert.Assert(t, strings.Contains(got.Data["restore-app.sh"], "chmod 2750"))
}

func TestRenderDirectoryOnlyPermissionProducesValidScript(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))
	resource.Injection.Permissions.DirectoryMode = "0750"

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	assert.NilError(t, exec.Command("/bin/sh", "-n", scriptPath).Run())
	assert.Assert(t, !strings.Contains(got.Data["restore-app.sh"], "chmod 0"))
	assert.Assert(t, strings.Contains(got.Data["restore-app.sh"], "chmod 2750"))
}

func TestRenderDatabaseOnlyPermissionProducesValidScript(t *testing.T) {
	resource := litestreamInput(restoreOnlyDatabase("app", "/var/lib/app/app.db", backendS3()))
	resource.Injection.Permissions.DatabaseMode = "0640"

	got, err := litestreamconfig.Render(resource)
	assert.NilError(t, err)
	scriptPath := filepath.Join(t.TempDir(), "restore.sh")
	assert.NilError(t, os.WriteFile(scriptPath, []byte(got.Data["restore-app.sh"]), 0o755))

	assert.NilError(t, exec.Command("/bin/sh", "-n", scriptPath).Run())
	assert.Assert(t, strings.Contains(got.Data["restore-app.sh"], "chmod 0640 '/var/lib/app/app.db'"))
	assert.Assert(t, !strings.Contains(got.Data["restore-app.sh"], "chmod 2"))
}

func credentialPurposesFor(bindings []litestreamconfig.CredentialBinding, envName string) []string {
	var purposes []string
	for _, b := range bindings {
		if b.EnvName == envName {
			purposes = append(purposes, b.ContainerPurpose)
		}
	}
	return purposes
}

func litestreamInput(databases ...litestreamconfig.Database) litestreamconfig.Input {
	return litestreamconfig.Input{Databases: databases}
}

func restoreOnlyDatabase(name, path string, replica v1alpha1.ReplicaSpec) litestreamconfig.Database {
	return litestreamconfig.Database{
		Name: name,
		Path: path,
		Restore: &litestreamconfig.Restore{
			Replica:          replica,
			IfDatabaseExists: v1alpha1.IfDatabaseExistsSkip,
			IfReplicaMissing: v1alpha1.IfReplicaMissingSkip,
			IntegrityCheck:   v1alpha1.IntegrityCheckQuick,
		},
	}
}

func replicateOnlyDatabase(name, path string, replica v1alpha1.ReplicaSpec) litestreamconfig.Database {
	return litestreamconfig.Database{
		Name: name,
		Path: path,
		Replicate: &litestreamconfig.Replicate{
			Replica:      replica,
			SyncInterval: metav1.Duration{},
		},
	}
}

func cloneDatabase(name, path string, source, destination v1alpha1.ReplicaSpec) litestreamconfig.Database {
	return litestreamconfig.Database{
		Name:        name,
		Path:        path,
		Clone:       true,
		ClonePolicy: v1alpha1.ClonePolicyResumeOrCreate,
		Restore: &litestreamconfig.Restore{
			Replica:          source,
			IfDatabaseExists: v1alpha1.IfDatabaseExistsSkip,
			IfReplicaMissing: v1alpha1.IfReplicaMissingSkip,
			IntegrityCheck:   v1alpha1.IntegrityCheckQuick,
		},
		Replicate: &litestreamconfig.Replicate{Replica: destination},
	}
}

func backendS3() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeS3,
		S3: &v1alpha1.S3ReplicaSpec{
			Bucket: "destination-bucket",
			Path:   "environments/pr-123/app",
			Region: "ap-northeast-1",
			Credentials: v1alpha1.S3Credentials{
				AccessKeyID:     secretRef("destination-s3", "access-key-id"),
				SecretAccessKey: secretRef("destination-s3", "secret-access-key"),
			},
		},
	}
}

func backendGCS() v1alpha1.ReplicaSpec {
	return backendGCSWithServiceAccount("destination-gcs")
}

func backendGCSWithServiceAccount(secretName string) v1alpha1.ReplicaSpec {
	return backendGCSWithBucket(secretName, "destination-bucket")
}

func backendGCSWithBucket(secretName, bucket string) v1alpha1.ReplicaSpec {
	var serviceAccountJSON *v1alpha1.SecretReference
	if secretName != "" {
		serviceAccountJSON = secretRef(secretName, "service-account.json")
	}
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeGCS,
		GCS: &v1alpha1.GCSReplicaSpec{
			Bucket:             bucket,
			Path:               "environments/base/app",
			ServiceAccountJSON: serviceAccountJSON,
		},
	}
}

func backendS3WithPath(path string) v1alpha1.ReplicaSpec {
	replica := backendS3()
	replica.S3.Path = path
	return replica
}

func explicitRestoreReplicateDatabase(name, path string, source, destination v1alpha1.ReplicaSpec) litestreamconfig.Database {
	return litestreamconfig.Database{
		Name: name,
		Path: path,
		Restore: &litestreamconfig.Restore{
			Replica:          source,
			IfDatabaseExists: v1alpha1.IfDatabaseExistsSkip,
			IfReplicaMissing: v1alpha1.IfReplicaMissingSkip,
			IntegrityCheck:   v1alpha1.IntegrityCheckQuick,
		},
		Replicate: &litestreamconfig.Replicate{Replica: destination},
	}
}

func backendAzure() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeAzure,
		Azure: &v1alpha1.AzureReplicaSpec{
			AccountName: "destinationaccount",
			Container:   "destination-container",
			Path:        "environments/pr-123/app",
			AccountKey:  secretRef("destination-azure", "account-key"),
		},
	}
}

func backendFile() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeFile,
		File: &v1alpha1.FileReplicaSpec{
			Path: "/mnt/backups/app",
		},
	}
}

func backendNATS() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeNATS,
		NATS: &v1alpha1.NATSReplicaSpec{
			URL:      "nats://nats.example.com:4222/destination-bucket",
			Username: secretRef("destination-nats", "username"),
			Password: secretRef("destination-nats", "password"),
		},
	}
}

func backendOSS() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeOSS,
		OSS: &v1alpha1.OSSReplicaSpec{
			Bucket:          "destination-bucket",
			Path:            "environments/pr-123/app",
			Region:          "oss-cn-hangzhou",
			AccessKeyID:     secretRef("destination-oss", "access-key-id"),
			AccessKeySecret: secretRef("destination-oss", "access-key-secret"),
		},
	}
}

func backendSFTP() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeSFTP,
		SFTP: &v1alpha1.SFTPReplicaSpec{
			Host:     "backup.example.com:22",
			User:     "backup",
			Path:     "/backups/app",
			Password: secretRef("destination-sftp", "password"),
		},
	}
}

func backendWebDAV() v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeWebDAV,
		WebDAV: &v1alpha1.WebDAVReplicaSpec{
			URL:      "https://webdav.example.com",
			Path:     "backups/app",
			Username: secretRef("destination-webdav", "username"),
			Password: secretRef("destination-webdav", "password"),
		},
	}
}

func secretRef(name, key string) *v1alpha1.SecretReference {
	return &v1alpha1.SecretReference{
		SecretKeyRef: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Key:                  key,
		},
	}
}
