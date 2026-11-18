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
	// NodeGroup is the group that provisions the node: a Karpenter NodePool,
	// an EKS, GKE, or AKS node group, an OpenShift or Cluster API MachineSet,
	// or, without any of these, a shape group ("shape:<instance type>").
	NodeGroup         string `json:"nodeGroup"`
	NodeGroupProvider string `json:"nodeGroupProvider"`
	// NodeGroupMembership is MembershipVerified or MembershipInferred, and
	// NodeGroupEvidence says which label, annotation, or object decided it.
	NodeGroupMembership string `json:"nodeGroupMembership"`
	NodeGroupEvidence   string `json:"nodeGroupEvidence"`
	// Labels and Taints decide which pods the node accepts.
	Labels map[string]string `json:"-"`
	Taints []NodeTaint       `json:"taints,omitempty"`
}

// Node-group providers.
const (
	NodeGroupKarpenter  = "karpenter"
	NodeGroupEKS        = "eks"
	NodeGroupGKE        = "gke"
	NodeGroupAKS        = "aks"
	NodeGroupOpenShift  = "openshift"
	NodeGroupClusterAPI = "cluster-api"
	NodeGroupNone       = "none"
)

// Node-group membership evidence.
const (
	// MembershipVerified comes from the platform's own node-group label or
	// annotation, or from the node's Machine object.
	MembershipVerified = "verified"
	// MembershipInferred comes from a Machine name prefix or the node shape.
	MembershipInferred = "inferred"
)

// NodeGroupImpact is one node group's share of the node-impact estimate.
type NodeGroupImpact struct {
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	Membership       string   `json:"membership"`
	Evidence         string   `json:"evidence"`
	Nodes            int      `json:"nodes"`
	SchedulableNodes int      `json:"schedulableNodes"`
	InstanceTypes    []string `json:"instanceTypes"`
	CapacityTypes    []string `json:"capacityTypes"`
	CPUMilli         int64    `json:"cpuMilli"`
	MemoryBytes      int64    `json:"memoryBytes"`
	// CurrentNodesNeeded and RecommendedNodesNeeded count this group's nodes
	// that receive at least one pod in each packing.
	CurrentNodesNeeded     int `json:"currentNodesNeeded"`
	RecommendedNodesNeeded int `json:"recommendedNodesNeeded"`
	ReclaimableNodes       int `json:"reclaimableNodes"`
	// Limit is the group's autoscaling size range, when one was read.
	// LimitedByMinimum is set when the minimum size keeps nodes the packing
	// would free, and ExceedsMaximum when the recommended packing needs more
	// nodes than the group may have.
	Limit            *NodeGroupLimit `json:"limit,omitempty"`
	LimitedByMinimum bool            `json:"limitedByMinimum,omitempty"`
	ExceedsMaximum   bool            `json:"exceedsMaximum,omitempty"`
	// Realization explains how, or whether, this group shrinks.
	Realization string `json:"realization"`
}

// NodeGroupLimit is a node group's autoscaling size range and where it was
// read. Membership is MembershipVerified for an exact reference and
// MembershipInferred for a provider naming convention.
type NodeGroupLimit struct {
	MinSize    int    `json:"minSize"`
	MaxSize    int    `json:"maxSize"`
	Source     string `json:"source"`
	Membership string `json:"membership"`
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
	// Constraints are pod-template placement rules by "namespace/kind/name".
	// A workload without an entry can use any schedulable node.
	Constraints map[string]SchedulingConstraints `json:"-"`
	// Pinned lists, by "namespace/kind/name", the nodes of pods that a
	// PodDisruptionBudget allowing no voluntary evictions keeps in place.
	Pinned map[string][]string `json:"-"`
	// EvictionBlockers describes each such PodDisruptionBudget.
	EvictionBlockers []string `json:"-"`
	// GroupLimits are node-group size limits by nodeGroupKey.
	GroupLimits map[string]NodeGroupLimit `json:"-"`
}

// CapacityReader is implemented by inventory providers that can read nodes.
type CapacityReader interface {
	Capacity(ctx context.Context) (ClusterCapacity, error)
}
