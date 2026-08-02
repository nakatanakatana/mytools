package webhook

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	corev1 "k8s.io/api/core/v1"
)

const (
	// InjectAnnotation names the Litestream resource to inject into a Pod.
	InjectAnnotation = v1alpha1.Group + "/inject"
	// TargetContainerAnnotation selects the application container for injection.
	TargetContainerAnnotation = v1alpha1.Group + "/target-container"
	// VolumeAnnotation selects the mounted volume holding every database.
	VolumeAnnotation = v1alpha1.Group + "/volume"
)

// ResolvedTarget identifies the application container and volume that hold the
// databases managed by a Litestream resource.
type ResolvedTarget struct {
	ContainerIndex int
	ContainerName  string
	VolumeName     string
	MountPath      string
}

// ResolveTarget resolves a Litestream resource's databases to exactly one
// mounted Pod volume. Pod annotations override injection defaults, which
// override automatic detection.
func ResolveTarget(pod *corev1.Pod, input litestreamconfig.Input) (ResolvedTarget, error) {
	if pod == nil {
		return ResolvedTarget{}, fmt.Errorf("cannot resolve target: pod is required")
	}

	databasePaths, err := cleanDatabasePaths(input.Databases)
	if err != nil {
		return ResolvedTarget{}, err
	}

	annotations := pod.GetAnnotations()
	containerName, containerConfigured := annotations[TargetContainerAnnotation]
	if !containerConfigured {
		containerName = input.Injection.TargetContainer
		containerConfigured = containerName != ""
	}
	volumeName, volumeConfigured := annotations[VolumeAnnotation]
	if !volumeConfigured {
		volumeName = input.Injection.Volume
		volumeConfigured = volumeName != ""
	}

	var candidates []ResolvedTarget
	for i, container := range pod.Spec.Containers {
		if containerConfigured && container.Name != containerName {
			continue
		}
		for _, volumeMount := range container.VolumeMounts {
			if volumeConfigured && volumeMount.Name != volumeName {
				continue
			}
			mountPath := path.Clean(volumeMount.MountPath)
			if !path.IsAbs(mountPath) || !containsAll(mountPath, databasePaths) {
				continue
			}
			candidates = append(candidates, ResolvedTarget{
				ContainerIndex: i,
				ContainerName:  container.Name,
				VolumeName:     volumeMount.Name,
				MountPath:      mountPath,
			})
		}
	}

	if containerConfigured && !hasContainer(pod.Spec.Containers, containerName) {
		return ResolvedTarget{}, fmt.Errorf("annotation %q selects container %q, but it does not exist", TargetContainerAnnotation, containerName)
	}
	if len(candidates) == 0 {
		return ResolvedTarget{}, fmt.Errorf(
			"no mount contains all database paths %s; set annotations %q and %q to select a valid target",
			strings.Join(databasePaths, ", "),
			TargetContainerAnnotation,
			VolumeAnnotation,
		)
	}

	candidates = mostSpecific(candidates)
	if len(candidates) != 1 {
		return ResolvedTarget{}, fmt.Errorf(
			"ambiguous target candidates %s; set annotations %q and %q",
			formatCandidates(candidates),
			TargetContainerAnnotation,
			VolumeAnnotation,
		)
	}
	return candidates[0], nil
}

func cleanDatabasePaths(databases []litestreamconfig.Database) ([]string, error) {
	paths := make([]string, 0, len(databases))
	for _, database := range databases {
		if !path.IsAbs(database.Path) {
			return nil, fmt.Errorf("database path %q must be absolute", database.Path)
		}
		paths = append(paths, path.Clean(database.Path))
	}
	return paths, nil
}

func containsAll(mountPath string, databasePaths []string) bool {
	for _, databasePath := range databasePaths {
		if !pathContains(mountPath, databasePath) {
			return false
		}
	}
	return true
}

func pathContains(parent, child string) bool {
	return parent == "/" || child == parent || strings.HasPrefix(child, parent+"/")
}

func hasContainer(containers []corev1.Container, name string) bool {
	return findContainer(containers, name) != nil
}

func mostSpecific(candidates []ResolvedTarget) []ResolvedTarget {
	longestPath := 0
	for _, candidate := range candidates {
		if len(candidate.MountPath) > longestPath {
			longestPath = len(candidate.MountPath)
		}
	}

	var mostSpecific []ResolvedTarget
	for _, candidate := range candidates {
		if len(candidate.MountPath) == longestPath {
			mostSpecific = append(mostSpecific, candidate)
		}
	}
	return mostSpecific
}

func formatCandidates(candidates []ResolvedTarget) string {
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, fmt.Sprintf(
			"%s (volume=%s, mount=%s)",
			candidate.ContainerName,
			candidate.VolumeName,
			candidate.MountPath,
		))
	}
	sort.Strings(values)
	return strings.Join(values, ", ")
}
