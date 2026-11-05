// Package automation holds the shared, side-effect-free parts of continuous
// rightsizing: opt-in labels, GitOps ownership detection, change windows, and
// the strategic-merge patch that applies an approved recommendation. Every
// write is opt-in: operationMode=apply, a namespace label, and an approved,
// fresh recommendation whose evidence still matches the live workload.
package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// NamespaceLabel must be "enabled" on a namespace before any workload in
	// it is changed.
	NamespaceLabel = "kuberight.io/automation"
	// WorkloadAnnotation set to "disabled" opts one workload out.
	WorkloadAnnotation = "kuberight.io/automation"
	// GitOpsAnnotation set to "allow" permits changes to a GitOps-managed
	// workload. Without it the GitOps controller would revert the change.
	GitOpsAnnotation = "kuberight.io/automation-gitops"
	// AppliedAnnotation records the recommendation an automated change
	// applied; PreviousAnnotation records the resources it replaced, so a
	// regression can be rolled back exactly.
	AppliedAnnotation  = "kuberight.io/applied-recommendation"
	PreviousAnnotation = "kuberight.io/previous-resources"
	// FieldManager owns the resource fields KubeRight writes.
	FieldManager = "kuberight-automation"
)

// Supported reports whether automated changes are implemented for a kind.
// Jobs and CronJobs run to completion and unmanaged ReplicaSets do not roll
// out template changes, so they stay advisory.
func Supported(kind string) bool {
	return kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet"
}

// Resources are one container's requests and limits. A zero limit means no
// limit.
type Resources struct {
	CPURequestMilli    int64 `json:"cpuRequestMilli"`
	CPULimitMilli      int64 `json:"cpuLimitMilli"`
	MemoryRequestBytes int64 `json:"memoryRequestBytes"`
	MemoryLimitBytes   int64 `json:"memoryLimitBytes"`
}

// Change is one container update with an optimistic-concurrency
// precondition on the workload's resourceVersion.
type Change struct {
	Namespace        string    `json:"namespace"`
	Kind             string    `json:"kind"`
	Workload         string    `json:"workload"`
	Container        string    `json:"container"`
	ResourceVersion  string    `json:"resourceVersion"`
	RecommendationID string    `json:"recommendationId"`
	Desired          Resources `json:"desired"`
	Previous         Resources `json:"previous"`
	Rollback         bool      `json:"rollback,omitempty"`
}

// GitOpsManager names the GitOps controller that owns a workload, or "".
func GitOpsManager(labels, annotations map[string]string) string {
	switch {
	case annotations["argocd.argoproj.io/tracking-id"] != "" || labels["argocd.argoproj.io/instance"] != "":
		return "Argo CD"
	case labels["kustomize.toolkit.fluxcd.io/name"] != "" || labels["helm.toolkit.fluxcd.io/name"] != "":
		return "Flux"
	}
	return ""
}

func cpuQuantity(milli int64) string {
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	return strconv.FormatInt(milli, 10) + "m"
}

func memoryQuantity(bytes int64) string {
	if bytes%(1<<20) == 0 {
		return strconv.FormatInt(bytes>>20, 10) + "Mi"
	}
	return strconv.FormatInt(bytes, 10)
}

func quantities(cpu, memory int64) map[string]any {
	values := map[string]any{"cpu": nil, "memory": nil}
	if cpu > 0 {
		values["cpu"] = cpuQuantity(cpu)
	}
	if memory > 0 {
		values["memory"] = memoryQuantity(memory)
	}
	return values
}

// PatchBody renders a strategic-merge patch for the change. Containers merge
// by name, so other containers are untouched; a null quantity removes that
// request or limit. The resourceVersion makes the API server reject the
// patch if the workload changed after it was evaluated.
func PatchBody(change Change) ([]byte, error) {
	if change.Namespace == "" || change.Workload == "" || change.Container == "" || change.ResourceVersion == "" || change.RecommendationID == "" {
		return nil, errors.New("an automated change needs a namespace, workload, container, resourceVersion, and recommendation")
	}
	if change.Desired.CPURequestMilli <= 0 || change.Desired.MemoryRequestBytes <= 0 {
		return nil, errors.New("an automated change must set CPU and memory requests")
	}
	if (change.Desired.CPULimitMilli > 0 && change.Desired.CPULimitMilli < change.Desired.CPURequestMilli) ||
		(change.Desired.MemoryLimitBytes > 0 && change.Desired.MemoryLimitBytes < change.Desired.MemoryRequestBytes) {
		return nil, errors.New("limits cannot be lower than requests")
	}
	previous, err := json.Marshal(change.Previous)
	if err != nil {
		return nil, err
	}
	resources := map[string]any{
		"requests": quantities(change.Desired.CPURequestMilli, change.Desired.MemoryRequestBytes),
		"limits":   quantities(change.Desired.CPULimitMilli, change.Desired.MemoryLimitBytes),
	}
	patch := map[string]any{
		"metadata": map[string]any{
			"resourceVersion": change.ResourceVersion,
			"annotations":     map[string]any{AppliedAnnotation: change.RecommendationID, PreviousAnnotation: string(previous)},
		},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": change.Container, "resources": resources}},
		}}},
	}
	return json.Marshal(patch)
}

// Window is a daily UTC change window. A zero Window always allows changes.
type Window struct {
	Start, End time.Duration
	set        bool
}

// ParseWindow reads "HH:MM-HH:MM" in UTC; the end may be before the start
// for a window that crosses midnight. An empty value is always open.
func ParseWindow(value string) (Window, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Window{}, nil
	}
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return Window{}, fmt.Errorf("change window %q must be HH:MM-HH:MM in UTC", value)
	}
	var bounds [2]time.Duration
	for index, part := range parts {
		at, err := time.Parse("15:04", strings.TrimSpace(part))
		if err != nil {
			return Window{}, fmt.Errorf("change window %q must be HH:MM-HH:MM in UTC", value)
		}
		bounds[index] = time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute
	}
	if bounds[0] == bounds[1] {
		return Window{}, fmt.Errorf("change window %q is empty", value)
	}
	return Window{Start: bounds[0], End: bounds[1], set: true}, nil
}

// Open reports whether changes are allowed at the instant.
func (window Window) Open(at time.Time) bool {
	if !window.set {
		return true
	}
	at = at.UTC()
	offset := time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute
	if window.Start < window.End {
		return offset >= window.Start && offset < window.End
	}
	return offset >= window.Start || offset < window.End
}

// String renders the window for status responses.
func (window Window) String() string {
	if !window.set {
		return ""
	}
	format := func(value time.Duration) string {
		return fmt.Sprintf("%02d:%02d", int(value.Hours()), int(value.Minutes())%60)
	}
	return format(window.Start) + "-" + format(window.End) + " UTC"
}
