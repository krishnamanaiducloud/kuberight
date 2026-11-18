package optimizer

import "time"

// ContainerResources are one container's current requests and limits, in
// millicores and bytes. Zero means unset.
type ContainerResources struct {
	Name               string `json:"name"`
	CPURequestMilli    int64  `json:"cpuRequestMilli"`
	CPULimitMilli      int64  `json:"cpuLimitMilli"`
	MemoryRequestBytes int64  `json:"memoryRequestBytes"`
	MemoryLimitBytes   int64  `json:"memoryLimitBytes"`
}

// InitContainer is an init container. A native sidecar (restartPolicy
// Always) keeps running beside the application containers.
type InitContainer struct {
	ContainerResources
	Sidecar bool `json:"sidecar,omitempty"`
}

// PodContext is the pod template the recommended container belongs to. It
// enables QoS protection, pod-level LimitRange checks, scheduling checks,
// and workload aggregation.
type PodContext struct {
	Containers     []ContainerResources `json:"containers"`
	InitContainers []InitContainer      `json:"initContainers,omitempty"`
	// OverheadCPUMilli and OverheadMemoryBytes come from the RuntimeClass.
	OverheadCPUMilli    int64 `json:"overheadCpuMilli,omitempty"`
	OverheadMemoryBytes int64 `json:"overheadMemoryBytes,omitempty"`
}

// LimitRange bounds for containers (Type "Container") or whole pods ("Pod").
// Zero means unbounded.
type LimitRange struct {
	Name                       string  `json:"name"`
	Type                       string  `json:"type"`
	MinCPUMilli                int64   `json:"minCpuMilli,omitempty"`
	MaxCPUMilli                int64   `json:"maxCpuMilli,omitempty"`
	MinMemoryBytes             int64   `json:"minMemoryBytes,omitempty"`
	MaxMemoryBytes             int64   `json:"maxMemoryBytes,omitempty"`
	MaxCPULimitRequestRatio    float64 `json:"maxCpuLimitRequestRatio,omitempty"`
	MaxMemoryLimitRequestRatio float64 `json:"maxMemoryLimitRequestRatio,omitempty"`
}

// QuotaAmounts are ResourceQuota values; -1 means the quota does not
// constrain that resource.
type QuotaAmounts struct {
	RequestsCPUMilli    int64 `json:"requestsCpuMilli"`
	RequestsMemoryBytes int64 `json:"requestsMemoryBytes"`
	LimitsCPUMilli      int64 `json:"limitsCpuMilli"`
	LimitsMemoryBytes   int64 `json:"limitsMemoryBytes"`
}

// ResourceQuota is a namespace quota with its current usage.
type ResourceQuota struct {
	Name string       `json:"name"`
	Hard QuotaAmounts `json:"hard"`
	Used QuotaAmounts `json:"used"`
}

// NamespacePolicy is the namespace's admission constraints.
type NamespacePolicy struct {
	LimitRanges []LimitRange    `json:"limitRanges,omitempty"`
	Quotas      []ResourceQuota `json:"quotas,omitempty"`
}

// NodeShape is a schedulable node's allocatable capacity.
type NodeShape struct {
	Name        string `json:"name"`
	CPUMilli    int64  `json:"cpuMilli"`
	MemoryBytes int64  `json:"memoryBytes"`
}

// HPAContext is a HorizontalPodAutoscaler that scales on CPU utilization.
type HPAContext struct {
	MinReplicas int32 `json:"minReplicas"`
	MaxReplicas int32 `json:"maxReplicas"`
	// TargetCPUUtilization is the target average utilization in percent.
	TargetCPUUtilization int32 `json:"targetCpuUtilization"`
	CurrentReplicas      int32 `json:"currentReplicas"`
	// ContainerMetric is true when the HPA targets this container's
	// utilization (ContainerResource) rather than the whole pod's.
	ContainerMetric bool `json:"containerMetric,omitempty"`
}

// ForecastContext is the ML forecast for this container over the policy's
// trend horizon, with its calibrated prediction interval.
type ForecastContext struct {
	Horizon           time.Duration `json:"horizon"`
	CPUPeakMilli      float64       `json:"cpuPeakMilli"`
	CPUUpperMilli     float64       `json:"cpuUpperMilli"`
	MemoryPeakBytes   float64       `json:"memoryPeakBytes"`
	MemoryUpperBytes  float64       `json:"memoryUpperBytes"`
	IntervalLevel     float64       `json:"intervalLevel"`
	EmpiricalCoverage *float64      `json:"empiricalCoverage,omitempty"`
	Model             string        `json:"model,omitempty"`
}
