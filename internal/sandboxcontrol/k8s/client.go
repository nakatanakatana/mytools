package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	OperatingModeRunning   = "Running"
	OperatingModeSuspended = "Suspended"

	AnnotationPodName             = "agents.x-k8s.io/pod-name"
	AnnotationSandboxTemplateRef  = "agents.x-k8s.io/sandbox-template-ref"
	AnnotationAssignedSandboxName = "agents.x-k8s.io/sandbox-name"
	LabelCheckpointSourceSandbox  = "sandbox-control.nakatanakatana.app/sandbox"
	LabelTemplate                 = "sandbox.nakatanakatana.app/template"
)

var (
	SandboxClaimGVR = schema.GroupVersionResource{
		Group:    "extensions.agents.x-k8s.io",
		Version:  "v1beta1",
		Resource: "sandboxclaims",
	}
	SandboxTemplateGVR = schema.GroupVersionResource{
		Group:    "extensions.agents.x-k8s.io",
		Version:  "v1beta1",
		Resource: "sandboxtemplates",
	}
	SandboxWarmPoolGVR = schema.GroupVersionResource{
		Group:    "extensions.agents.x-k8s.io",
		Version:  "v1beta1",
		Resource: "sandboxwarmpools",
	}
	SandboxGVR = schema.GroupVersionResource{
		Group:    "agents.x-k8s.io",
		Version:  "v1beta1",
		Resource: "sandboxes",
	}
	VolumeSnapshotGVR = schema.GroupVersionResource{
		Group:    "snapshot.storage.k8s.io",
		Version:  "v1",
		Resource: "volumesnapshots",
	}
)

// WarmPoolInfo summarizes a SandboxWarmPool resource.
type WarmPoolInfo struct {
	Name            string `json:"name"`
	TemplateName    string `json:"templateName"`
	DesiredReplicas int64  `json:"desiredReplicas"`
	CurrentReplicas int64  `json:"currentReplicas"`
	ReadyReplicas   int64  `json:"readyReplicas"`
}

// TemplateInfo summarizes a SandboxTemplate resource.
type TemplateInfo struct {
	Name       string                     `json:"name"`
	Persistent bool                       `json:"persistent"`
	Raw        *unstructured.Unstructured `json:"-"`
}

// ClaimInfo summarizes a SandboxClaim resource.
type ClaimInfo struct {
	Name         string     `json:"name"`
	WarmPoolName string     `json:"warmPoolName"`
	SandboxName  string     `json:"sandboxName"`
	Ready        bool       `json:"ready"`
	Reason       string     `json:"reason"`
	ShutdownTime *time.Time `json:"shutdownTime,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// SandboxInfo summarizes a Sandbox resource.
type SandboxInfo struct {
	Name                string     `json:"name"`
	TemplateName        string     `json:"templateName"`
	ClaimName           string     `json:"claimName,omitempty"`
	OperatingMode       string     `json:"operatingMode"`
	Phase               string     `json:"phase"`
	Ready               bool       `json:"ready"`
	ReadyReason         string     `json:"readyReason"`
	Suspended           bool       `json:"suspended"`
	PodName             string     `json:"podName,omitempty"`
	PodIPs              []string   `json:"podIPs,omitempty"`
	NodeName            string     `json:"nodeName,omitempty"`
	LabelSelector       string     `json:"-"`
	Persistent          bool       `json:"persistent"`
	WarmPoolStandby     bool       `json:"warmPoolStandby,omitempty"`
	WorkspaceVolumeName string     `json:"workspaceVolumeName,omitempty"`
	ShutdownTime        *time.Time `json:"shutdownTime,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
}

