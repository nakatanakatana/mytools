package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

const (
	// ConfigVolumeName holds the generated Litestream configuration and
	// restore scripts.
	ConfigVolumeName = "litestream-config"

	// SecretVolumeNamePrefix starts the name of the projected volume that
	// carries file-backed credentials. The suffix is a hash of the
	// projection, so unrelated credentials never share a name.
	SecretVolumeNamePrefix = "litestream-secret-"

	// RestoreContainerNamePrefix starts the name of the init container that
	// restores one database.
	RestoreContainerNamePrefix = "litestream-restore-"

	// ReplicateContainerName is the replication sidecar. It is injected as a
	// native sidecar, an init container with restartPolicy Always, so
	// injected Pods require Kubernetes 1.29 or later, where the
	// SidecarContainers feature gate is on by default. The feature reached
	// GA in 1.33.
	ReplicateContainerName = "litestream"

	// injectedFileMode keeps the injected configuration and credentials
	// readable by any user the Litestream container runs as, since neither
	// volume is owned by that user unless fsGroup applies.
	injectedFileMode int32 = 0o444

	// volumeNameHashLength keeps injected volume names short enough for the
	// 63 character DNS label limit.
	volumeNameHashLength = 16

	// defaultLitestreamUID/GID match the nonroot user in the official hardened
	// Litestream image. They also make the default Debian image compatible with
	// runAsNonRoot while retaining its /bin/sh required by restore scripts.
	defaultLitestreamUID int64 = 65532
	defaultLitestreamGID int64 = 65532
)

// podInjection is the complete set of fragments to add to one Pod. It is
// built before anything is applied so that a rejected Pod is never left
// partially mutated.
type podInjection struct {
	volumes            []corev1.Volume
	initContainers     []corev1.Container
	podSecurityContext *corev1.PodSecurityContext
}

// buildInjection renders every fragment needed to run resource's databases
// alongside pod's application container. It reads pod but never modifies it.
func buildInjection(
	pod *corev1.Pod,
	input litestreamconfig.Input,
	rendered litestreamconfig.RenderedConfig,
	target ResolvedTarget,
	configMapName string,
	defaultImage string,
) (podInjection, error) {
	image, err := resolveImage(input.Image, defaultImage)
	if err != nil {
		return podInjection{}, err
	}
	databaseMount, err := databaseVolumeMount(pod, target)
	if err != nil {
		return podInjection{}, err
	}

	volumes := []corev1.Volume{{
		Name: ConfigVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
			DefaultMode:          ptr.To(injectedFileMode),
		}},
	}}
	baseMounts := []corev1.VolumeMount{
		{Name: ConfigVolumeName, MountPath: litestreamconfig.ConfigMountDir, ReadOnly: true},
	}
	baseMounts = append(baseMounts, databaseMount)
	secretMounts := make(map[string]corev1.VolumeMount)
	addSecretMount := func(purpose string) {
		sources := secretProjections(rendered.Credentials, purpose)
		if len(sources) == 0 {
			return
		}
		name := secretVolumeName(sources)
		if findVolume(volumes, name) == nil {
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					Sources:     sources,
					DefaultMode: ptr.To(injectedFileMode),
				}},
			})
		}
		secretMounts[purpose] = corev1.VolumeMount{Name: name, MountPath: litestreamconfig.SecretMountDir, ReadOnly: true}
	}
	for _, database := range input.Databases {
		addSecretMount(litestreamconfig.RestoreContainerPurpose(database.Name))
	}
	if _, replicates := rendered.Data[litestreamconfig.ReplicateConfigFile]; replicates {
		addSecretMount(litestreamconfig.ReplicateContainerPurpose)
	}

	generatedMounts := append([]corev1.VolumeMount(nil), baseMounts...)
	for _, secretMount := range secretMounts {
		generatedMounts = append(generatedMounts, secretMount)
	}
	if err := checkExtraVolumeMounts(pod, volumes, generatedMounts, input.Injection.ExtraVolumeMounts); err != nil {
		return podInjection{}, err
	}
	if err := checkPodSecurityContext(input.Injection.PodSecurityContext); err != nil {
		return podInjection{}, err
	}
	if err := validateContainerSecurityContext(input.Injection.ContainerSecurityContext); err != nil {
		return podInjection{}, err
	}

	injection := podInjection{
		volumes:            volumes,
		podSecurityContext: input.Injection.PodSecurityContext,
	}
	for _, database := range input.Databases {
		mounts := append([]corev1.VolumeMount(nil), baseMounts...)
		mounts = append(mounts, input.Injection.ExtraVolumeMounts...)
		if secretMount, ok := secretMounts[litestreamconfig.RestoreContainerPurpose(database.Name)]; ok {
			mounts = append(mounts, secretMount)
		}
		restoreName := RestoreContainerNamePrefix + database.Name
		if problems := validation.IsDNS1123Label(restoreName); len(problems) > 0 {
			return podInjection{}, fmt.Errorf(
				"cannot inject litestream: database %q needs init container %q, which is not a valid container name: %s",
				database.Name, restoreName, strings.Join(problems, "; "),
			)
		}
		script := path.Join(litestreamconfig.ConfigMountDir, litestreamconfig.RestoreScriptFileName(database.Name))
		injection.initContainers = append(injection.initContainers, injectedContainer(
			restoreName,
			input,
			image,
			credentialStartupCheckCommand(
				[]string{"/bin/sh", script},
				rendered.Credentials,
				litestreamconfig.RestoreContainerPurpose(database.Name),
			),
			credentialEnv(rendered.Credentials, litestreamconfig.RestoreContainerPurpose(database.Name)),
			mounts,
			nil,
		))
	}

	if _, replicates := rendered.Data[litestreamconfig.ReplicateConfigFile]; replicates {
		mounts := append([]corev1.VolumeMount(nil), baseMounts...)
		mounts = append(mounts, input.Injection.ExtraVolumeMounts...)
		if secretMount, ok := secretMounts[litestreamconfig.ReplicateContainerPurpose]; ok {
			mounts = append(mounts, secretMount)
		}
		// Replication is a native sidecar: it then starts only after every
		// restore has finished, lets Job Pods complete, and keeps running
		// until the application container has stopped writing.
		injection.initContainers = append(injection.initContainers, injectedContainer(
			ReplicateContainerName,
			input,
			image,
			credentialStartupCheckCommand(
				[]string{
					"litestream", "replicate",
					"-config", path.Join(litestreamconfig.ConfigMountDir, litestreamconfig.ReplicateConfigFile),
				},
				rendered.Credentials,
				litestreamconfig.ReplicateContainerPurpose,
			),
			credentialEnv(rendered.Credentials, litestreamconfig.ReplicateContainerPurpose),
			mounts,
			ptr.To(corev1.ContainerRestartPolicyAlways),
		))
	}
	return injection, nil
}

