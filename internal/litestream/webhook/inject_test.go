package webhook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

func TestInjectContainerSecurityContext(t *testing.T) {
	secureDefaults := &corev1.SecurityContext{
		RunAsUser:                ptr.To(int64(65532)),
		RunAsGroup:               ptr.To(int64(65532)),
		RunAsNonRoot:             ptr.To(true),
		ReadOnlyRootFilesystem:   ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	for _, test := range []struct {
		name     string
		override *corev1.SecurityContext
		want     *corev1.SecurityContext
	}{
		{
			name: "applies secure defaults",
			want: secureDefaults,
		},
		{
			name:     "keeps secure defaults alongside unrelated settings",
			override: &corev1.SecurityContext{RunAsUser: ptr.To(int64(10001)), RunAsGroup: ptr.To(int64(2000))},
			want: &corev1.SecurityContext{
				RunAsUser:                ptr.To(int64(10001)),
				RunAsGroup:               ptr.To(int64(2000)),
				RunAsNonRoot:             ptr.To(true),
				ReadOnlyRootFilesystem:   ptr.To(true),
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		},
		{
			name: "preserves explicitly configured settings",
			override: &corev1.SecurityContext{
				ReadOnlyRootFilesystem: ptr.To(false),
				Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN"}},
			},
			want: &corev1.SecurityContext{
				RunAsUser:                ptr.To(int64(65532)),
				RunAsGroup:               ptr.To(int64(65532)),
				RunAsNonRoot:             ptr.To(true),
				ReadOnlyRootFilesystem:   ptr.To(false),
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := test.override.DeepCopy()

			got := buildContainerSecurityContext(test.override)

			assert.Assert(t, equality.Semantic.DeepEqual(got, test.want), "got %#v, want %#v", got, test.want)
			assert.Assert(t, equality.Semantic.DeepEqual(test.override, original), "the configured security context was modified")
		})
	}
}

func TestBuildInjectionRejectsRootWithDefaultNonRoot(t *testing.T) {
	input := litestreamconfig.Input{
		Injection: v1alpha1.InjectionSpec{
			ContainerSecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0))},
		},
		Databases: []litestreamconfig.Database{{
			Name: "app",
			Path: testDatabasePath,
			Replicate: &litestreamconfig.Replicate{
				Replica: v1alpha1.ReplicaSpec{
					Type: v1alpha1.ReplicaTypeS3,
					S3:   &v1alpha1.S3ReplicaSpec{Bucket: "bucket", Path: "app"},
				},
			},
		}},
	}
	rendered, err := litestreamconfig.Render(input)
	assert.NilError(t, err)
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "app",
			VolumeMounts: []corev1.VolumeMount{{
				Name: "data", MountPath: testMountPath,
			}},
		}},
		Volumes: []corev1.Volume{{Name: "data"}},
	}}
	target, err := ResolveTarget(pod, input)
	assert.NilError(t, err)

	_, err = buildInjection(pod, input, rendered, target, "config", testDefaultImage)

	assert.ErrorContains(t, err, "runAsUser=0 requires runAsNonRoot=false")
}

func TestCredentialStartupCheckFailsBeforeCommandWhenSecretIsMissing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	command := credentialStartupCheckCommand(
		[]string{"/bin/sh", "-c", "echo started > " + marker},
		[]litestreamconfig.CredentialBinding{{
			ContainerPurpose: "restore-app",
			EnvName:          "LS_APP_SRC_S3_ACCESS_KEY_ID",
			SecretKeyRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "source-s3"},
				Key:                  "access-key-id",
				Optional:             ptr.To(true),
			},
		}},
		"restore-app",
	)

	process := exec.Command(command[0], command[1:]...)
	process.Env = []string{"PATH=/usr/bin:/bin"}
	err := process.Run()

	assert.Assert(t, err != nil, "missing credentials must fail before Litestream starts")
	_, statErr := os.Stat(marker)
	assert.Assert(t, os.IsNotExist(statErr), "the original command must not run: %v", statErr)
}

func TestInjectPodSecurityContext(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured *corev1.PodSecurityContext
		err        string
	}{
		{
			name: "accepts no settings",
		},
		{
			name:       "accepts the settings injection applies",
			configured: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)},
		},
		{
			name:       "rejects settings that would apply to the whole Pod",
			configured: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			err:        "but also sets runAsNonRoot, seccompProfile",
		},
		{
			name:       "rejects a policy that would never take effect",
			configured: &corev1.PodSecurityContext{FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)},
			err:        "sets fsGroupChangePolicy without fsGroup",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkPodSecurityContext(test.configured)

			if test.err != "" {
				assert.ErrorContains(t, err, test.err)
				return
			}
			assert.NilError(t, err)
		})
	}
}

