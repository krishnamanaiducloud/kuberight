package httpapi

import (
	"context"
	"errors"

	"github.com/kuberight/kuberight/internal/optimizer"
)

var (
	// ErrWorkloadNotFound is returned when the inventory has no such workload.
	ErrWorkloadNotFound = errors.New("workload was not found")
	// ErrWorkloadOutOfScope is returned for a namespace the inventory
	// provider is not permitted to read.
	ErrWorkloadOutOfScope = errors.New("workload is outside the configured inventory scope")
)

// ContainerSpec is a container's configured resources. Zero means unset.
type ContainerSpec struct {
	Name               string `json:"name"`
	CPURequestMilli    int64  `json:"cpuRequestMilli"`
	CPULimitMilli      int64  `json:"cpuLimitMilli"`
	MemoryRequestBytes int64  `json:"memoryRequestBytes"`
	MemoryLimitBytes   int64  `json:"memoryLimitBytes"`
	VPAControlsCPU     bool   `json:"vpaControlsCpu"`
	VPAControlsMemory  bool   `json:"vpaControlsMemory"`
}

// WorkloadSpec is the live configuration needed to generate a recommendation.
type WorkloadSpec struct {
	Cluster               string          `json:"cluster"`
	Namespace             string          `json:"namespace"`
	Kind                  string          `json:"kind"`
	Name                  string          `json:"name"`
	UID                   string          `json:"uid,omitempty"`
	Generation            int64           `json:"generation,omitempty"`
	ResourceVersion       string          `json:"resourceVersion,omitempty"`
	SourceSpecFingerprint string          `json:"sourceSpecFingerprint,omitempty"`
	Replicas              int32           `json:"replicas"`
	Containers            []ContainerSpec `json:"containers"`
	HPAUtilizationBased   bool            `json:"hpaUtilizationBased"`
	KEDAEnabled           bool            `json:"kedaEnabled"`
	Warnings              []string        `json:"warnings"`
	// InitContainers include native sidecars (restartPolicy Always), which
	// run beside the application containers and can be right-sized.
	InitContainers []InitContainerSpec `json:"initContainers,omitempty"`
	// HPA is the CPU-utilization HorizontalPodAutoscaler, when one exists.
	HPA *HPASpec `json:"hpa,omitempty"`
	// KEDATriggers are the trigger types of a KEDA ScaledObject.
	KEDATriggers        []string `json:"kedaTriggers,omitempty"`
	OverheadCPUMilli    int64    `json:"overheadCpuMilli,omitempty"`
	OverheadMemoryBytes int64    `json:"overheadMemoryBytes,omitempty"`
}

// InitContainerSpec is an init container's configured resources.
type InitContainerSpec struct {
	ContainerSpec
	Sidecar bool `json:"sidecar,omitempty"`
}

// HPASpec is a HorizontalPodAutoscaler that scales on CPU utilization.
type HPASpec struct {
	Name                 string `json:"name"`
	MinReplicas          int32  `json:"minReplicas"`
	MaxReplicas          int32  `json:"maxReplicas"`
	TargetCPUUtilization int32  `json:"targetCpuUtilization"`
	CurrentReplicas      int32  `json:"currentReplicas"`
	// Container is set when the HPA targets one container's utilization.
	Container string `json:"container,omitempty"`
}

// Container returns the named application container or native sidecar,
// the long-running containers that can be right-sized.
func (spec WorkloadSpec) Container(name string) (ContainerSpec, bool) {
	for _, container := range spec.Containers {
		if container.Name == name {
			return container, true
		}
	}
	for _, init := range spec.InitContainers {
		if init.Sidecar && init.Name == name {
			return init.ContainerSpec, true
		}
	}
	return ContainerSpec{}, false
}

// IsInitContainer reports whether name is a run-to-completion init
// container, which has too little usage history to size safely.
func (spec WorkloadSpec) IsInitContainer(name string) bool {
	for _, init := range spec.InitContainers {
		if !init.Sidecar && init.Name == name {
			return true
		}
	}
	return false
}

// NamespacePolicyReader reads a namespace's LimitRanges and ResourceQuotas.
type NamespacePolicyReader interface {
	NamespacePolicy(ctx context.Context, namespace string) (optimizer.NamespacePolicy, error)
}

func toResources(container ContainerSpec) optimizer.ContainerResources {
	return optimizer.ContainerResources{
		Name: container.Name, CPURequestMilli: container.CPURequestMilli, CPULimitMilli: container.CPULimitMilli,
		MemoryRequestBytes: container.MemoryRequestBytes, MemoryLimitBytes: container.MemoryLimitBytes,
	}
}

// podContext describes the workload's pod template for the optimizer.
func (spec WorkloadSpec) podContext() *optimizer.PodContext {
	pod := &optimizer.PodContext{OverheadCPUMilli: spec.OverheadCPUMilli, OverheadMemoryBytes: spec.OverheadMemoryBytes}
	for _, container := range spec.Containers {
		pod.Containers = append(pod.Containers, toResources(container))
	}
	for _, init := range spec.InitContainers {
		pod.InitContainers = append(pod.InitContainers, optimizer.InitContainer{ContainerResources: toResources(init.ContainerSpec), Sidecar: init.Sidecar})
	}
	return pod
}

// hpaContext describes the workload's HPA for the optimizer, or nil.
func (spec WorkloadSpec) hpaContext(container string) *optimizer.HPAContext {
	if spec.HPA == nil || spec.HPA.TargetCPUUtilization <= 0 {
		return nil
	}
	if spec.HPA.Container != "" && spec.HPA.Container != container {
		return nil
	}
	return &optimizer.HPAContext{
		MinReplicas: spec.HPA.MinReplicas, MaxReplicas: spec.HPA.MaxReplicas, TargetCPUUtilization: spec.HPA.TargetCPUUtilization,
		CurrentReplicas: spec.HPA.CurrentReplicas, ContainerMetric: spec.HPA.Container != "",
	}
}

// WorkloadResolver is implemented by inventory providers that can read one
// workload's live configuration.
type WorkloadResolver interface {
	Workload(ctx context.Context, namespace, kind, name string) (WorkloadSpec, error)
}
