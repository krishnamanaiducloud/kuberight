package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

func nodeGroupKey(node NodeCapacity) string { return node.NodeGroupProvider + "\x00" + node.NodeGroup }

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
func nodeGroupImpacts(nodes []NodeCapacity, autoscaling NodeAutoscaling, limits map[string]NodeGroupLimit, current, recommended packingResult) []NodeGroupImpact {
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
		kept := group.RecommendedNodesNeeded
		if limit, found := limits[key]; found {
			group.Limit = &limit
			// The autoscaler keeps at least the group's minimum size.
			group.LimitedByMinimum = limit.MinSize > group.RecommendedNodesNeeded && group.CurrentNodesNeeded > group.RecommendedNodesNeeded
			group.ExceedsMaximum = group.RecommendedNodesNeeded > limit.MaxSize
			kept = max(kept, limit.MinSize)
		}
		group.ReclaimableNodes = max(0, group.CurrentNodesNeeded-kept)
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
	case group.Limit != nil && group.Provider == NodeGroupOpenShift && strings.HasPrefix(group.Limit.Source, "MachineAutoscaler"):
		return fmt.Sprintf("%s scales this MachineSet between %d and %d machines.", group.Limit.Source, group.Limit.MinSize, group.Limit.MaxSize)
	case group.Limit != nil:
		return fmt.Sprintf("Cluster Autoscaler can remove underutilized nodes from this group down to its minimum of %d (maximum %d).", group.Limit.MinSize, group.Limit.MaxSize)
	case clusterAutoscaler:
		return "Cluster Autoscaler can remove underutilized nodes from this group down to its minimum size, which was not found in its status."
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
		current.namespace, recommended.namespace = namespace, namespace
		current.constraints, recommended.constraints = cluster.Constraints[key], cluster.Constraints[key]
		pins := cluster.Pinned[key]
		for replica := range numeric(record["desiredReplicas"]) {
			current.pinnedNode, recommended.pinnedNode = "", ""
			if replica < int64(len(pins)) {
				current.pinnedNode, recommended.pinnedNode = pins[replica], pins[replica]
			}
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
	groups := nodeGroupImpacts(cluster.Nodes, cluster.Autoscaling, cluster.GroupLimits, current, recommended)
	// Nodes a group's minimum size keeps cannot be reclaimed.
	held := 0
	for _, group := range groups {
		held += max(0, group.CurrentNodesNeeded-group.RecommendedNodesNeeded) - group.ReclaimableNodes
	}
	body := map[string]any{
		"status":                        Availability{Available: true, Source: "kubernetes-api"},
		"nodes":                         map[string]int{"total": len(cluster.Nodes), "schedulable": schedulable},
		"allocatable":                   map[string]int64{"cpuMilli": allocatableCPU, "memoryBytes": allocatableMemory},
		"current":                       current,
		"recommended":                   recommended,
		"reclaimableNodes":              max(0, current.NodesNeeded-recommended.NodesNeeded-held),
		"nodesHeldByMinimumSize":        held,
		"costTiers":                     server.costTiers(r.Context(), current, recommended, groups),
		"containersWithRecommendations": applied,
		"autoscaling":                   cluster.Autoscaling,
		"nodeGroups":                    groups,
		"realization":                   realization(cluster.Autoscaling),
		"evictionBlockers":              evictionBlockers(cluster.EvictionBlockers),
		"assumptions": []string{
			"Pods are packed by requests with first-fit-decreasing onto existing node shapes, largest first.",
			"Each pod only uses nodes whose NoSchedule and NoExecute taints it tolerates and whose labels satisfy its nodeSelector and required node affinity.",
			"Required pod affinity and anti-affinity are checked, in both directions, against the pods already placed in the same topology domain; a namespace selector is treated as selecting every namespace.",
			"DoNotSchedule topology spread caps each node at maxSkew matching pods on the hostname key, more only when the replicas outnumber maxSkew per eligible node, and keeps other domains, such as zones, within maxSkew of the least-used domain.",
			"Pods covered by a PodDisruptionBudget that allows no voluntary evictions stay on the nodes they run on now, so those nodes are kept.",
			"Preferred affinity, ScheduleAnyway spread, and pods not owned by a supported controller are not modeled; this is an estimate, not a scheduler simulation.",
			"A node group never shrinks below a minimum size read from an OpenShift MachineAutoscaler or Cluster Autoscaler's status; groups without one may also have a minimum that is not visible.",
			"Per-group node counts come from the same packing, which fills the largest nodes first; an autoscaler that scales each group on its own may remove nodes from different groups.",
			fmt.Sprintf("Recommended requests use the newest approved, unexpired, live-workload-matched recommendation for %d containers; others keep their current requests.", applied),
		},
	}
	if cluster.Nodes != nil {
		body["nodeList"] = cluster.Nodes
	}
	respondJSON(w, http.StatusOK, body)
}

// costTiers separates three amounts that are easy to conflate: the value of
// lower requests, which changes no bill by itself; the cost of the nodes the
// packing frees within node-group minimums, which is what removing them could
// save; and the realized bill, which KubeRight does not measure.
func (server *apiServer) costTiers(ctx context.Context, current, recommended packingResult, groups []NodeGroupImpact) map[string]any {
	billing := map[string]any{"measured": false, "detail": "Cloud billing is not ingested. Realized savings appear on the bill only after the nodes are removed, and depend on discounts, reservations, and spot prices."}
	if !server.config.CostConfigured() {
		return map[string]any{"available": false, "reason": "unit prices are not configured; set KUBERIGHT_COST_CPU_CORE_HOUR and KUBERIGHT_COST_MEMORY_GIB_HOUR", "billing": billing}
	}
	prices := server.pricer.Prices(ctx, "")
	monthly := func(cpuMilli, memoryBytes float64) float64 {
		value := (cpuMilli/1000*prices.CPUCoreHour + memoryBytes/(1<<30)*prices.MemoryGiBHour) * hoursPerMonth
		return math.Round(value*100) / 100
	}
	nodes, nodeCPU, nodeMemory := 0, 0.0, 0.0
	for _, group := range groups {
		if group.ReclaimableNodes == 0 || group.SchedulableNodes == 0 {
			continue
		}
		// A freed node is priced as the group's average schedulable node.
		share := float64(group.ReclaimableNodes) / float64(group.SchedulableNodes)
		nodes += group.ReclaimableNodes
		nodeCPU += float64(group.CPUMilli) * share
		nodeMemory += float64(group.MemoryBytes) * share
	}
	return map[string]any{
		"available": true, "currency": server.config.CostCurrency, "priceSource": prices.Source,
		"requestReduction": map[string]any{
			"monthly": monthly(float64(current.CPURequested-recommended.CPURequested), float64(current.MemoryRequests-recommended.MemoryRequests)),
			"detail":  "Lower requests across every replica, priced at unit prices for 730 hours. Requests alone do not change the bill.",
		},
		"feasibleNodeSavings": map[string]any{
			"nodes":   nodes,
			"monthly": monthly(nodeCPU, nodeMemory),
			"detail":  "The allocatable CPU and memory of the nodes the packing frees, after node-group minimums, priced at unit prices for 730 hours. An estimate of what removing those nodes could save, not a quote.",
		},
		"billing": billing,
	}
}

func evictionBlockers(blockers []string) []string {
	if blockers == nil {
		return []string{}
	}
	return blockers
}
