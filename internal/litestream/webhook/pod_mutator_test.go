package webhook

import (
	"strings"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/controller"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	"github.com/nakatanakatana/mytools/internal/litestream/resolver"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace    = "default"
	testResourceName = "app-db"
	testDefaultImage = "litestream/litestream@sha256:f45ca298a567bef6edd23d43429b5f80721473a9a9719e467f11d7888999403e"
	testMountPath    = "/var/lib/app"
	testDatabasePath = "/var/lib/app/app.db"
)

func TestPodMutatorInjectsRestoreOnlyDatabase(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, restoreOnlyDatabase("app"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	configVolume := podVolume(t, pod, ConfigVolumeName)
	assert.Assert(t, configVolume.ConfigMap != nil, "the config volume must project the generated ConfigMap")
	assert.Equal(t, configVolume.ConfigMap.Name, controller.ConfigMapName(resource))

	restore := podInitContainer(t, pod, RestoreContainerNamePrefix+"app")
	assert.Equal(t, restore.Image, testDefaultImage)
	assert.DeepEqual(t, restore.Command[4:], []string{"/bin/sh", litestreamconfig.ConfigMountDir + "/restore-app.sh"})
	assert.Equal(t, restore.Command[0], "/bin/sh")
	assert.Equal(t, restore.Command[1], "-c")
	assert.Assert(t, strings.Contains(restore.Command[2], "LS_APP_SRC_S3_ACCESS_KEY_ID"),
		"restore must validate environment-backed credentials before starting")
	assert.Equal(t, containerMount(t, restore, ConfigVolumeName).MountPath, litestreamconfig.ConfigMountDir)
	assert.Equal(t, containerMount(t, restore, ConfigVolumeName).ReadOnly, true)
	assert.Equal(t, containerMount(t, restore, "data").MountPath, testMountPath)
	assert.Equal(t, containerMount(t, restore, "data").ReadOnly, false)
	assert.Assert(t, containerEnv(t, restore, "LS_APP_SRC_S3_SECRET_ACCESS_KEY").ValueFrom != nil,
		"credentials must be read by the kubelet, never by the webhook")

	assert.Equal(t, len(pod.Spec.Containers), 1, "restore-only must not add a replication sidecar")
	for _, initContainer := range pod.Spec.InitContainers {
		assert.Assert(t, initContainer.Name != ReplicateContainerName, "restore-only must not add a replication sidecar")
	}
}

func TestPodMutatorInjectsReplicationSidecar(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	// Replication runs as a native sidecar so that it starts after every
	// restore finishes, keeps running for Job Pods without blocking their
	// completion, and shuts down only after the application container has
	// stopped writing to the database.
	sidecar := podInitContainer(t, pod, ReplicateContainerName)
	assert.Assert(t, sidecar.RestartPolicy != nil && *sidecar.RestartPolicy == corev1.ContainerRestartPolicyAlways)
	assert.DeepEqual(t, sidecar.Command[4:], []string{
		"litestream", "replicate", "-config", litestreamconfig.ConfigMountDir + "/replicate.yml",
	})
	assert.Equal(t, sidecar.Command[0], "/bin/sh")
	assert.Equal(t, sidecar.Command[1], "-c")
	assert.Assert(t, strings.Contains(sidecar.Command[2], "LS_APP_DEST_S3_ACCESS_KEY_ID"),
		"replication must validate environment-backed credentials before starting")
	assert.Assert(t, containerEnv(t, sidecar, "LS_APP_DEST_S3_ACCESS_KEY_ID").ValueFrom != nil)

	restore := podInitContainer(t, pod, RestoreContainerNamePrefix+"app")
	assert.Assert(t, containerEnv(t, restore, "LS_APP_DEST_S3_ACCESS_KEY_ID").ValueFrom != nil,
		"a replicate database restores from its own destination")

	assert.Assert(t, initContainerIndex(t, pod, RestoreContainerNamePrefix+"app") < initContainerIndex(t, pod, ReplicateContainerName),
		"replication must start after the database has been restored")
}

func TestPodMutatorRejectsExistingPodUsingSameDestinationAcrossDifferentLitestreams(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource.Name = "existing-db"
	existingResource.Status.ConfigMapName = ""
	existingPod := targetPod()
	existingPod.Name = "existing"
	existingPod.Annotations[InjectAnnotation] = existingResource.Name

	err := newMutator(t, resource, existingResource, existingPod).Mutate(t.Context(), targetPod())

	assert.ErrorContains(t, err, "existing Pod")
}

func TestPodMutatorAllowsExistingPodUsingDifferentDatabasePathOnSameReplica(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingResource.Name = "existing-db"
	existingResource.Spec.Databases[0].Path = "/var/lib/app/other.db"
	existingResource.Status.ConfigMapName = ""
	existingPod := targetPod()
	existingPod.Name = "existing"
	existingPod.Annotations[InjectAnnotation] = existingResource.Name

	err := newMutator(t, resource, existingResource, existingPod).Mutate(t.Context(), targetPod())

	assert.NilError(t, err)
}

func TestPodMutatorRejectsExistingPodUsingSameLitestream(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	existingPod := targetPod()
	existingPod.Name = "existing"

	err := newMutator(t, resource, existingPod).Mutate(t.Context(), targetPod())

	assert.ErrorContains(t, err, "already uses Litestream")
}

func TestPodMutatorIgnoresFinishedPodsWhenCheckingReplicationConflicts(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase), func(t *testing.T) {
			resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
			existingPod := targetPod()
			existingPod.Name = "finished"
			existingPod.Status.Phase = phase

			err := newMutator(t, resource, existingPod).Mutate(t.Context(), targetPod())

			assert.NilError(t, err)
		})
	}
}