// applyInjection adds every fragment to pod, rejecting any name already used
// by a different element. Callers must pass a copy of the admitted Pod: the
// checks stop at the first conflict, part way through the fragments.
func applyInjection(pod *corev1.Pod, injection podInjection) error {
	for _, volume := range injection.volumes {
		existing := findVolume(pod.Spec.Volumes, volume.Name)
		if existing == nil {
			pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
			continue
		}
		if !equality.Semantic.DeepEqual(*existing, volume) {
			return fmt.Errorf("cannot inject volume %q: the Pod already declares a different volume with that name", volume.Name)
		}
	}

	for _, injected := range injection.initContainers {
		if findContainer(pod.Spec.Containers, injected.Name) != nil {
			return fmt.Errorf("cannot inject init container %q: the Pod already declares a container with that name", injected.Name)
		}
		existing := findContainer(pod.Spec.InitContainers, injected.Name)
		if existing != nil && !equality.Semantic.DeepEqual(*existing, injected) {
			return fmt.Errorf("cannot inject init container %q: the Pod already declares a different init container with that name", injected.Name)
		}
	}

	initContainers := make([]corev1.Container, 0, len(injection.initContainers)+len(pod.Spec.InitContainers))
	for _, injected := range injection.initContainers {
		if existing := findContainer(pod.Spec.InitContainers, injected.Name); existing != nil {
			initContainers = append(initContainers, *existing)
		} else {
			initContainers = append(initContainers, injected)
		}
	}
	for _, existing := range pod.Spec.InitContainers {
		if findContainer(injection.initContainers, existing.Name) == nil {
			initContainers = append(initContainers, existing)
		}
	}
	pod.Spec.InitContainers = initContainers

	return applyPodSecurityContext(pod, injection.podSecurityContext)
}

