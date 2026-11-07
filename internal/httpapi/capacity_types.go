package httpapi

import "context"

// NodeCapacity is one node's schedulable resources.
type NodeCapacity struct {
	Name         string `json:"name"`
	CPUMilli     int64  `json:"cpuMilli"`
	MemoryBytes  int64  `json:"memoryBytes"`
	Schedulable  bool   `json:"schedulable"`
	InstanceType string `json:"instanceType,omitempty"`
	CapacityType string `json:"capacityType,omitempty"`
	NodePool     string `json:"nodePool,omitempty"`
}

// NodePool summarizes a Karpenter NodePool's consolidation behavior.
type NodePool struct {
	Name                string   `json:"name"`
	ConsolidationPolicy string   `json:"consolidationPolicy"`
	ConsolidateAfter    string   `json:"consolidateAfter,omitempty"`
	DisruptionBudgets   []string `json:"disruptionBudgets"`
	CPULimit            string   `json:"cpuLimit,omitempty"`
	MemoryLimit         string   `json:"memoryLimit,omitempty"`
}

// NodeAutoscaling describes the node autoscalers visible in the cluster.
// Managed autoscalers that run outside the cluster are not visible.
type NodeAutoscaling struct {
	Karpenter         bool       `json:"karpenter"`
	NodePools         []NodePool `json:"nodePools"`
	ClusterAutoscaler bool       `json:"clusterAutoscaler"`
	Warnings          []string   `json:"warnings"`
}

// ClusterCapacity is the input to node-impact simulation.
type ClusterCapacity struct {
	Nodes       []NodeCapacity  `json:"nodes"`
	Autoscaling NodeAutoscaling `json:"autoscaling"`
}

// CapacityReader is implemented by inventory providers that can read nodes.
type CapacityReader interface {
	Capacity(ctx context.Context) (ClusterCapacity, error)
}