func TestPodMutatorRunsRestoreBeforeUserInitContainers(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	pod := targetPod()
	pod.Spec.InitContainers = []corev1.Container{{Name: "prepare-data", Image: "app:1.0.0"}}

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	restoreIndex := initContainerIndex(t, pod, RestoreContainerNamePrefix+"app")
	assert.Assert(t, restoreIndex < initContainerIndex(t, pod, "prepare-data"),
		"restore must run before user init containers")
	assert.Assert(t, restoreIndex < initContainerIndex(t, pod, ReplicateContainerName),
		"replication must start after the database has been restored")
}

func TestPodMutatorOrdersMissingSidecarAfterExistingRestore(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	mutator := newMutator(t, resource)
	pod := targetPod()
	pod.Spec.InitContainers = []corev1.Container{{Name: "prepare-data", Image: "app:1.0.0"}}

	assert.NilError(t, mutator.Mutate(t.Context(), pod))
	pod.Spec.InitContainers = []corev1.Container{
		podInitContainer(t, pod, RestoreContainerNamePrefix+"app"),
		podInitContainer(t, pod, "prepare-data"),
	}

	assert.NilError(t, mutator.Mutate(t.Context(), pod))
	assert.DeepEqual(t, []string{
		RestoreContainerNamePrefix + "app",
		ReplicateContainerName,
		"prepare-data",
	}, []string{
		pod.Spec.InitContainers[0].Name,
		pod.Spec.InitContainers[1].Name,
		pod.Spec.InitContainers[2].Name,
	})

	injected := pod.DeepCopy()
	assert.NilError(t, mutator.Mutate(t.Context(), pod))
	assert.Assert(t, equality.Semantic.DeepEqual(pod, injected), "re-running the mutation changed the Pod")
}

func TestPodMutatorInjectsCloneSources(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, cloneDatabase("app"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	restore := podInitContainer(t, pod, RestoreContainerNamePrefix+"app")
	assert.Assert(t, containerEnv(t, restore, "LS_APP_SRC_S3_ACCESS_KEY_ID").ValueFrom != nil,
		"resume-or-create restores from the base source")
	assert.Assert(t, containerEnv(t, restore, "LS_APP_DEST_S3_ACCESS_KEY_ID").ValueFrom != nil,
		"resume-or-create resumes from the destination first")

	sidecar := podInitContainer(t, pod, ReplicateContainerName)
	assert.Assert(t, containerEnv(t, sidecar, "LS_APP_DEST_S3_ACCESS_KEY_ID").ValueFrom != nil)
}

func TestPodMutatorInjectsSeparateResolvedSourceAndDestination(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, cloneDatabase("app"))
	source := replicaObject("app-source", s3Replica("source-bucket", "source-credentials"))
	destination := replicaObject("app-destination", s3Replica("destination-bucket", "destination-credentials"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource, source, destination).Mutate(t.Context(), pod))

	restore := podInitContainer(t, pod, RestoreContainerNamePrefix+"app")
	assert.Assert(t, containerEnv(t, restore, "LS_APP_SRC_S3_ACCESS_KEY_ID").ValueFrom != nil)
	assert.Assert(t, containerEnv(t, restore, "LS_APP_DEST_S3_ACCESS_KEY_ID").ValueFrom != nil)
	sidecar := podInitContainer(t, pod, ReplicateContainerName)
	assert.Assert(t, containerEnv(t, sidecar, "LS_APP_DEST_S3_ACCESS_KEY_ID").ValueFrom != nil)
	assert.Assert(t, findEnv(sidecar, "LS_APP_SRC_S3_ACCESS_KEY_ID") == nil, "replication must not receive source credentials")
}

