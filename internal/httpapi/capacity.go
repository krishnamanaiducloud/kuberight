package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

type podDemand struct {
	cpuMilli, memoryBytes int64
	// workload names the owner for unplaceable-pod reporting; constraints
	// restrict which nodes the pod may use.
	workload    string
	constraints SchedulingConstraints
}

type packingResult struct {
	NodesNeeded    int   `json:"nodesNeeded"`
	UnplacedPods   int   `json:"unplacedPods"`
	CPURequested   int64 `json:"cpuRequestedMilli"`
	MemoryRequests int64 `json:"memoryRequestedBytes"`
	// NoEligibleNodePods could not use any schedulable node because of
	// taints, nodeSelector, or required node affinity, whatever their size.
	NoEligibleNodePods int `json:"noEligibleNodePods"`
	// UnplacedWorkloads names up to 20 workloads with unplaced pods.
	UnplacedWorkloads []string `json:"unplacedWorkloads"`
	// nodesByGroup counts used nodes by nodeGroupKey.
	nodesByGroup map[string]int
}

func nodeGroupKey(node NodeCapacity) string { return node.NodeGroupProvider + "\x00" + node.NodeGroup }

// pack places pods with first-fit-decreasing onto nodes ordered largest
// first, after reserving DaemonSet pods on every node. A pod only uses nodes
// whose taints it tolerates and whose labels satisfy its nodeSelector and
// required node affinity. It returns how many nodes receive at least one
// pod. Preferred and pod (anti-)affinity, topology spread, and PDBs are not
// modeled, so the result is an estimate, not a scheduler simulation.
func pack(nodes []NodeCapacity, daemon podDemand, pods []podDemand) packingResult {
	type bin struct {
		cpu, memory int64
		node        NodeCapacity
	}
	bins := make([]bin, 0, len(nodes))
	var maxCPU, maxMemory int64 = 1, 1
	for _, node := range nodes {
		if !node.Schedulable {
			continue
		}
		bins = append(bins, bin{node.CPUMilli - daemon.cpuMilli, node.MemoryBytes - daemon.memoryBytes, node})
		maxCPU, maxMemory = max(maxCPU, node.CPUMilli), max(maxMemory, node.MemoryBytes)
	}
	sort.SliceStable(bins, func(left, right int) bool {
		return float64(bins[left].cpu)/float64(maxCPU)+float64(bins[left].memory)/float64(maxMemory) >
			float64(bins[right].cpu)/float64(maxCPU)+float64(bins[right].memory)/float64(maxMemory)
	})
	ordered := append([]podDemand(nil), pods...)
	dominant := func(pod podDemand) float64 {
		return max(float64(pod.cpuMilli)/float64(maxCPU), float64(pod.memoryBytes)/float64(maxMemory))
	}
	sort.SliceStable(ordered, func(left, right int) bool { return dominant(ordered[left]) > dominant(ordered[right]) })
	result := packingResult{UnplacedWorkloads: []string{}, nodesByGroup: map[string]int{}}
	used := make([]bool, len(bins))
	unplaced := map[string]bool{}
	for _, pod := range ordered {
		result.CPURequested += pod.cpuMilli
		result.MemoryRequests += pod.memoryBytes
		placed, eligible := false, false
		for index := range bins {
			if !pod.constraints.allows(bins[index].node) {
				continue
			}
			eligible = true
			if bins[index].cpu >= pod.cpuMilli && bins[index].memory >= pod.memoryBytes {
				bins[index].cpu -= pod.cpuMilli
				bins[index].memory -= pod.memoryBytes
				if !used[index] {
					used[index] = true
					result.NodesNeeded++
					result.nodesByGroup[nodeGroupKey(bins[index].node)]++
				}
				placed = true
				break
			}
		}
		if !placed {
			result.UnplacedPods++
			if !eligible {
				result.NoEligibleNodePods++
			}
			if pod.workload != "" && !unplaced[pod.workload] && len(result.UnplacedWorkloads) < 20 {
				unplaced[pod.workload] = true
				result.UnplacedWorkloads = append(result.UnplacedWorkloads, pod.workload)
			}
		}
	}
	return result
}

func recommendationKey(cluster, namespace, kind, workload, container string) string {
	return strings.Join([]string{cluster, namespace, kind, workload, container}, "\x00")
}