// CheckpointInfo summarizes a VolumeSnapshot resource.
type CheckpointInfo struct {
	Name              string    `json:"name"`
	SourcePVC         string    `json:"sourcePvc"`
	SandboxName       string    `json:"sandboxName,omitempty"`
	SnapshotClassName string    `json:"snapshotClassName"`
	ReadyToUse        bool      `json:"readyToUse"`
	RestoreSize       string    `json:"restoreSize,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
}

// CreateSandboxOptions configures direct Sandbox creation from a SandboxTemplate.
type CreateSandboxOptions struct {
	Name                string
	TemplateName        string
	WorkspaceVolumeName string
	CheckpointName      string
	ShutdownTime        *time.Time
}

// ExecOptions configures a pods/exec session.
type ExecOptions struct {
	PodName       string
	ContainerName string
	Command       []string
	TTY           bool
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	TerminalSize  remotecommand.TerminalSizeQueue
}

// Client abstracts Kubernetes API calls scoped to the sandbox namespace.
type Client interface {
	Namespace() string
	Ping(ctx context.Context) error
	ListWarmPools(ctx context.Context) ([]WarmPoolInfo, error)
	ListTemplates(ctx context.Context) ([]TemplateInfo, error)
	GetTemplate(ctx context.Context, name string) (*TemplateInfo, error)
	ListClaims(ctx context.Context) ([]ClaimInfo, error)
	CreateClaim(ctx context.Context, name, warmPoolName string, shutdownTime *time.Time) error
	DeleteClaim(ctx context.Context, name string) error
	ListSandboxes(ctx context.Context) ([]SandboxInfo, error)
	GetSandbox(ctx context.Context, name string) (*SandboxInfo, error)
	CreateSandboxFromTemplate(ctx context.Context, opts CreateSandboxOptions) error
	SetSandboxOperatingMode(ctx context.Context, name, mode string) error
	DeleteSandbox(ctx context.Context, name string) error
	ListCheckpoints(ctx context.Context) ([]CheckpointInfo, error)
	CreateCheckpoint(ctx context.Context, name, pvcName, snapshotClass, sandboxName string) error
	DeleteCheckpoint(ctx context.Context, name string) error
	ResolvePodName(ctx context.Context, sandbox *SandboxInfo) (string, error)
	GetPodLogs(ctx context.Context, podName, container string, tailLines int64) (string, error)
	StreamPodLogs(ctx context.Context, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error)
	ExecPod(ctx context.Context, opts ExecOptions) error
}

// KubeClient implements Client against a Kubernetes cluster.
type KubeClient struct {
	namespace string
	restCfg   *rest.Config
	core      kubernetes.Interface
	dyn       dynamic.Interface
}

// NewFromKubeconfig builds a KubeClient using kubeconfig (or in-cluster config when empty).
func NewFromKubeconfig(kubeconfig, namespace string) (*KubeClient, error) {
	var (
		cfg *rest.Config
		err error
	)
	if strings.TrimSpace(kubeconfig) != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("load kubernetes config: %w", err)
	}
	coreClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes core client: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes dynamic client: %w", err)
	}
	return NewClient(namespace, cfg, coreClient, dynClient), nil
}

// NewClient constructs a KubeClient from pre-built interfaces (useful for tests).
func NewClient(namespace string, restCfg *rest.Config, core kubernetes.Interface, dyn dynamic.Interface) *KubeClient {
	return &KubeClient{
		namespace: namespace,
		restCfg:   restCfg,
		core:      core,
		dyn:       dyn,
	}
}

func (c *KubeClient) Namespace() string {
	return c.namespace
}

func (c *KubeClient) Ping(ctx context.Context) error {
	_, err := c.dyn.Resource(SandboxWarmPoolGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("kubernetes api ping in namespace %q: %w", c.namespace, err)
	}
	return nil
}

func (c *KubeClient) ListWarmPools(ctx context.Context) ([]WarmPoolInfo, error) {
	list, err := c.dyn.Resource(SandboxWarmPoolGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list sandboxwarmpools: %w", err)
	}
	out := make([]WarmPoolInfo, 0, len(list.Items))
	for _, item := range list.Items {
		tmpl, _, _ := unstructured.NestedString(item.Object, "spec", "sandboxTemplateRef", "name")
		desired, _, _ := unstructured.NestedInt64(item.Object, "spec", "replicas")
		current, _, _ := unstructured.NestedInt64(item.Object, "status", "replicas")
		ready, _, _ := unstructured.NestedInt64(item.Object, "status", "readyReplicas")
		out = append(out, WarmPoolInfo{
			Name:            item.GetName(),
			TemplateName:    tmpl,
			DesiredReplicas: desired,
			CurrentReplicas: current,
			ReadyReplicas:   ready,
		})
	}
	return out, nil
}

func (c *KubeClient) ListTemplates(ctx context.Context) ([]TemplateInfo, error) {
	list, err := c.dyn.Resource(SandboxTemplateGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list sandboxtemplates: %w", err)
	}
	out := make([]TemplateInfo, 0, len(list.Items))
	for i := range list.Items {
		item := list.Items[i].DeepCopy()
		out = append(out, parseTemplate(item))
	}
	return out, nil
}

func (c *KubeClient) GetTemplate(ctx context.Context, name string) (*TemplateInfo, error) {
	item, err := c.dyn.Resource(SandboxTemplateGVR).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get sandboxtemplate %q: %w", name, err)
	}
	info := parseTemplate(item)
	return &info, nil
}

func (c *KubeClient) ListClaims(ctx context.Context) ([]ClaimInfo, error) {
	list, err := c.dyn.Resource(SandboxClaimGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list sandboxclaims: %w", err)
	}
	out := make([]ClaimInfo, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, parseClaim(&item))
	}
	return out, nil
}

func (c *KubeClient) CreateClaim(ctx context.Context, name, warmPoolName string, shutdownTime *time.Time) error {
	spec := map[string]any{
		"warmPoolRef": map[string]any{
			"name": warmPoolName,
		},
	}
	if shutdownTime != nil {
		spec["lifecycle"] = map[string]any{
			"shutdownTime":   shutdownTime.UTC().Format(time.RFC3339),
			"shutdownPolicy": "Delete",
		}
	}
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
			"kind":       "SandboxClaim",
			"metadata": map[string]any{
				"name":      name,
				"namespace": c.namespace,
			},
			"spec": spec,
		},
	}
	_, err := c.dyn.Resource(SandboxClaimGVR).Namespace(c.namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create sandboxclaim %q: %w", name, err)
	}
	return nil
}

func (c *KubeClient) DeleteClaim(ctx context.Context, name string) error {
	err := c.dyn.Resource(SandboxClaimGVR).Namespace(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete sandboxclaim %q: %w", name, err)
	}
	return nil
}

func (c *KubeClient) ListSandboxes(ctx context.Context) ([]SandboxInfo, error) {
	list, err := c.dyn.Resource(SandboxGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	out := make([]SandboxInfo, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, parseSandbox(&item))
	}
	return out, nil
}

func (c *KubeClient) GetSandbox(ctx context.Context, name string) (*SandboxInfo, error) {
	item, err := c.dyn.Resource(SandboxGVR).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get sandbox %q: %w", name, err)
	}
	info := parseSandbox(item)
	return &info, nil
}

func (c *KubeClient) CreateSandboxFromTemplate(ctx context.Context, opts CreateSandboxOptions) error {
	tmpl, err := c.GetTemplate(ctx, opts.TemplateName)
	if err != nil {
		return err
	}
	podTemplate, found, err := unstructured.NestedMap(tmpl.Raw.Object, "spec", "podTemplate")
	if err != nil || !found {
		return fmt.Errorf("sandboxtemplate %q missing spec.podTemplate", opts.TemplateName)
	}

	sandboxSpec := map[string]any{
		"podTemplate":   deepCopyMap(podTemplate),
		"operatingMode": OperatingModeRunning,
	}

	if vcts, ok, _ := unstructured.NestedSlice(tmpl.Raw.Object, "spec", "volumeClaimTemplates"); ok && len(vcts) > 0 {
		copied := deepCopySlice(vcts)
		if opts.CheckpointName != "" {
			injected := false
			for _, rawVCT := range copied {
				vctMap, ok := rawVCT.(map[string]any)
				if !ok {
					continue
				}
				vctName, _, _ := unstructured.NestedString(vctMap, "metadata", "name")
				if opts.WorkspaceVolumeName == "" || vctName == opts.WorkspaceVolumeName {
					if err := unstructured.SetNestedMap(vctMap, map[string]any{
						"apiGroup": "snapshot.storage.k8s.io",
						"kind":     "VolumeSnapshot",
						"name":     opts.CheckpointName,
					}, "spec", "dataSource"); err != nil {
						return fmt.Errorf("set volumeSnapshot dataSource: %w", err)
					}
					injected = true
					break
				}
			}
			if !injected {
				return fmt.Errorf("sandboxtemplate %q has no volumeClaimTemplate named %q for checkpoint fork", opts.TemplateName, opts.WorkspaceVolumeName)
			}
		}
		sandboxSpec["volumeClaimTemplates"] = copied
	} else if opts.CheckpointName != "" {
		return fmt.Errorf("sandboxtemplate %q has no volumeClaimTemplates for checkpoint fork", opts.TemplateName)
	}

	if opts.ShutdownTime != nil {
		sandboxSpec["shutdownTime"] = opts.ShutdownTime.UTC().Format(time.RFC3339)
		sandboxSpec["shutdownPolicy"] = "Delete"
	}

	annotations := map[string]any{
		AnnotationSandboxTemplateRef: opts.TemplateName,
	}
	if opts.CheckpointName != "" {
		annotations["sandbox-control.nakatanakatana.app/forked-from"] = opts.CheckpointName
	}

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "agents.x-k8s.io/v1beta1",
			"kind":       "Sandbox",
			"metadata": map[string]any{
				"name":        opts.Name,
				"namespace":   c.namespace,
				"annotations": annotations,
				"labels": map[string]any{
					LabelTemplate: opts.TemplateName,
				},
			},
			"spec": sandboxSpec,
		},
	}

	_, err = c.dyn.Resource(SandboxGVR).Namespace(c.namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create sandbox %q: %w", opts.Name, err)
	}
	return nil
}

func (c *KubeClient) SetSandboxOperatingMode(ctx context.Context, name, mode string) error {
	payload, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"operatingMode": mode,
		},
	})
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(SandboxGVR).Namespace(c.namespace).Patch(
		ctx,
		name,
		types.MergePatchType,
		payload,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patch sandbox %q operatingMode=%q: %w", name, mode, err)
	}
	return nil
}

func (c *KubeClient) DeleteSandbox(ctx context.Context, name string) error {
	err := c.dyn.Resource(SandboxGVR).Namespace(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete sandbox %q: %w", name, err)
	}
	return nil
}

func (c *KubeClient) ListCheckpoints(ctx context.Context) ([]CheckpointInfo, error) {
	list, err := c.dyn.Resource(VolumeSnapshotGVR).Namespace(c.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list volumesnapshots: %w", err)
	}
	out := make([]CheckpointInfo, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, parseCheckpoint(&item))
	}
	return out, nil
}

func (c *KubeClient) CreateCheckpoint(ctx context.Context, name, pvcName, snapshotClass, sandboxName string) error {
	labels := map[string]any{}
	if sandboxName != "" {
		labels[LabelCheckpointSourceSandbox] = sandboxName
	}
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]any{
				"name":      name,
				"namespace": c.namespace,
				"labels":    labels,
			},
			"spec": map[string]any{
				"volumeSnapshotClassName": snapshotClass,
				"source": map[string]any{
					"persistentVolumeClaimName": pvcName,
				},
			},
		},
	}
	_, err := c.dyn.Resource(VolumeSnapshotGVR).Namespace(c.namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create volumesnapshot %q: %w", name, err)
	}
	return nil
}

func (c *KubeClient) DeleteCheckpoint(ctx context.Context, name string) error {
	err := c.dyn.Resource(VolumeSnapshotGVR).Namespace(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete volumesnapshot %q: %w", name, err)
	}
	return nil
}

func (c *KubeClient) ResolvePodName(ctx context.Context, sandbox *SandboxInfo) (string, error) {
	if sandbox == nil {
		return "", errors.New("sandbox is nil")
	}
	if sandbox.PodName != "" {
		if _, err := c.core.CoreV1().Pods(c.namespace).Get(ctx, sandbox.PodName, metav1.GetOptions{}); err == nil {
			return sandbox.PodName, nil
		}
	}
	if sandbox.LabelSelector != "" {
		pods, err := c.core.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: sandbox.LabelSelector,
		})
		if err == nil && len(pods.Items) > 0 {
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					return p.Name, nil
				}
			}
			return pods.Items[0].Name, nil
		}
	}
	if _, err := c.core.CoreV1().Pods(c.namespace).Get(ctx, sandbox.Name, metav1.GetOptions{}); err == nil {
		return sandbox.Name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get pod %q: %w", sandbox.Name, err)
	}
	return "", fmt.Errorf("no backing pod found for sandbox %q", sandbox.Name)
}

func (c *KubeClient) GetPodLogs(ctx context.Context, podName, container string, tailLines int64) (string, error) {
	rc, err := c.StreamPodLogs(ctx, podName, container, tailLines, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, 2<<20))
	if err != nil {
		return "", fmt.Errorf("read pod logs: %w", err)
	}
	return string(data), nil
}

func (c *KubeClient) StreamPodLogs(ctx context.Context, podName, container string, tailLines int64, follow bool) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Container: container,
		Follow:    follow,
	}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	rc, err := c.core.CoreV1().Pods(c.namespace).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream logs for pod %q: %w", podName, err)
	}
	return rc, nil
}

func (c *KubeClient) ExecPod(ctx context.Context, opts ExecOptions) error {
	if c.restCfg == nil {
		return errors.New("pods/exec requires a REST config")
	}
	req := c.core.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(opts.PodName).
		Namespace(c.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: opts.ContainerName,
			Command:   opts.Command,
			Stdin:     opts.Stdin != nil,
			Stdout:    opts.Stdout != nil,
			Stderr:    opts.Stderr != nil && !opts.TTY,
			TTY:       opts.TTY,
		}, scheme.ParameterCodec)

	spdyExec, err := remotecommand.NewSPDYExecutor(c.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create spdy executor: %w", err)
	}
	executor := spdyExec
	if wsExec, wsErr := remotecommand.NewWebSocketExecutor(c.restCfg, "GET", req.URL().String()); wsErr == nil {
		if fb, fbErr := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
			return err != nil
		}); fbErr == nil {
			executor = fb
		}
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:             opts.Stdin,
		Stdout:            opts.Stdout,
		Stderr:            opts.Stderr,
		Tty:               opts.TTY,
		TerminalSizeQueue: opts.TerminalSize,
	}
	if opts.TTY {
		streamOpts.Stderr = nil
	}
	if err := executor.StreamWithContext(ctx, streamOpts); err != nil {
		return fmt.Errorf("exec in pod %q: %w", opts.PodName, err)
	}
	return nil
}

func parseTemplate(item *unstructured.Unstructured) TemplateInfo {
	vcts, _, _ := unstructured.NestedSlice(item.Object, "spec", "volumeClaimTemplates")
	return TemplateInfo{
		Name:       item.GetName(),
		Persistent: len(vcts) > 0,
		Raw:        item,
	}
}

func parseClaim(item *unstructured.Unstructured) ClaimInfo {
	warmPool, _, _ := unstructured.NestedString(item.Object, "spec", "warmPoolRef", "name")
	sandboxName, _, _ := unstructured.NestedString(item.Object, "status", "sandbox", "name")
	if sandboxName == "" {
		sandboxName = item.GetAnnotations()[AnnotationAssignedSandboxName]
	}
	ready, reason := findCondition(item.Object, "Ready")
	shutdownStr, _, _ := unstructured.NestedString(item.Object, "spec", "lifecycle", "shutdownTime")
	return ClaimInfo{
		Name:         item.GetName(),
		WarmPoolName: warmPool,
		SandboxName:  sandboxName,
		Ready:        ready == "True",
		Reason:       reason,
		ShutdownTime: parseTimePtr(shutdownStr),
		CreatedAt:    item.GetCreationTimestamp().Time,
	}
}

func parseSandbox(item *unstructured.Unstructured) SandboxInfo {
	opMode, _, _ := unstructured.NestedString(item.Object, "spec", "operatingMode")
	if opMode == "" {
		opMode = OperatingModeRunning
	}
	readyStatus, readyReason := findCondition(item.Object, "Ready")
	suspStatus, _ := findCondition(item.Object, "Suspended")
	finStatus, finReason := findCondition(item.Object, "Finished")

	phase := "Starting"
	switch {
	case opMode == OperatingModeSuspended || suspStatus == "True" || readyReason == "SandboxSuspended":
		phase = "Suspended"
	case readyReason == "SandboxExpired":
		phase = "Expired"
	case finStatus == "True" && finReason == "PodFailed":
		phase = "Failed"
	case readyStatus == "True":
		phase = "Running"
	}

	tmplName := item.GetAnnotations()[AnnotationSandboxTemplateRef]
	if tmplName == "" {
		tmplName = item.GetLabels()[LabelTemplate]
	}
	if tmplName == "" {
		tmplName, _, _ = unstructured.NestedString(item.Object, "spec", "podTemplate", "metadata", "labels", LabelTemplate)
	}

	podName := item.GetAnnotations()[AnnotationPodName]
	podIPs, _, _ := unstructured.NestedStringSlice(item.Object, "status", "podIPs")
	nodeName, _, _ := unstructured.NestedString(item.Object, "status", "nodeName")
	selector, _, _ := unstructured.NestedString(item.Object, "status", "selector")
	shutdownStr, _, _ := unstructured.NestedString(item.Object, "spec", "shutdownTime")

	vcts, _, _ := unstructured.NestedSlice(item.Object, "spec", "volumeClaimTemplates")
	wsVolName := ""
	if len(vcts) > 0 {
		if m, ok := vcts[0].(map[string]any); ok {
			wsVolName, _, _ = unstructured.NestedString(m, "metadata", "name")
		}
	}

	warmPoolStandby := false
	for _, ref := range item.GetOwnerReferences() {
		if ref.Kind == "SandboxWarmPool" {
			warmPoolStandby = true
			break
		}
	}

	return SandboxInfo{
		Name:                item.GetName(),
		TemplateName:        tmplName,
		OperatingMode:       opMode,
		Phase:               phase,
		Ready:               readyStatus == "True",
		ReadyReason:         readyReason,
		Suspended:           opMode == OperatingModeSuspended || suspStatus == "True",
		PodName:             podName,
		PodIPs:              podIPs,
		NodeName:            nodeName,
		LabelSelector:       selector,
		Persistent:          len(vcts) > 0,
		WarmPoolStandby:     warmPoolStandby,
		WorkspaceVolumeName: wsVolName,
		ShutdownTime:        parseTimePtr(shutdownStr),
		CreatedAt:           item.GetCreationTimestamp().Time,
	}
}

func parseCheckpoint(item *unstructured.Unstructured) CheckpointInfo {
	pvcName, _, _ := unstructured.NestedString(item.Object, "spec", "source", "persistentVolumeClaimName")
	className, _, _ := unstructured.NestedString(item.Object, "spec", "volumeSnapshotClassName")
	readyToUse, _, _ := unstructured.NestedBool(item.Object, "status", "readyToUse")
	restoreSize, _, _ := unstructured.NestedString(item.Object, "status", "restoreSize")
	sandboxName := item.GetLabels()[LabelCheckpointSourceSandbox]
	if sandboxName == "" && strings.HasPrefix(pvcName, "workspace-") {
		sandboxName = strings.TrimPrefix(pvcName, "workspace-")
	}
	return CheckpointInfo{
		Name:              item.GetName(),
		SourcePVC:         pvcName,
		SandboxName:       sandboxName,
		SnapshotClassName: className,
		ReadyToUse:        readyToUse,
		RestoreSize:       restoreSize,
		CreatedAt:         item.GetCreationTimestamp().Time,
	}
}

func findCondition(obj map[string]any, condType string) (status, reason string) {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t == condType {
			s, _, _ := unstructured.NestedString(m, "status")
			r, _, _ := unstructured.NestedString(m, "reason")
			return s, r
		}
	}
	return "", ""
}

func parseTimePtr(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &t
}

func deepCopyMap(in map[string]any) map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return in
	}
	return out
}

func deepCopySlice(in []any) []any {
	raw, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		return in
	}
	return out
}