func TestPodMutatorRejectsMissingResolvedDependencies(t *testing.T) {
	for _, test := range []struct {
		name     string
		resource *v1alpha1.Litestream
		objects  []client.Object
		want     string
	}{
		{
			name: "source replica",
			resource: readyResource(t, v1alpha1.InjectionSpec{}, v1alpha1.DatabaseBinding{
				Name:    "app",
				Path:    testDatabasePath,
				Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef("missing-source")},
			}),
			want: "missing-source",
		},
		{
			name:     "destination replica",
			resource: readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")),
			want:     "app-destination",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := append([]client.Object{test.resource}, test.objects...)
			err := newMutatorWithObjects(t, objects...).Mutate(t.Context(), targetPod())
			assert.ErrorContains(t, err, test.want)
		})
	}
}

func TestPodMutatorRejectsConfigMapStaleAfterDependencyChange(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, cloneDatabase("app"))
	source := replicaObject("app-source", s3Replica("source-v1", "source-credentials"))
	destination := replicaObject("app-destination", s3Replica("destination", "destination-credentials"))
	objects := []client.Object{resource, source, destination}
	synchronizeReadyStatus(t, resource, objects...)
	configMap := renderedConfigMapWithReader(t, newTestReader(t, objects...), resource)

	changedSource := replicaObject("app-source", s3Replica("source-v2", "source-credentials"))
	err := newMutatorWithObjects(t, resource, changedSource, destination, configMap).Mutate(t.Context(), targetPod())

	assert.ErrorContains(t, err, "published config hash")
}

func TestPodMutatorUsesPathsFromTwoInlineDatabaseBindings(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{},
		v1alpha1.DatabaseBinding{Name: "orders", Path: "/var/lib/app/orders/orders.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("orders-destination")}},
		v1alpha1.DatabaseBinding{Name: "events", Path: "/var/lib/app/events/events.db", Replicate: &v1alpha1.ReplicateSpec{ReplicaRef: objectRef("events-destination")}},
	)
	ordersDestination := replicaObject("orders-destination", s3Replica("orders", "orders-credentials"))
	eventsDestination := replicaObject("events-destination", s3Replica("events", "events-credentials"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource, ordersDestination, eventsDestination).Mutate(t.Context(), pod))

	assert.Equal(t, containerMount(t, podInitContainer(t, pod, ReplicateContainerName), "data").MountPath, testMountPath)
}

func TestPodMutatorProjectsFileCredentials(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, restoreOnlyDatabase("app"))
	source := replicaObject("app-source", gcsReplica("source-gcs"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource, source).Mutate(t.Context(), pod))

	restore := podInitContainer(t, pod, RestoreContainerNamePrefix+"app")
	secretVolume := podVolume(t, pod, secretMount(t, restore).Name)
	assert.Assert(t, secretVolume.Projected != nil, "file credentials must be projected, never copied")
	assert.DeepEqual(t, secretVolume.Projected.Sources, []corev1.VolumeProjection{{
		Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "source-gcs"},
			Items:                []corev1.KeyToPath{{Key: "service-account.json", Path: "app-src/gcs-service-account.json"}},
		},
	}})

	credentialMount := secretMount(t, restore)
	assert.Equal(t, credentialMount.MountPath, litestreamconfig.SecretMountDir)
	assert.Equal(t, credentialMount.ReadOnly, true)

	// A volume mounted inside the read-only ConfigMap mount has to be created
	// under a read-only bind mount, which the runtime can reject with EROFS.
	configMount := containerMount(t, restore, ConfigVolumeName)
	assert.Assert(t, !strings.HasPrefix(credentialMount.MountPath, configMount.MountPath+"/"),
		"credentials at %q must not be nested in the configuration mount at %q",
		credentialMount.MountPath, configMount.MountPath)

	credentials := containerEnv(t, restore, "GOOGLE_APPLICATION_CREDENTIALS")
	assert.Equal(t, credentials.Value, litestreamconfig.SecretMountDir+"/app-src/gcs-service-account.json")
	assert.Assert(t, credentials.ValueFrom == nil, "the file path is not a secret value")
}