// applyPodSecurityContext shares the database between the application and
// Litestream by adding the configured fsGroup and the policy that governs it.
// Existing values belong to the workload owner, so a value that differs from
// the configured one is reported instead of overwritten.
func applyPodSecurityContext(pod *corev1.Pod, configured *corev1.PodSecurityContext) error {
	if configured == nil || configured.FSGroup == nil {
		return nil
	}

	if current := pod.Spec.SecurityContext; current != nil {
		if current.FSGroup != nil && *current.FSGroup != *configured.FSGroup {
			return fmt.Errorf(
				"cannot inject litestream: the Pod fsGroup %d differs from the configured fsGroup %d",
				*current.FSGroup, *configured.FSGroup,
			)
		}
		if current.FSGroupChangePolicy != nil && configured.FSGroupChangePolicy != nil &&
			*current.FSGroupChangePolicy != *configured.FSGroupChangePolicy {
			return fmt.Errorf(
				"cannot inject litestream: the Pod fsGroupChangePolicy %q differs from the configured fsGroupChangePolicy %q",
				*current.FSGroupChangePolicy, *configured.FSGroupChangePolicy,
			)
		}
	}

	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if pod.Spec.SecurityContext.FSGroup == nil {
		pod.Spec.SecurityContext.FSGroup = ptr.To(*configured.FSGroup)
	}
	if pod.Spec.SecurityContext.FSGroupChangePolicy == nil && configured.FSGroupChangePolicy != nil {
		pod.Spec.SecurityContext.FSGroupChangePolicy = ptr.To(*configured.FSGroupChangePolicy)
	}
	return nil
}

func injectedContainer(
	name string,
	input litestreamconfig.Input,
	image string,
	command []string,
	env []corev1.EnvVar,
	mounts []corev1.VolumeMount,
	restartPolicy *corev1.ContainerRestartPolicy,
) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: input.Image.PullPolicy,
		Command:         command,
		Env:             env,
		VolumeMounts:    append([]corev1.VolumeMount(nil), mounts...),
		Resources:       *input.Injection.Resources.DeepCopy(),
		SecurityContext: buildContainerSecurityContext(input.Injection.ContainerSecurityContext),
		RestartPolicy:   restartPolicy,
	}
}

// checkPodSecurityContext rejects the settings the webhook cannot honor.
// Anything other than fsGroup and fsGroupChangePolicy would have to be
// applied to the whole Pod, including the application container, which is
// not the injection's to change. Reporting them beats accepting a resource
// whose security settings are silently dropped.
func checkPodSecurityContext(configured *corev1.PodSecurityContext) error {
	if ignored := ignoredPodSecurityContextFields(configured); len(ignored) > 0 {
		return fmt.Errorf(
			"cannot inject litestream: injection.podSecurityContext supports only fsGroup and fsGroupChangePolicy, but also sets %s",
			strings.Join(ignored, ", "),
		)
	}
	// The kubelet applies fsGroupChangePolicy only while it is taking
	// ownership of volumes for an fsGroup, so a policy on its own would
	// never take effect.
	if configured != nil && configured.FSGroupChangePolicy != nil && configured.FSGroup == nil {
		return fmt.Errorf("cannot inject litestream: injection.podSecurityContext sets fsGroupChangePolicy without fsGroup, which has no effect")
	}
	return nil
}

func validateContainerSecurityContext(configured *corev1.SecurityContext) error {
	if configured == nil || configured.RunAsUser == nil || *configured.RunAsUser != 0 {
		return nil
	}
	if configured.RunAsNonRoot == nil || *configured.RunAsNonRoot {
		return fmt.Errorf("cannot inject litestream: injection.containerSecurityContext runAsUser=0 requires runAsNonRoot=false")
	}
	return nil
}

// ignoredPodSecurityContextFields names every field set on configured that
// applyPodSecurityContext would not carry over. It works by elimination so
// that fields added to the Kubernetes API are reported rather than dropped.
func ignoredPodSecurityContextFields(configured *corev1.PodSecurityContext) []string {
	if configured == nil {
		return nil
	}
	remaining := configured.DeepCopy()
	remaining.FSGroup = nil
	remaining.FSGroupChangePolicy = nil

	value := reflect.ValueOf(*remaining)
	structType := value.Type()

	var ignored []string
	for i := range value.NumField() {
		if value.Field(i).IsZero() {
			continue
		}
		ignored = append(ignored, fieldName(structType.Field(i)))
	}
	return ignored
}

