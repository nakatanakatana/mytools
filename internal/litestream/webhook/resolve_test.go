package webhook

import (
	"strings"
	"testing"

	"github.com/nakatanakatana/mytools/api/litestream/v1alpha1"
	"github.com/nakatanakatana/mytools/internal/litestream/litestreamconfig"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveTarget(t *testing.T) {
	for _, test := range []struct {
		name  string
		pod   *corev1.Pod
		input litestreamconfig.Input
		want  ResolvedTarget
		err   string
	}{
		{
			name: "auto detects the only container and longest matching mount",
			pod: podWithContainers(container("app",
				mount("data", "/data"),
				mount("database", "/data/sqlite"),
			)),
			input: inputWithDatabases("/data/sqlite/app.db"),
			want: ResolvedTarget{
				ContainerIndex: 0,
				ContainerName:  "app",
				VolumeName:     "database",
				MountPath:      "/data/sqlite",
			},
		},
		{
			name:  "treats the root mount as containing absolute database paths",
			pod:   podWithContainers(container("app", mount("root", "/"))),
			input: inputWithDatabases("/var/lib/app.db"),
			want: ResolvedTarget{
				ContainerIndex: 0,
				ContainerName:  "app",
				VolumeName:     "root",
				MountPath:      "/",
			},
		},
		{
			name: "pod annotations override injection defaults",
			pod: annotatedPod(
				map[string]string{
					TargetContainerAnnotation: "annotated",
					VolumeAnnotation:          "annotated-volume",
				},
				container("default", mount("default-volume", "/data")),
				container("annotated", mount("annotated-volume", "/data")),
			),
			input: inputWithInjection(
				v1alpha1.InjectionSpec{TargetContainer: "default", Volume: "default-volume"},
				"/data/app.db",
			),
			want: ResolvedTarget{
				ContainerIndex: 1,
				ContainerName:  "annotated",
				VolumeName:     "annotated-volume",
				MountPath:      "/data",
			},
		},
		{
			name: "injection defaults override auto detection",
			pod: podWithContainers(
				container("first", mount("first-volume", "/data")),
				container("configured", mount("configured-volume", "/data")),
			),
			input: inputWithInjection(
				v1alpha1.InjectionSpec{TargetContainer: "configured", Volume: "configured-volume"},
				"/data/app.db",
			),
			want: ResolvedTarget{
				ContainerIndex: 1,
				ContainerName:  "configured",
				VolumeName:     "configured-volume",
				MountPath:      "/data",
			},
		},
		{
			name: "rejects an annotated container that does not exist",
			pod: annotatedPod(
				map[string]string{TargetContainerAnnotation: "missing"},
				container("app", mount("data", "/data")),
			),
			input: inputWithDatabases("/data/app.db"),
			err:   TargetContainerAnnotation,
		},
		{
			name:  "rejects a database outside every mount",
			pod:   podWithContainers(container("app", mount("data", "/data"))),
			input: inputWithDatabases("/var/lib/app.db"),
			err:   "/var/lib/app.db",
		},
		{
			name:  "does not treat a sibling path as inside a mount",
			pod:   podWithContainers(container("app", mount("data", "/data"))),
			input: inputWithDatabases("/data2/app.db"),
			err:   "/data2/app.db",
		},
		{
			name: "rejects ambiguous auto detected mounts",
			pod: podWithContainers(
				container("one", mount("one-data", "/data")),
				container("two", mount("two-data", "/data")),
			),
			input: inputWithDatabases("/data/app.db"),
			err:   "one (volume=one-data, mount=/data)",
		},
		{
			name: "identifies conflicting volume candidates in ambiguity errors",
			pod: podWithContainers(
				container("app", mount("one-data", "/data"), mount("two-data", "/data")),
			),
			input: inputWithDatabases("/data/app.db"),
			err:   "one-data",
		},
		{
			name:  "rejects relative database paths",
			pod:   podWithContainers(container("app", mount("data", "/data"))),
			input: inputWithDatabases("data/app.db"),
			err:   "data/app.db",
		},
		{
			name: "requires every database to be in the selected mount",
			pod:  podWithContainers(container("app", mount("data", "/data"), mount("other", "/other"))),
			input: inputWithDatabases(
				"/data/first.db",
				"/other/second.db",
			),
			err: "/other/second.db",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveTarget(test.pod, test.input)
			if test.err != "" {
				if err == nil {
					t.Fatal("ResolveTarget() error = nil, want an error")
				}
				if !strings.Contains(err.Error(), test.err) {
					t.Fatalf("ResolveTarget() error = %q, want it to contain %q", err, test.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTarget() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ResolveTarget() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func inputWithDatabases(paths ...string) litestreamconfig.Input {
	return inputWithInjection(v1alpha1.InjectionSpec{}, paths...)
}

func inputWithInjection(injection v1alpha1.InjectionSpec, paths ...string) litestreamconfig.Input {
	input := litestreamconfig.Input{Injection: injection}
	for _, databasePath := range paths {
		input.Databases = append(input.Databases, litestreamconfig.Database{Path: databasePath})
	}
	return input
}

func podWithContainers(containers ...corev1.Container) *corev1.Pod {
	return annotatedPod(nil, containers...)
}

func annotatedPod(annotations map[string]string, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

func container(name string, mounts ...corev1.VolumeMount) corev1.Container {
	return corev1.Container{Name: name, VolumeMounts: mounts}
}

func mount(name, mountPath string) corev1.VolumeMount {
	return corev1.VolumeMount{Name: name, MountPath: mountPath}
}