func TestPodMutatorScopesFileCredentialsToEachContainerPurpose(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, cloneDatabase("app"))
	source := replicaObject("app-source", gcsReplica("source-gcs"))
	destination := replicaObject("app-destination", sftpReplica("destination-sftp"))
	pod := targetPod()

	assert.NilError(t, newMutator(t, resource, source, destination).Mutate(t.Context(), pod))

	restore := podInitContainer(t, pod, RestoreContainerNamePrefix+"app")
	sidecar := podInitContainer(t, pod, ReplicateContainerName)
	restoreSecret := secretMount(t, restore)
	sidecarSecret := secretMount(t, sidecar)
	assert.Assert(t, restoreSecret.Name != sidecarSecret.Name, "restore and replication must not share credential volumes")

	restoreVolume := podVolume(t, pod, restoreSecret.Name)
	sidecarVolume := podVolume(t, pod, sidecarSecret.Name)
	assert.Assert(t, projectedSecretNames(restoreVolume).contains("source-gcs"), "restore must receive source credentials")
	assert.Assert(t, projectedSecretNames(restoreVolume).contains("destination-sftp"), "restore must receive destination credentials")
	assert.Assert(t, projectedSecretNames(sidecarVolume).contains("destination-sftp"), "replication must receive destination credentials")
	assert.Assert(t, !projectedSecretNames(sidecarVolume).contains("source-gcs"), "replication must not receive source credentials")
}

func TestPodMutatorAppliesInjectionSettings(t *testing.T) {
	injection := v1alpha1.InjectionSpec{
		ExtraVolumeMounts: []corev1.VolumeMount{{Name: "backups", MountPath: "/backups", ReadOnly: true}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		PodSecurityContext:       &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000)), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)},
		ContainerSecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(10001)), RunAsGroup: ptr.To(int64(2000))},
	}
	cr := readyResource(t, injection, replicateDatabase("app"))
	cr.Spec.Image = v1alpha1.ImageSpec{
		Repository: "registry.example.com/litestream",
		Digest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		PullPolicy: corev1.PullAlways,
	}
	pod := targetPod()

	assert.NilError(t, newMutator(t, cr).Mutate(t.Context(), pod))

	for _, injected := range []corev1.Container{
		podInitContainer(t, pod, RestoreContainerNamePrefix+"app"),
		podInitContainer(t, pod, ReplicateContainerName),
	} {
		assert.Equal(t, injected.Image, "registry.example.com/litestream@sha256:0000000000000000000000000000000000000000000000000000000000000000")
		assert.Equal(t, injected.ImagePullPolicy, corev1.PullAlways)
		assert.Assert(t, injected.Resources.Requests.Memory().Equal(resource.MustParse("64Mi")))
		assert.Equal(t, containerMount(t, injected, "backups").MountPath, "/backups")

		assert.Equal(t, *injected.SecurityContext.RunAsUser, int64(10001))
		assert.Equal(t, *injected.SecurityContext.RunAsGroup, int64(2000))
		assert.Equal(t, *injected.SecurityContext.RunAsNonRoot, true)
		assert.Equal(t, *injected.SecurityContext.ReadOnlyRootFilesystem, true)
		assert.Equal(t, *injected.SecurityContext.AllowPrivilegeEscalation, false)
		assert.DeepEqual(t, injected.SecurityContext.Capabilities.Drop, []corev1.Capability{"ALL"})
	}

	assert.Equal(t, *pod.Spec.SecurityContext.FSGroup, int64(2000))
	assert.Equal(t, *pod.Spec.SecurityContext.FSGroupChangePolicy, corev1.FSGroupChangeOnRootMismatch)
}

func TestPodMutatorKeepsAMatchingFSGroup(t *testing.T) {
	injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{
		FSGroup:             ptr.To(int64(2000)),
		FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
	}}
	resource := readyResource(t, injection, replicateDatabase("app"))
	pod := targetPod()
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000)), RunAsUser: ptr.To(int64(1000))}

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	assert.Equal(t, *pod.Spec.SecurityContext.FSGroup, int64(2000))
	assert.Equal(t, *pod.Spec.SecurityContext.RunAsUser, int64(1000))
	assert.Equal(t, *pod.Spec.SecurityContext.FSGroupChangePolicy, corev1.FSGroupChangeOnRootMismatch,
		"a Pod that already carries the fsGroup still needs the configured policy")
}