func TestCheckExtraVolumeMountsRejectsUnsafeMounts(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "audit"}, {Name: "logs"}}}}
	injected := []corev1.Volume{{Name: ConfigVolumeName}}
	generated := []corev1.VolumeMount{
		{Name: ConfigVolumeName, MountPath: litestreamconfig.ConfigMountDir},
		{Name: "database", MountPath: "/var/lib/app"},
	}

	for _, test := range []struct {
		name   string
		mounts []corev1.VolumeMount
		err    string
	}{
		{
			name:   "subPathExpr",
			mounts: []corev1.VolumeMount{{Name: "audit", MountPath: "/audit", SubPathExpr: "$(POD_NAME)"}},
			err:    "subPathExpr",
		},
		{
			name:   "generated mount path",
			mounts: []corev1.VolumeMount{{Name: "audit", MountPath: litestreamconfig.ConfigMountDir + "/extra"}},
			err:    "overlaps generated mount",
		},
		{
			name:   "generated volume",
			mounts: []corev1.VolumeMount{{Name: ConfigVolumeName, MountPath: "/audit"}},
			err:    "generated volume",
		},
		{
			name: "duplicate path",
			mounts: []corev1.VolumeMount{
				{Name: "audit", MountPath: "/audit"},
				{Name: "audit", MountPath: "/audit/"},
			},
			err: "duplicate mount path",
		},
		{
			name: "nested paths",
			mounts: []corev1.VolumeMount{
				{Name: "audit", MountPath: "/audit"},
				{Name: "logs", MountPath: "/audit/logs"},
			},
			err: "overlaps another extra volume mount",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkExtraVolumeMounts(pod, injected, generated, test.mounts)
			assert.ErrorContains(t, err, test.err)
		})
	}
}