// fieldName reports the name the resource's author wrote, not the Go name.
func fieldName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if name == "" {
		return field.Name
	}
	return name
}

// checkExtraVolumeMounts confirms the configured extra mounts name volumes
// the Pod carries and cannot shadow generated mounts. It also rejects
// subPathExpr because the injected containers do not inherit the application's
// environment used to expand it.
func checkExtraVolumeMounts(pod *corev1.Pod, injected []corev1.Volume, generated []corev1.VolumeMount, mounts []corev1.VolumeMount) error {
	seenPaths := make(map[string]struct{}, len(mounts))
	seenMounts := make([]corev1.VolumeMount, 0, len(mounts))
	for _, volumeMount := range mounts {
		if volumeMount.SubPathExpr != "" {
			return fmt.Errorf(
				"cannot inject litestream: extra volume mount %q uses subPathExpr, which cannot be evaluated for injected containers",
				volumeMount.Name,
			)
		}
		if findVolume(injected, volumeMount.Name) != nil {
			return fmt.Errorf(
				"cannot inject litestream: extra volume mount %q names a generated volume",
				volumeMount.Name,
			)
		}
		if findVolume(pod.Spec.Volumes, volumeMount.Name) == nil {
			return fmt.Errorf(
				"cannot inject litestream: extra volume mount %q names a volume the Pod does not declare",
				volumeMount.Name,
			)
		}

		mountPath := path.Clean(volumeMount.MountPath)
		if _, ok := seenPaths[mountPath]; ok {
			return fmt.Errorf("cannot inject litestream: extra volume mounts contain duplicate mount path %q", volumeMount.MountPath)
		}
		seenPaths[mountPath] = struct{}{}
		for _, seenMount := range seenMounts {
			if mountPathsOverlap(mountPath, seenMount.MountPath) {
				return fmt.Errorf(
					"cannot inject litestream: extra volume mount %q at %q overlaps another extra volume mount %q at %q",
					volumeMount.Name, volumeMount.MountPath, seenMount.Name, seenMount.MountPath,
				)
			}
		}
		for _, generatedMount := range generated {
			if mountPathsOverlap(mountPath, generatedMount.MountPath) {
				return fmt.Errorf(
					"cannot inject litestream: extra volume mount %q at %q overlaps generated mount %q at %q",
					volumeMount.Name, volumeMount.MountPath, generatedMount.Name, generatedMount.MountPath,
				)
			}
		}
		seenMounts = append(seenMounts, volumeMount)
	}
	return nil
}

func mountPathsOverlap(left, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// databaseVolumeMount mounts the application's database volume into the
// injected containers exactly as the application sees it, so that the
// configured database paths resolve to the same files.
func databaseVolumeMount(pod *corev1.Pod, target ResolvedTarget) (corev1.VolumeMount, error) {
	if target.ContainerIndex < 0 || target.ContainerIndex >= len(pod.Spec.Containers) {
		return corev1.VolumeMount{}, fmt.Errorf("cannot inject litestream: container %q is no longer part of the Pod", target.ContainerName)
	}
	for _, volumeMount := range pod.Spec.Containers[target.ContainerIndex].VolumeMounts {
		if volumeMount.Name != target.VolumeName || path.Clean(volumeMount.MountPath) != target.MountPath {
			continue
		}
		injected := *volumeMount.DeepCopy()
		if injected.SubPathExpr != "" {
			return corev1.VolumeMount{}, fmt.Errorf(
				"cannot inject litestream: volume mount %q uses subPathExpr, which cannot be evaluated for injected containers",
				injected.Name,
			)
		}
		injected.MountPath = target.MountPath
		injected.ReadOnly = false
		injected.RecursiveReadOnly = nil
		return injected, nil
	}
	return corev1.VolumeMount{}, fmt.Errorf(
		"cannot inject litestream: container %q no longer mounts volume %q at %q",
		target.ContainerName, target.VolumeName, target.MountPath,
	)
}

// credentialEnv builds the environment of one injected container. Bindings
// that also name a file carry that file's path, not the Secret value, which
// only the kubelet ever reads.
func credentialEnv(bindings []litestreamconfig.CredentialBinding, purpose string) []corev1.EnvVar {
	var env []corev1.EnvVar
	named := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.ContainerPurpose != purpose || binding.EnvName == "" {
			continue
		}
		if _, duplicate := named[binding.EnvName]; duplicate {
			continue
		}
		named[binding.EnvName] = struct{}{}

		if binding.FileMountPath != "" {
			env = append(env, corev1.EnvVar{Name: binding.EnvName, Value: binding.FileMountPath})
			continue
		}
		secretKeyRef := binding.SecretKeyRef
		env = append(env, corev1.EnvVar{
			Name:      binding.EnvName,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &secretKeyRef},
		})
	}
	return env
}

