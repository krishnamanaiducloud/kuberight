package httpapi

import (
	"context"
	"errors"
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
	Cluster             string          `json:"cluster"`
	Namespace           string          `json:"namespace"`
	Kind                string          `json:"kind"`
	Name                string          `json:"name"`
	Replicas            int32           `json:"replicas"`
	Containers          []ContainerSpec `json:"containers"`
	HPAUtilizationBased bool            `json:"hpaUtilizationBased"`
	KEDAEnabled         bool            `json:"kedaEnabled"`
	Warnings            []string        `json:"warnings"`
}

// Container returns the named container.
func (spec WorkloadSpec) Container(name string) (ContainerSpec, bool) {
	for _, container := range spec.Containers {
		if container.Name == name {
			return container, true
		}
	}
	return ContainerSpec{}, false
}

// WorkloadResolver is implemented by inventory providers that can read one
// workload's live configuration.
type WorkloadResolver interface {
	Workload(ctx context.Context, namespace, kind, name string) (WorkloadSpec, error)
}