func TestPodMutatorKeepsAMatchingFSGroupChangePolicy(t *testing.T) {
	injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{
		FSGroup:             ptr.To(int64(2000)),
		FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
	}}
	resource := readyResource(t, injection, replicateDatabase("app"))
	pod := targetPod()
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch)}

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	assert.Equal(t, *pod.Spec.SecurityContext.FSGroup, int64(2000))
	assert.Equal(t, *pod.Spec.SecurityContext.FSGroupChangePolicy, corev1.FSGroupChangeOnRootMismatch)
}

func TestPodMutatorIsIdempotent(t *testing.T) {
	injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000))}}
	resource := readyResource(t, injection, cloneDatabase("app"), replicateDatabase("logs"))
	mutator := newMutator(t, resource)
	pod := targetPod()

	assert.NilError(t, mutator.Mutate(t.Context(), pod))
	injected := pod.DeepCopy()
	assert.NilError(t, mutator.Mutate(t.Context(), pod))

	assert.Assert(t, equality.Semantic.DeepEqual(pod, injected), "re-running the mutation changed the Pod")
}

func TestPodMutatorIgnoresPodsWithoutTheInjectAnnotation(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	pod := targetPod()
	delete(pod.Annotations, InjectAnnotation)
	untouched := pod.DeepCopy()

	assert.NilError(t, newMutator(t, resource).Mutate(t.Context(), pod))

	assert.Assert(t, equality.Semantic.DeepEqual(pod, untouched), "an unannotated Pod must not be modified")
}

func TestPodMutatorRejectsUnusablePods(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod)
		err   string
	}{
		{
			name: "missing resource",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Name = "other"
				return resource, targetPod()
			},
			err: testResourceName,
		},
		{
			name: "resource is not ready",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Status.Conditions[0].Status = metav1.ConditionFalse
				return resource, targetPod()
			},
			err: "not Ready",
		},
		{
			name: "ready condition is stale",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Generation++
				return resource, targetPod()
			},
			err: "stale",
		},
		{
			name: "rendered configuration does not match the published ConfigMap",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
				resource.Status.ConfigHash = "0000"
				return resource, targetPod()
			},
			err: "config hash",
		},
		{
			name: "windows pod",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				pod := targetPod()
				pod.Spec.OS = &corev1.PodOS{Name: corev1.Windows}
				return readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")), pod
			},
			err: "windows",
		},
		{
			name: "unresolvable database volume",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				pod := targetPod()
				pod.Spec.Containers[0].VolumeMounts = nil
				return readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")), pod
			},
			err: testDatabasePath,
		},
		{
			name: "conflicting container name",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				pod := targetPod()
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: ReplicateContainerName, Image: "other"})
				return readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")), pod
			},
			err: `init container "litestream": the Pod already declares a container`,
		},
		{
			name: "conflicting volume name",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				pod := targetPod()
				pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
					Name:         ConfigVolumeName,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				})
				return readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app")), pod
			},
			err: `volume "litestream-config": the Pod already declares a different volume`,
		},
		{
			name: "extra volume mount without a volume",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				injection := v1alpha1.InjectionSpec{
					ExtraVolumeMounts: []corev1.VolumeMount{{Name: "audit", MountPath: "/audit"}},
				}
				return readyResource(t, injection, replicateDatabase("app")), targetPod()
			},
			err: `extra volume mount "audit" names a volume the Pod does not declare`,
		},
		{
			name: "pod security context settings that cannot be honored",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{
					FSGroup:            ptr.To(int64(2000)),
					RunAsUser:          ptr.To(int64(1000)),
					SupplementalGroups: []int64{3000},
				}}
				return readyResource(t, injection, replicateDatabase("app")), targetPod()
			},
			err: "but also sets runAsUser, supplementalGroups",
		},
		{
			name: "fsGroupChangePolicy without an fsGroup",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{
					FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
				}}
				return readyResource(t, injection, replicateDatabase("app")), targetPod()
			},
			err: "sets fsGroupChangePolicy without fsGroup",
		},
		{
			name: "conflicting fsGroupChangePolicy",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{
					FSGroup:             ptr.To(int64(2000)),
					FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
				}}
				pod := targetPod()
				pod.Spec.SecurityContext = &corev1.PodSecurityContext{
					FSGroup:             ptr.To(int64(2000)),
					FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeAlways),
				}
				return readyResource(t, injection, replicateDatabase("app")), pod
			},
			err: `the Pod fsGroupChangePolicy "Always" differs from the configured fsGroupChangePolicy "OnRootMismatch"`,
		},
		{
			name: "conflicting fsGroup",
			build: func(t *testing.T) (*v1alpha1.Litestream, *corev1.Pod) {
				t.Helper()
				injection := v1alpha1.InjectionSpec{PodSecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(2000))}}
				pod := targetPod()
				pod.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr.To(int64(3000))}
				return readyResource(t, injection, replicateDatabase("app")), pod
			},
			err: "fsGroup",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource, pod := test.build(t)
			untouched := pod.DeepCopy()

			err := newMutator(t, resource).Mutate(t.Context(), pod)

			assert.ErrorContains(t, err, test.err)
			assert.Assert(t, equality.Semantic.DeepEqual(pod, untouched), "a rejected Pod must not be modified")
		})
	}
}