// credentialStartupCheckCommand fails before the wrapped command starts when
// an environment-backed Secret reference was not resolved by the kubelet.
// Omitting a reference entirely leaves the command unchanged so ambient
// credentials continue to work.
func credentialStartupCheckCommand(command []string, bindings []litestreamconfig.CredentialBinding, purpose string) []string {
	envNames := make([]string, 0, len(bindings))
	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.ContainerPurpose != purpose || binding.EnvName == "" || binding.FileMountPath != "" {
			continue
		}
		if _, duplicate := seen[binding.EnvName]; duplicate {
			continue
		}
		seen[binding.EnvName] = struct{}{}
		envNames = append(envNames, binding.EnvName)
	}
	if len(envNames) == 0 {
		return command
	}

	var script strings.Builder
	script.WriteString("set -eu\n")
	for _, envName := range envNames {
		fmt.Fprintf(&script, "if [ -z \"${%s:-}\" ]; then\n", envName)
		fmt.Fprintf(&script, "  echo 'error: required Litestream credential %s is missing' >&2\n", envName)
		script.WriteString("  exit 1\nfi\n")
	}
	script.WriteString("exec \"$@\"\n")

	wrapped := []string{"/bin/sh", "-c", script.String(), "litestream-credential-check"}
	return append(wrapped, command...)
}

// secretProjections returns only the file-backed credentials needed by one
// injected container purpose. Each key gets its own projection so a SecretKey
// Selector's Optional setting can be preserved even when another key in the
// same Secret is required.
func secretProjections(bindings []litestreamconfig.CredentialBinding, purpose string) []corev1.VolumeProjection {
	projected := make(map[string]struct{}, len(bindings))
	sources := make([]corev1.VolumeProjection, 0, len(bindings))

	for _, binding := range bindings {
		if binding.ContainerPurpose != purpose || binding.FileMountPath == "" {
			continue
		}
		relativePath := strings.TrimPrefix(binding.FileMountPath, litestreamconfig.SecretMountDir+"/")
		name := binding.SecretKeyRef.Name
		optional := ""
		if binding.SecretKeyRef.Optional != nil {
			optional = fmt.Sprintf("%t", *binding.SecretKeyRef.Optional)
		}
		identity := strings.Join([]string{name, binding.SecretKeyRef.Key, relativePath, optional}, "\x00")
		if _, duplicate := projected[identity]; duplicate {
			continue
		}
		projected[identity] = struct{}{}
		var projectionOptional *bool
		if binding.SecretKeyRef.Optional != nil {
			projectionOptional = ptr.To(*binding.SecretKeyRef.Optional)
		}
		sources = append(sources, corev1.VolumeProjection{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Optional:             projectionOptional,
			Items:                []corev1.KeyToPath{{Key: binding.SecretKeyRef.Key, Path: relativePath}},
		}})
	}

	sort.Slice(sources, func(i, j int) bool {
		a, b := sources[i].Secret, sources[j].Secret
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Items[0].Path != b.Items[0].Path {
			return a.Items[0].Path < b.Items[0].Path
		}
		return a.Items[0].Key < b.Items[0].Key
	})
	return sources
}

// secretVolumeName derives a stable name from the projection itself, so that
// re-running the mutation reuses the volume while a changed projection never
// silently reuses a name the Pod already carries.
func secretVolumeName(sources []corev1.VolumeProjection) string {
	digest := sha256.New()
	for _, source := range sources {
		digest.Write([]byte(source.Secret.Name))
		digest.Write([]byte{0})
		for _, item := range source.Secret.Items {
			digest.Write([]byte(item.Key))
			digest.Write([]byte{0})
			digest.Write([]byte(item.Path))
			digest.Write([]byte{0})
		}
		if source.Secret.Optional != nil {
			_, _ = fmt.Fprintf(digest, "%t", *source.Secret.Optional)
		}
		digest.Write([]byte{0})
	}
	return SecretVolumeNamePrefix + hex.EncodeToString(digest.Sum(nil))[:volumeNameHashLength]
}

