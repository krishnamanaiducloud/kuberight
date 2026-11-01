package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/kuberight/kuberight/internal/repository"
)

type podDemand struct {
	cpuMilli, memoryBytes int64
}

type packingResult struct {
	NodesNeeded    int   `json:"nodesNeeded"`
	UnplacedPods   int   `json:"unplacedPods"`
	CPURequested   int64 `json:"cpuRequestedMilli"`
	MemoryRequests int64 `json:"memoryRequestedBytes"`
}

// pack places pods with first-fit-decreasing onto nodes ordered largest
// first, after reserving DaemonSet pods on every node. It returns how many
// nodes receive at least one pod. This mirrors scheduler requests-based
// placement but ignores affinity, taints, topology spread, and PDBs.
func pack(nodes []NodeCapacity, daemon podDemand, pods []podDemand) packingResult {
	type bin struct{ cpu, memory int64 }
	bins := make([]bin, 0, len(nodes))
	var maxCPU, maxMemory int64 = 1, 1
	for _, node := range nodes {
		if !node.Schedulable {
			continue
		}
		bins = append(bins, bin{node.CPUMilli - daemon.cpuMilli, node.MemoryBytes - daemon.memoryBytes})
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
	result := packingResult{}
	used := make([]bool, len(bins))
	for _, pod := range ordered {
		result.CPURequested += pod.cpuMilli
		result.MemoryRequests += pod.memoryBytes
		placed := false
		for index := range bins {
			if bins[index].cpu >= pod.cpuMilli && bins[index].memory >= pod.memoryBytes {
				bins[index].cpu -= pod.cpuMilli
				bins[index].memory -= pod.memoryBytes
				if !used[index] {
					used[index] = true
					result.NodesNeeded++
				}
				placed = true
				break
			}
		}
		if !placed {
			result.UnplacedPods++
		}
	}
	return result
}

func recommendationKey(namespace, kind, workload, container string) string {
	return strings.Join([]string{namespace, kind, workload, container}, "\x00")
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
				latest[recommendationKey(record.Namespace, record.Kind, record.Workload, record.Container)] = record
			}
		}
		if page*savingsPageSize >= result.Total {
			break
		}
	}
	return latest, nil
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
			if advice, found := latest[recommendationKey(namespace, kind, name, containerName)]; found {
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
		"realization":                   realization(cluster.Autoscaling),
		"assumptions": []string{
			"Pods are packed by requests with first-fit-decreasing onto existing node shapes, largest first.",
			"Affinity, taints, topology spread, PodDisruptionBudgets, and pods not owned by a supported controller are ignored.",
			fmt.Sprintf("Recommended requests use the newest non-rejected recommendation for %d containers; others keep their current requests.", applied),
		},
	}
	if cluster.Nodes != nil {
		body["nodeList"] = cluster.Nodes
	}
	respondJSON(w, http.StatusOK, body)
}