// latestRecommendations returns the newest non-rejected recommendation for
// every container.
func (server *apiServer) latestRecommendations(ctx context.Context) (map[string]repository.RecommendationRecord, error) {
	latest := map[string]repository.RecommendationRecord{}
	for page := 1; page <= maxSavingsRecords/savingsPageSize; page++ {
		result, err := server.repository.ListRecommendations(ctx, repository.ListOptions{Page: page, PageSize: savingsPageSize, LatestOnly: true, Sort: "createdAt", Order: "desc"})
		if err != nil {
			return nil, err
		}
		for _, record := range result.Items {
			if record.ReviewStatus != repository.ReviewRejected {
				latest[recommendationKey(record.Cluster, record.Namespace, record.Kind, record.Workload, record.Container)] = record
			}
		}
		if page*savingsPageSize >= result.Total {
			break
		}
	}
	return latest, nil
}

// capacityAdviceMatches refuses to simulate speculative, stale, or
// cross-cluster advice. Capacity results can influence node removals, so they
// require the same live-object binding used by approval.
func capacityAdviceMatches(advice repository.RecommendationRecord, workload, container map[string]any, now time.Time) bool {
	provenance := advice.Provenance
	if advice.Source != repository.SourcePrometheus || advice.ReviewStatus != repository.ReviewApproved ||
		provenance.ExpiresAt == nil || advice.Expired(now) || provenance.WorkloadUID == "" ||
		provenance.WorkloadGeneration <= 0 || provenance.SourceSpecFingerprint == "" ||
		provenance.ObservationStart == nil || provenance.ObservationEnd == nil || provenance.QueryFingerprint == "" ||
		provenance.PolicyFingerprint == "" || provenance.OptimizerVersion == "" {
		return false
	}
	cluster, _ := workload["cluster"].(string)
	uid, _ := workload["uid"].(string)
	specFingerprint, _ := workload["sourceSpecFingerprint"].(string)
	if advice.Cluster != cluster || provenance.WorkloadUID != uid ||
		provenance.WorkloadGeneration != numeric(workload["generation"]) ||
		provenance.SourceSpecFingerprint != specFingerprint {
		return false
	}
	return advice.CurrentCPURequestMilli == numeric(container["cpuRequestMilli"]) &&
		advice.CurrentMemoryRequestBytes == numeric(container["memoryRequestBytes"]) &&
		advice.CurrentCPULimitMilli == numeric(container["cpuLimitMilli"]) &&
		advice.CurrentMemoryLimitBytes == numeric(container["memoryLimitBytes"])
}

func realization(autoscaling NodeAutoscaling) string {
	consolidating, emptyOnly := false, false
	for _, pool := range autoscaling.NodePools {
		if pool.ConsolidationPolicy == "WhenEmpty" {
			emptyOnly = true
		} else {
			consolidating = true
		}
	}
	switch {
	case autoscaling.Karpenter && consolidating:
		return "Karpenter consolidates underutilized nodes automatically once the lower requests are applied, within each NodePool's disruption budgets."
	case autoscaling.Karpenter && emptyOnly:
		return "Karpenter NodePools use WhenEmpty consolidation, so only empty nodes are removed; set consolidationPolicy to WhenEmptyOrUnderutilized to realize these savings."
	case autoscaling.Karpenter:
		return "Karpenter manages these nodes; it consolidates them according to its NodePool disruption settings."
	case autoscaling.ClusterAutoscaler:
		return "Cluster Autoscaler removes nodes whose requested resources fall below its scale-down utilization threshold (50% by default)."
	default:
		return "No node autoscaler was detected inside the cluster. A managed autoscaler outside the cluster may still act; otherwise drain and remove nodes manually."
	}
}