// buildContainerSecurityContext hardens the injected containers, keeping
// every setting the resource configures explicitly.
func buildContainerSecurityContext(configured *corev1.SecurityContext) *corev1.SecurityContext {
	securityContext := &corev1.SecurityContext{}
	if configured != nil {
		securityContext = configured.DeepCopy()
	}
	if securityContext.RunAsUser == nil {
		securityContext.RunAsUser = ptr.To(defaultLitestreamUID)
	}
	if securityContext.RunAsGroup == nil {
		securityContext.RunAsGroup = ptr.To(defaultLitestreamGID)
	}
	if securityContext.RunAsNonRoot == nil {
		securityContext.RunAsNonRoot = ptr.To(true)
	}
	if securityContext.ReadOnlyRootFilesystem == nil {
		securityContext.ReadOnlyRootFilesystem = ptr.To(true)
	}
	if securityContext.AllowPrivilegeEscalation == nil {
		securityContext.AllowPrivilegeEscalation = ptr.To(false)
	}
	if securityContext.Capabilities == nil {
		securityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	}
	return securityContext
}

var imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// ValidateImageReference rejects mutable or malformed image references used
// for the controller's default image.
func ValidateImageReference(image string) error {
	separator := strings.LastIndex(image, "@")
	if separator <= 0 || !imageDigestPattern.MatchString(image[separator+1:]) {
		return fmt.Errorf("image %q must use a sha256 digest", image)
	}
	repository := image[:separator]
	if strings.Contains(repository, "@") {
		return fmt.Errorf("image %q contains more than one digest separator", image)
	}
	if imageRepositoryHasTag(repository) {
		tagSeparator := strings.LastIndex(repository, ":")
		if !imageTagPattern.MatchString(repository[tagSeparator+1:]) {
			return fmt.Errorf("image %q contains an invalid tag", image)
		}
	}
	if err := v1alpha1.ValidateImageRepository(imageRepository(repository)); err != nil {
		return fmt.Errorf("image %q contains an invalid repository: %w", image, err)
	}
	return nil
}

func imageRepositoryHasTag(repository string) bool {
	return strings.LastIndex(repository, ":") > strings.LastIndex(repository, "/")
}

func validateDefaultImage(image string) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("no default litestream image is configured")
	}
	return ValidateImageReference(image)
}

// resolveImage applies digest-pinned resource image overrides to the
// controller's digest-pinned default image.
func resolveImage(image v1alpha1.ImageSpec, defaultImage string) (string, error) {
	if image.Repository == "" && image.Tag == "" && image.Digest == "" {
		if err := validateDefaultImage(defaultImage); err != nil {
			return "", fmt.Errorf("cannot inject litestream: invalid default image: %w", err)
		}
		return defaultImage, nil
	}

	if image.Digest == "" {
		return "", fmt.Errorf("cannot inject litestream: image overrides require a sha256 digest")
	}
	if !imageDigestPattern.MatchString(image.Digest) {
		return "", fmt.Errorf("cannot inject litestream: image digest %q is not a valid sha256 digest", image.Digest)
	}
	if image.Tag != "" && !imageTagPattern.MatchString(image.Tag) {
		return "", fmt.Errorf("cannot inject litestream: image tag %q is invalid", image.Tag)
	}

	repository := image.Repository
	if repository == "" {
		if err := validateDefaultImage(defaultImage); err != nil {
			return "", fmt.Errorf("cannot inject litestream: invalid default image: %w", err)
		}
		repository = imageRepository(defaultImage)
	}
	if repository == "" || strings.Contains(repository, "@") || imageRepositoryHasTag(repository) {
		return "", fmt.Errorf("cannot inject litestream: image repository %q must not include a tag or digest", repository)
	}
	if err := v1alpha1.ValidateImageRepository(repository); err != nil {
		return "", fmt.Errorf("cannot inject litestream: image repository %q is an invalid repository: %w", repository, err)
	}
	if image.Tag != "" {
		repository += ":" + image.Tag
	}
	return repository + "@" + image.Digest, nil
}

// imageRepository strips the tag or digest from a fully qualified image
// reference, leaving any registry host and port intact.
func imageRepository(image string) string {
	if digest := strings.Index(image, "@"); digest >= 0 {
		image = image[:digest]
	}
	separator := strings.LastIndex(image, ":")
	if separator > strings.LastIndex(image, "/") {
		image = image[:separator]
	}
	return image
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}