func TestPodMutatorRejectsMissingConfigMap(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	mutator := newMutatorWithoutRenderedConfigMaps(t, resource)

	err := mutator.Mutate(t.Context(), targetPod())

	assert.ErrorContains(t, err, "ConfigMap")
}

func TestPodMutatorRejectsEmptyPublishedConfigHash(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	objects := appendDefaultDependencies(resource)
	configMap := renderedConfigMap(t, resource)
	resource.Status.ConfigHash = ""

	err := newMutatorWithObjects(t, append(objects, configMap)...).Mutate(t.Context(), targetPod())

	assert.ErrorContains(t, err, "published config hash is empty")
}

func TestPodMutatorRejectsStaleConfigMap(t *testing.T) {
	resource := readyResource(t, v1alpha1.InjectionSpec{}, replicateDatabase("app"))
	configMap := renderedConfigMap(t, resource)
	configMap.Data["replicate.yml"] = "stale"
	objects := appendDefaultDependencies(resource)
	mutator := newMutatorWithObjects(t, append(objects, configMap)...)

	err := mutator.Mutate(t.Context(), targetPod())

	assert.ErrorContains(t, err, "does not match rendered configuration")
}

func newMutator(t *testing.T, objects ...client.Object) *PodMutator {
	t.Helper()
	objects = appendDefaultDependencies(objects...)
	return newMutatorWithObjects(t, appendRenderedConfigMaps(t, objects...)...)
}

func newMutatorWithoutRenderedConfigMaps(t *testing.T, objects ...client.Object) *PodMutator {
	t.Helper()
	objects = appendDefaultDependencies(objects...)
	populateConfigHashes(t, objects...)
	return newMutatorWithObjects(t, objects...)
}

func newMutatorWithObjects(t *testing.T, objects ...client.Object) *PodMutator {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	return &PodMutator{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		DefaultImage: testDefaultImage,
	}
}

func appendRenderedConfigMaps(t *testing.T, objects ...client.Object) []client.Object {
	t.Helper()
	result := append([]client.Object(nil), objects...)
	reader := newTestReader(t, objects...)
	for _, object := range objects {
		resource, ok := object.(*v1alpha1.Litestream)
		if !ok {
			continue
		}
		name := controller.ConfigMapName(resource)
		found := false
		for _, existing := range objects {
			configMap, ok := existing.(*corev1.ConfigMap)
			if ok && configMap.Namespace == resource.Namespace && configMap.Name == name {
				found = true
				break
			}
		}
		if !found {
			result = append(result, renderedConfigMapWithReader(t, reader, resource))
		}
	}
	return result
}

func renderedConfigMap(t *testing.T, resource *v1alpha1.Litestream) *corev1.ConfigMap {
	t.Helper()
	objects := appendDefaultDependencies(resource)
	reader := newTestReader(t, objects...)
	return renderedConfigMapWithReader(t, reader, resource)
}

func renderedConfigMapWithReader(t *testing.T, reader client.Reader, resource *v1alpha1.Litestream) *corev1.ConfigMap {
	t.Helper()
	input, err := (resolver.Resolver{Reader: reader}).Resolve(t.Context(), resource)
	assert.NilError(t, err)
	rendered, err := litestreamconfig.Render(input)
	assert.NilError(t, err)
	if resource.Status.ConfigHash == "" {
		resource.Status.ConfigHash = rendered.Hash
	}
	controllerRef := true
	blockOwnerDeletion := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.ConfigMapName(resource),
			Namespace: resource.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.GroupVersion.String(),
				Kind:               "Litestream",
				Name:               resource.Name,
				UID:                resource.UID,
				Controller:         &controllerRef,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Data: rendered.Data,
	}
}