// nodeGroupImpacts summarizes every node group and its share of both
// packings. A group with any inferred member is reported as inferred.
func nodeGroupImpacts(nodes []NodeCapacity, autoscaling NodeAutoscaling, current, recommended packingResult) []NodeGroupImpact {
	policies := map[string]string{}
	for _, pool := range autoscaling.NodePools {
		policies[pool.Name] = pool.ConsolidationPolicy
	}
	positions := map[string]int{}
	groups := []NodeGroupImpact{}
	for _, node := range nodes {
		key := nodeGroupKey(node)
		position, found := positions[key]
		if !found {
			position = len(groups)
			positions[key] = position
			groups = append(groups, NodeGroupImpact{Name: node.NodeGroup, Provider: node.NodeGroupProvider, Membership: node.NodeGroupMembership, Evidence: node.NodeGroupEvidence, InstanceTypes: []string{}, CapacityTypes: []string{}})
		}
		group := &groups[position]
		group.Nodes++
		if node.NodeGroupMembership == MembershipInferred && group.Membership != MembershipInferred {
			group.Membership, group.Evidence = MembershipInferred, node.NodeGroupEvidence
		}
		if node.Schedulable {
			group.SchedulableNodes++
			group.CPUMilli += node.CPUMilli
			group.MemoryBytes += node.MemoryBytes
		}
		if node.InstanceType != "" && !slices.Contains(group.InstanceTypes, node.InstanceType) {
			group.InstanceTypes = append(group.InstanceTypes, node.InstanceType)
		}
		if node.CapacityType != "" && !slices.Contains(group.CapacityTypes, node.CapacityType) {
			group.CapacityTypes = append(group.CapacityTypes, node.CapacityType)
		}
	}
	for index := range groups {
		group := &groups[index]
		key := group.Provider + "\x00" + group.Name
		group.CurrentNodesNeeded, group.RecommendedNodesNeeded = current.nodesByGroup[key], recommended.nodesByGroup[key]
		group.ReclaimableNodes = max(0, group.CurrentNodesNeeded-group.RecommendedNodesNeeded)
		group.Realization = groupRealization(*group, policies[group.Name], autoscaling.ClusterAutoscaler)
	}
	sort.SliceStable(groups, func(left, right int) bool {
		if groups[left].ReclaimableNodes != groups[right].ReclaimableNodes {
			return groups[left].ReclaimableNodes > groups[right].ReclaimableNodes
		}
		return groups[left].Provider+groups[left].Name < groups[right].Provider+groups[right].Name
	})
	return groups
}

// groupRealization explains how a node group's nodes would be removed.
func groupRealization(group NodeGroupImpact, consolidationPolicy string, clusterAutoscaler bool) string {
	switch {
	case group.Provider == NodeGroupKarpenter && consolidationPolicy == "WhenEmpty":
		return "This NodePool removes only empty nodes (consolidationPolicy WhenEmpty); set WhenEmptyOrUnderutilized to realize these savings."
	case group.Provider == NodeGroupKarpenter && consolidationPolicy != "":
		return "Karpenter consolidates underutilized nodes in this NodePool within its disruption budgets."
	case group.Provider == NodeGroupKarpenter:
		return "Karpenter manages these nodes, but the NodePool could not be read, so its consolidation policy is unknown."
	case group.Provider == NodeGroupNone:
		return "No node group was identified for these nodes; drain and remove them manually."
	case clusterAutoscaler:
		return "Cluster Autoscaler can remove underutilized nodes from this group down to its minimum size, which is not visible from inside the cluster."
	case group.Provider == NodeGroupGKE || group.Provider == NodeGroupAKS:
		return "The provider's node-pool autoscaler removes underutilized nodes when it is enabled; its settings are not visible from inside the cluster."
	case group.Provider == NodeGroupOpenShift:
		return "A MachineAutoscaler scales this MachineSet when one is configured; otherwise lower the MachineSet's replicas."
	case group.Provider == NodeGroupClusterAPI:
		return "Lower the owning MachineDeployment's replicas, or enable Cluster Autoscaler for it."
	default:
		return "No autoscaler was detected for this node group; lower its desired size or enable Cluster Autoscaler or Karpenter."
	}
}