func TestInjectResolveImage(t *testing.T) {
	for _, test := range []struct {
		name         string
		image        v1alpha1.ImageSpec
		defaultImage string
		want         string
		err          string
	}{
		{
			name:         "uses the controller default",
			defaultImage: testDefaultImage,
			want:         testDefaultImage,
		},
		{
			name:         "accepts a registry port in the default repository",
			defaultImage: "registry.example.com:5000/litestream@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			want:         "registry.example.com:5000/litestream@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		{
			name:         "accepts a tagged default repository with a digest",
			defaultImage: "registry.example.com/litestream:latest@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			want:         "registry.example.com/litestream:latest@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		{
			name:         "rejects a mutable repository and tag override",
			image:        v1alpha1.ImageSpec{Repository: "registry.example.com/litestream", Tag: "0.6.0"},
			defaultImage: testDefaultImage,
			err:          "digest",
		},
		{
			name:         "rejects a mutable tag override",
			image:        v1alpha1.ImageSpec{Tag: "0.6.0"},
			defaultImage: testDefaultImage,
			err:          "digest",
		},
		{
			name:         "rejects a mutable repository-only override",
			image:        v1alpha1.ImageSpec{Repository: "registry.example.com/litestream"},
			defaultImage: testDefaultImage,
			err:          "digest",
		},
		{
			name: "accepts a digest override",
			image: v1alpha1.ImageSpec{
				Repository: "registry.example.com/litestream",
				Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
			defaultImage: "litestream/litestream@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			want:         "registry.example.com/litestream@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name: "accepts a tag with a digest override",
			image: v1alpha1.ImageSpec{
				Repository: "registry.example.com/litestream",
				Tag:        "0.6.0",
				Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
			defaultImage: testDefaultImage,
			want:         "registry.example.com/litestream:0.6.0@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name: "accepts a registry port in the repository",
			image: v1alpha1.ImageSpec{
				Repository: "registry.example.com:5000/litestream",
				Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
			defaultImage: testDefaultImage,
			want:         "registry.example.com:5000/litestream@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		{
			name: "rejects a tagged repository with a digest",
			image: v1alpha1.ImageSpec{
				Repository: "registry.example.com/litestream:latest",
				Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
			defaultImage: testDefaultImage,
			err:          "must not include a tag",
		},
		{
			name:  "rejects a missing default image",
			image: v1alpha1.ImageSpec{},
			err:   "default litestream image",
		},
		{
			name:         "rejects a malformed default repository",
			defaultImage: "registry.example.com//litestream@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			err:          "invalid repository",
		},
		{
			name: "rejects a malformed repository override",
			image: v1alpha1.ImageSpec{
				Repository: "registry.example.com//litestream",
				Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
			defaultImage: testDefaultImage,
			err:          "invalid repository",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource := &v1alpha1.Litestream{Spec: v1alpha1.LitestreamSpec{Image: test.image}}

			got, err := resolveImage(resource.Spec.Image, test.defaultImage)

			if test.err != "" {
				assert.ErrorContains(t, err, test.err)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, got, test.want)
		})
	}
}

func TestInjectSecretProjections(t *testing.T) {
	serviceAccount := litestreamconfig.SecretMountDir + "/app-src/gcs-service-account.json"
	privateKey := litestreamconfig.SecretMountDir + "/app-dest/sftp-private-key"

	bindings := []litestreamconfig.CredentialBinding{
		{ContainerPurpose: "replicate", EnvName: "LS_APP_DEST_S3_ACCESS_KEY_ID", SecretKeyRef: keySelector("destination-s3", "access-key-id")},
		{ContainerPurpose: "replicate", SecretKeyRef: keySelector("destination-sftp", "id_ed25519"), FileMountPath: privateKey},
		{ContainerPurpose: "restore-app", EnvName: "GOOGLE_APPLICATION_CREDENTIALS", SecretKeyRef: keySelector("source-gcs", "key.json"), FileMountPath: serviceAccount},
		{ContainerPurpose: "restore-app", SecretKeyRef: keySelector("source-gcs", "key.json"), FileMountPath: serviceAccount},
	}

	got := secretProjections(bindings, "restore-app")

	want := []corev1.VolumeProjection{
		{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "source-gcs"},
			Items:                []corev1.KeyToPath{{Key: "key.json", Path: "app-src/gcs-service-account.json"}},
		}},
	}
	assert.Assert(t, equality.Semantic.DeepEqual(got, want), "got %#v, want %#v", got, want)
}

func TestInjectSecretProjectionsPreservesOptional(t *testing.T) {
	optional := true
	got := secretProjections([]litestreamconfig.CredentialBinding{{
		ContainerPurpose: "restore-app",
		SecretKeyRef: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "source-gcs"},
			Key:                  "key.json",
			Optional:             &optional,
		},
		FileMountPath: litestreamconfig.SecretMountDir + "/app-src/gcs-service-account.json",
	}}, "restore-app")

	assert.Assert(t, got[0].Secret.Optional != nil)
	assert.Equal(t, *got[0].Secret.Optional, true)
}

func TestInjectSecretVolumeName(t *testing.T) {
	sources := secretProjections([]litestreamconfig.CredentialBinding{
		{ContainerPurpose: "restore-app", SecretKeyRef: keySelector("source-gcs", "key.json"), FileMountPath: litestreamconfig.SecretMountDir + "/app-src/gcs-service-account.json"},
	}, "restore-app")
	other := secretProjections([]litestreamconfig.CredentialBinding{
		{ContainerPurpose: "restore-app", SecretKeyRef: keySelector("source-gcs", "other.json"), FileMountPath: litestreamconfig.SecretMountDir + "/app-src/gcs-service-account.json"},
	}, "restore-app")

	name := secretVolumeName(sources)

	assert.Equal(t, name, secretVolumeName(sources))
	assert.Assert(t, name != secretVolumeName(other), "different credentials must not share a volume name")
	assert.Assert(t, strings.HasPrefix(name, SecretVolumeNamePrefix), "got %q", name)
	assert.Assert(t, len(validation.IsDNS1123Label(name)) == 0, "%q is not a valid volume name", name)
}

func TestDatabaseVolumeMountRejectsSubPathExpr(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name:         "app",
		VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data", SubPathExpr: "$(POD_NAME)"}},
	}}}}

	_, err := databaseVolumeMount(pod, ResolvedTarget{
		ContainerIndex: 0,
		ContainerName:  "app",
		VolumeName:     "data",
		MountPath:      "/data",
	})

	assert.ErrorContains(t, err, "subPathExpr")
}

func TestDatabaseVolumeMountClearsRecursiveReadOnly(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app",
		VolumeMounts: []corev1.VolumeMount{{
			Name:              "data",
			MountPath:         "/data",
			ReadOnly:          true,
			RecursiveReadOnly: ptr.To(corev1.RecursiveReadOnlyEnabled),
		}},
	}}}}

	got, err := databaseVolumeMount(pod, ResolvedTarget{
		ContainerIndex: 0,
		ContainerName:  "app",
		VolumeName:     "data",
		MountPath:      "/data",
	})

	assert.NilError(t, err)
	assert.Assert(t, !got.ReadOnly)
	assert.Assert(t, got.RecursiveReadOnly == nil)
}

func keySelector(name, key string) corev1.SecretKeySelector {
	return corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
	}
}