// readyResource builds a Litestream resource whose status reports the
// rendering the webhook is expected to inject, as the reconciler would.
func readyResource(t *testing.T, injection v1alpha1.InjectionSpec, databases ...v1alpha1.DatabaseBinding) *v1alpha1.Litestream {
	t.Helper()
	resource := &v1alpha1.Litestream{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testResourceName,
			Namespace:  testNamespace,
			UID:        "0123456789abcdef",
			Generation: 2,
		},
		Spec: v1alpha1.LitestreamSpec{Injection: injection, Databases: databases},
	}

	resource.Status = v1alpha1.LitestreamStatus{
		ObservedGeneration: resource.Generation,
		ConfigMapName:      controller.ConfigMapName(resource),
		Conditions: []metav1.Condition{{
			Type:               controller.ReadyConditionType,
			Status:             metav1.ConditionTrue,
			Reason:             controller.ReasonConfigRendered,
			ObservedGeneration: resource.Generation,
			LastTransitionTime: metav1.Now(),
		}},
	}
	return resource
}

func targetPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "app",
			Namespace:   testNamespace,
			Annotations: map[string]string{InjectAnnotation: testResourceName},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "backups", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			Containers: []corev1.Container{{
				Name:         "app",
				Image:        "app:1.0.0",
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: testMountPath}},
			}},
		},
	}
}

func restoreOnlyDatabase(name string) v1alpha1.DatabaseBinding {
	return v1alpha1.DatabaseBinding{
		Name:    name,
		Path:    databasePath(name),
		Restore: &v1alpha1.RestoreSpec{ReplicaRef: objectRef(name + "-source")},
	}
}

func replicateDatabase(name string) v1alpha1.DatabaseBinding {
	return v1alpha1.DatabaseBinding{
		Name: name,
		Path: databasePath(name),
		Replicate: &v1alpha1.ReplicateSpec{
			ReplicaRef:   objectRef(name + "-destination"),
			SyncInterval: metav1.Duration{Duration: time.Second},
		},
	}
}

func cloneDatabase(name string) v1alpha1.DatabaseBinding {
	database := replicateDatabase(name)
	database.ClonePolicy = v1alpha1.ClonePolicyResumeOrCreate
	database.Restore = &v1alpha1.RestoreSpec{ReplicaRef: objectRef(name + "-source")}
	return database
}

func appendDefaultDependencies(objects ...client.Object) []client.Object {
	result := append([]client.Object(nil), objects...)
	for _, object := range objects {
		resource, ok := object.(*v1alpha1.Litestream)
		if !ok {
			continue
		}
		for _, binding := range resource.Spec.Databases {
			if binding.Restore != nil {
				name := binding.Restore.ReplicaRef.Name
				if !hasObject(result, &v1alpha1.LitestreamReplica{}, resource.Namespace, name) {
					result = append(result, replicaObject(name, s3Replica("source", "source-s3")))
				}
			}
			if binding.Replicate != nil {
				name := binding.Replicate.ReplicaRef.Name
				if !hasObject(result, &v1alpha1.LitestreamReplica{}, resource.Namespace, name) {
					result = append(result, replicaObject(name, s3Replica("destination", "destination-s3")))
				}
			}
		}
	}
	return result
}

func hasObject(objects []client.Object, want client.Object, namespace, name string) bool {
	for _, object := range objects {
		if sameObjectType(object, want) && object.GetNamespace() == namespace && object.GetName() == name {
			return true
		}
	}
	return false
}

func sameObjectType(left, right client.Object) bool {
	switch right.(type) {
	case *v1alpha1.LitestreamReplica:
		_, ok := left.(*v1alpha1.LitestreamReplica)
		return ok
	default:
		return false
	}
}

func replicaObject(name string, replica v1alpha1.ReplicaSpec) *v1alpha1.LitestreamReplica {
	return &v1alpha1.LitestreamReplica{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}, Spec: v1alpha1.LitestreamReplicaSpec{Replica: replica}}
}

func objectRef(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}

func newTestReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	assert.NilError(t, corev1.AddToScheme(scheme))
	assert.NilError(t, v1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func synchronizeReadyStatus(t *testing.T, resource *v1alpha1.Litestream, objects ...client.Object) {
	t.Helper()
	input, err := (resolver.Resolver{Reader: newTestReader(t, objects...)}).Resolve(t.Context(), resource)
	assert.NilError(t, err)
	rendered, err := litestreamconfig.Render(input)
	assert.NilError(t, err)
	resource.Status.ConfigHash = rendered.Hash
}

func populateConfigHashes(t *testing.T, objects ...client.Object) {
	t.Helper()
	for _, object := range objects {
		resource, ok := object.(*v1alpha1.Litestream)
		if !ok || resource.Status.ConfigHash != "" {
			continue
		}
		synchronizeReadyStatus(t, resource, objects...)
	}
}

func databasePath(name string) string {
	if name == "app" {
		return testDatabasePath
	}
	return testMountPath + "/" + name + ".db"
}

func s3Replica(bucket, secretName string) v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeS3,
		S3: &v1alpha1.S3ReplicaSpec{
			Bucket: bucket,
			Path:   "app",
			Region: "ap-northeast-1",
			Credentials: v1alpha1.S3Credentials{
				AccessKeyID:     secretReference(secretName, "access-key-id"),
				SecretAccessKey: secretReference(secretName, "secret-access-key"),
			},
		},
	}
}

func gcsReplica(secretName string) v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeGCS,
		GCS: &v1alpha1.GCSReplicaSpec{
			Bucket:             "source",
			Path:               "app",
			ServiceAccountJSON: secretReference(secretName, "service-account.json"),
		},
	}
}

func sftpReplica(secretName string) v1alpha1.ReplicaSpec {
	return v1alpha1.ReplicaSpec{
		Type: v1alpha1.ReplicaTypeSFTP,
		SFTP: &v1alpha1.SFTPReplicaSpec{
			Host:       "backup.example.com:22",
			User:       "backup",
			Path:       "/backups/app",
			PrivateKey: secretReference(secretName, "private-key"),
		},
	}
}

func secretReference(name, key string) *v1alpha1.SecretReference {
	return &v1alpha1.SecretReference{SecretKeyRef: keySelector(name, key)}
}

func podVolume(t *testing.T, pod *corev1.Pod, name string) corev1.Volume {
	t.Helper()
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == name {
			return volume
		}
	}
	t.Fatalf("Pod has no volume %q", name)
	return corev1.Volume{}
}

func podInitContainer(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	return pod.Spec.InitContainers[initContainerIndex(t, pod, name)]
}

func initContainerIndex(t *testing.T, pod *corev1.Pod, name string) int {
	t.Helper()
	for i, initContainer := range pod.Spec.InitContainers {
		if initContainer.Name == name {
			return i
		}
	}
	t.Fatalf("Pod has no init container %q", name)
	return -1
}

func containerMount(t *testing.T, injected corev1.Container, name string) corev1.VolumeMount {
	t.Helper()
	for _, volumeMount := range injected.VolumeMounts {
		if volumeMount.Name == name {
			return volumeMount
		}
	}
	t.Fatalf("container %q has no volume mount %q", injected.Name, name)
	return corev1.VolumeMount{}
}

func containerEnv(t *testing.T, injected corev1.Container, name string) corev1.EnvVar {
	t.Helper()
	for _, env := range injected.Env {
		if env.Name == name {
			return env
		}
	}
	t.Fatalf("container %q has no environment variable %q", injected.Name, name)
	return corev1.EnvVar{}
}

func findEnv(injected corev1.Container, name string) *corev1.EnvVar {
	for i := range injected.Env {
		if injected.Env[i].Name == name {
			return &injected.Env[i]
		}
	}
	return nil
}

func secretMount(t *testing.T, container corev1.Container) corev1.VolumeMount {
	t.Helper()
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == litestreamconfig.SecretMountDir {
			return mount
		}
	}
	t.Fatalf("container %q has no secret mount", container.Name)
	return corev1.VolumeMount{}
}

type secretNames []string

func (names secretNames) contains(want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func projectedSecretNames(volume corev1.Volume) secretNames {
	var names secretNames
	if volume.Projected == nil {
		return names
	}
	for _, source := range volume.Projected.Sources {
		if source.Secret != nil {
			names = append(names, source.Secret.Name)
		}
	}
	return names
}