// capacity estimates how many nodes current and recommended requests need.
func (server *apiServer) capacity(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "capacity"}) {
		return
	}
	unavailable := func(reason string) {
		respondJSON(w, http.StatusOK, map[string]any{"status": Availability{Available: false, Source: "capacity", Reason: reason}})
	}
	reader, ok := server.provider.(CapacityReader)
	if server.provider == nil || !ok {
		unavailable("node capacity requires the Kubernetes inventory")
		return
	}
	cluster, err := reader.Capacity(r.Context())
	if err != nil {
		server.logger.Warn("node capacity unavailable", "request_id", requestIDFromContext(r.Context()), "error", err)
		unavailable("node capacity requires cluster-scoped inventory with permission to list nodes")
		return
	}
	inventory, err := server.provider.List(r.Context(), "workloads", CollectionQuery{Page: 1, PageSize: maxEfficiencyWorkloads})
	if err != nil || !inventory.Status.Available {
		unavailable("workload inventory is unavailable")
		return
	}
	latest, err := server.latestRecommendations(r.Context())
	if err != nil {
		server.repositoryFailure(w, r, "list recommendations", err)
		return
	}
	records, _ := inventory.Items.([]map[string]any)
	var currentPods, recommendedPods []podDemand
	var currentDaemon, recommendedDaemon podDemand
	applied := 0
	for _, record := range records {
		clusterID, _ := record["cluster"].(string)
		namespace, _ := record["namespace"].(string)
		kind, _ := record["kind"].(string)
		name, _ := record["name"].(string)
		containers, _ := record["containers"].([]map[string]any)
		var current, recommended podDemand
		for _, container := range containers {
			containerName, _ := container["name"].(string)
			cpu, memory := numeric(container["cpuRequestMilli"]), numeric(container["memoryRequestBytes"])
			current.cpuMilli += cpu
			current.memoryBytes += memory
			if advice, found := latest[recommendationKey(clusterID, namespace, kind, name, containerName)]; found && capacityAdviceMatches(advice, record, container, server.now().UTC()) {
				cpu, memory = advice.CPURequestMilli, advice.MemoryRequestBytes
				applied++
			}
			recommended.cpuMilli += cpu
			recommended.memoryBytes += memory
		}
		if kind == "DaemonSet" {
			currentDaemon.cpuMilli += current.cpuMilli
			currentDaemon.memoryBytes += current.memoryBytes
			recommendedDaemon.cpuMilli += recommended.cpuMilli
			recommendedDaemon.memoryBytes += recommended.memoryBytes
			continue
		}
		key := namespace + "/" + kind + "/" + name
		current.workload, recommended.workload = key, key
		current.constraints, recommended.constraints = cluster.Constraints[key], cluster.Constraints[key]
		for range numeric(record["desiredReplicas"]) {
			currentPods = append(currentPods, current)
			recommendedPods = append(recommendedPods, recommended)
		}
	}
	var allocatableCPU, allocatableMemory int64
	schedulable := 0
	for _, node := range cluster.Nodes {
		if node.Schedulable {
			schedulable++
			allocatableCPU += node.CPUMilli
			allocatableMemory += node.MemoryBytes
		}
	}
	current := pack(cluster.Nodes, currentDaemon, currentPods)
	recommended := pack(cluster.Nodes, recommendedDaemon, recommendedPods)
	current.CPURequested += currentDaemon.cpuMilli * int64(schedulable)
	current.MemoryRequests += currentDaemon.memoryBytes * int64(schedulable)
	recommended.CPURequested += recommendedDaemon.cpuMilli * int64(schedulable)
	recommended.MemoryRequests += recommendedDaemon.memoryBytes * int64(schedulable)
	body := map[string]any{
		"status":                        Availability{Available: true, Source: "kubernetes-api"},
		"nodes":                         map[string]int{"total": len(cluster.Nodes), "schedulable": schedulable},
		"allocatable":                   map[string]int64{"cpuMilli": allocatableCPU, "memoryBytes": allocatableMemory},
		"current":                       current,
		"recommended":                   recommended,
		"reclaimableNodes":              max(0, current.NodesNeeded-recommended.NodesNeeded),
		"containersWithRecommendations": applied,
		"autoscaling":                   cluster.Autoscaling,
		"nodeGroups":                    nodeGroupImpacts(cluster.Nodes, cluster.Autoscaling, current, recommended),
		"realization":                   realization(cluster.Autoscaling),
		"assumptions": []string{
			"Pods are packed by requests with first-fit-decreasing onto existing node shapes, largest first.",
			"Each pod only uses nodes whose NoSchedule and NoExecute taints it tolerates and whose labels satisfy its nodeSelector and required node affinity.",
			"Preferred affinity, pod affinity and anti-affinity, topology spread, PodDisruptionBudgets, and pods not owned by a supported controller are not modeled; this is an estimate, not a scheduler simulation.",
			"Per-group node counts come from the same packing, which fills the largest nodes first; an autoscaler that scales each group on its own may remove nodes from different groups.",
			fmt.Sprintf("Recommended requests use the newest approved, unexpired, live-workload-matched recommendation for %d containers; others keep their current requests.", applied),
		},
	}
	if cluster.Nodes != nil {
		body["nodeList"] = cluster.Nodes
	}
	respondJSON(w, http.StatusOK, body)
}
