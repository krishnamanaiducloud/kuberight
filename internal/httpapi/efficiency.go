package httpapi

import (
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

const maxEfficiencyWorkloads = 10_000

// efficiencyRow compares what a namespace reserves with what it uses.
// Usage fields are nil when Prometheus returned no series for the
// namespace, so missing data is never presented as zero usage.
type efficiencyRow struct {
	Namespace                 string   `json:"namespace"`
	Workloads                 int      `json:"workloads"`
	Pods                      int64    `json:"pods"`
	ContainersWithoutRequests int      `json:"containersWithoutRequests"`
	UsageAvailable            bool     `json:"usageAvailable"`
	CPURequestedMilli         int64    `json:"cpuRequestedMilli"`
	CPUAverageMilli           *float64 `json:"cpuAverageMilli,omitempty"`
	CPUP95Milli               *float64 `json:"cpuP95Milli,omitempty"`
	CPUEfficiency             *float64 `json:"cpuEfficiency,omitempty"`
	CPUReclaimableMilli       *float64 `json:"cpuReclaimableMilli,omitempty"`
	MemoryRequestedBytes      int64    `json:"memoryRequestedBytes"`
	MemoryAverageBytes        *float64 `json:"memoryAverageBytes,omitempty"`
	MemoryP95Bytes            *float64 `json:"memoryP95Bytes,omitempty"`
	MemoryEfficiency          *float64 `json:"memoryEfficiency,omitempty"`
	MemoryReclaimableBytes    *float64 `json:"memoryReclaimableBytes,omitempty"`
	RequestedMonthlyCost      *float64 `json:"requestedMonthlyCost,omitempty"`
	ReclaimableMonthlyCost    *float64 `json:"reclaimableMonthlyCost,omitempty"`
}

type efficiencySummary struct {
	Namespaces                int      `json:"namespaces"`
	Workloads                 int      `json:"workloads"`
	ContainersWithoutRequests int      `json:"containersWithoutRequests"`
	CPURequestedMilli         int64    `json:"cpuRequestedMilli"`
	CPUAverageMilli           float64  `json:"cpuAverageMilli"`
	CPUReclaimableMilli       float64  `json:"cpuReclaimableMilli"`
	CPUEfficiency             *float64 `json:"cpuEfficiency,omitempty"`
	MemoryRequestedBytes      int64    `json:"memoryRequestedBytes"`
	MemoryAverageBytes        float64  `json:"memoryAverageBytes"`
	MemoryReclaimableBytes    float64  `json:"memoryReclaimableBytes"`
	MemoryEfficiency          *float64 `json:"memoryEfficiency,omitempty"`
	ReclaimableMonthlyCost    *float64 `json:"reclaimableMonthlyCost,omitempty"`
	Currency                  string   `json:"currency,omitempty"`
	Window                    string   `json:"window"`
	Basis                     string   `json:"basis"`
}

func pointer(value float64) *float64 { return &value }

func ratio(numerator float64, denominator int64) *float64 {
	if denominator <= 0 {
		return nil
	}
	return pointer(math.Round(numerator/float64(denominator)*1000) / 1000)
}

func numeric(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

// efficiency reports, per namespace, requested resources from live inventory
// against average and P95 usage from Prometheus. Reclaimable capacity is
// requested minus P95 usage, which leaves the observed peak untouched.
func (server *apiServer) efficiency(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	window, err := parseDuration(r.URL.Query().Get("window"), 24*time.Hour, time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "efficiency", Namespace: namespace}) {
		return
	}
	unavailable := func(reason string) {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": 1, "pageSize": 0, "total": 0},
			"status": Availability{Available: false, Source: "efficiency", Reason: reason},
		})
	}
	if server.provider == nil {
		unavailable("efficiency requires the Kubernetes inventory, which is not configured")
		return
	}
	if server.usage == nil {
		unavailable("efficiency requires Prometheus usage, which is not configured")
		return
	}
	inventory, err := server.provider.List(r.Context(), "workloads", CollectionQuery{Page: 1, PageSize: maxEfficiencyWorkloads, Namespace: namespace})
	if err != nil {
		server.logger.Error("workload inventory failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "the Kubernetes inventory is unavailable")
		return
	}
	if !inventory.Status.Available {
		unavailable(inventory.Status.Reason)
		return
	}
	records, _ := inventory.Items.([]map[string]any)
	rows := map[string]*efficiencyRow{}
	for _, record := range records {
		name, _ := record["namespace"].(string)
		if name == "" {
			continue
		}
		row := rows[name]
		if row == nil {
			row = &efficiencyRow{Namespace: name}
			rows[name] = row
		}
		replicas := numeric(record["desiredReplicas"])
		row.Workloads++
		row.Pods += replicas
		row.CPURequestedMilli += numeric(record["cpuRequestMilli"]) * replicas
		row.MemoryRequestedBytes += numeric(record["memoryRequestBytes"]) * replicas
		containers, _ := record["containers"].([]map[string]any)
		for _, container := range containers {
			if numeric(container["cpuRequestMilli"]) == 0 || numeric(container["memoryRequestBytes"]) == 0 {
				row.ContainersWithoutRequests++
			}
		}
	}
	namespaces := make([]string, 0, len(rows))
	for name := range rows {
		namespaces = append(namespaces, name)
	}
	sort.Strings(namespaces)

	measured, err := server.usage.NamespaceUsage(r.Context(), namespaces, window, server.now())
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	costed := server.config.CostConfigured()
	summary := efficiencySummary{Window: window.String(), Basis: "requests x desired replicas from inventory; average and P95 usage of all containers in the namespace from Prometheus; reclaimable = requested - P95"}
	items := make([]efficiencyRow, 0, len(namespaces))
	for _, name := range namespaces {
		row := rows[name]
		summary.Workloads += row.Workloads
		summary.ContainersWithoutRequests += row.ContainersWithoutRequests
		if usage, ok := measured[name]; ok {
			row.UsageAvailable = true
			cpuReclaimable := math.Max(0, float64(row.CPURequestedMilli)-usage.CPUP95Milli)
			memoryReclaimable := math.Max(0, float64(row.MemoryRequestedBytes)-usage.MemoryP95Bytes)
			row.CPUAverageMilli, row.CPUP95Milli = pointer(roundMilli(usage.CPUAverageMilli)), pointer(roundMilli(usage.CPUP95Milli))
			row.MemoryAverageBytes, row.MemoryP95Bytes = pointer(math.Round(usage.MemoryAverageBytes)), pointer(math.Round(usage.MemoryP95Bytes))
			row.CPUEfficiency = ratio(usage.CPUAverageMilli, row.CPURequestedMilli)
			row.MemoryEfficiency = ratio(usage.MemoryAverageBytes, row.MemoryRequestedBytes)
			row.CPUReclaimableMilli, row.MemoryReclaimableBytes = pointer(roundMilli(cpuReclaimable)), pointer(math.Round(memoryReclaimable))
			if costed {
				row.ReclaimableMonthlyCost = pointer(roundCents(server.monthlyCost(int64(cpuReclaimable), int64(memoryReclaimable), 1)))
			}
			summary.CPURequestedMilli += row.CPURequestedMilli
			summary.MemoryRequestedBytes += row.MemoryRequestedBytes
			summary.CPUAverageMilli += usage.CPUAverageMilli
			summary.MemoryAverageBytes += usage.MemoryAverageBytes
			summary.CPUReclaimableMilli += cpuReclaimable
			summary.MemoryReclaimableBytes += memoryReclaimable
		}
		if costed {
			row.RequestedMonthlyCost = pointer(roundCents(server.monthlyCost(row.CPURequestedMilli, row.MemoryRequestedBytes, 1)))
		}
		items = append(items, *row)
	}
	// Largest reclaimable capacity first; unmeasured namespaces last.
	sort.SliceStable(items, func(left, right int) bool {
		leftValue, rightValue := -1.0, -1.0
		if items[left].CPUReclaimableMilli != nil {
			leftValue = *items[left].CPUReclaimableMilli
		}
		if items[right].CPUReclaimableMilli != nil {
			rightValue = *items[right].CPUReclaimableMilli
		}
		return leftValue > rightValue
	})
	summary.Namespaces = len(items)
	summary.CPUEfficiency = ratio(summary.CPUAverageMilli, summary.CPURequestedMilli)
	summary.MemoryEfficiency = ratio(summary.MemoryAverageBytes, summary.MemoryRequestedBytes)
	summary.CPUAverageMilli, summary.CPUReclaimableMilli = roundMilli(summary.CPUAverageMilli), roundMilli(summary.CPUReclaimableMilli)
	summary.MemoryAverageBytes, summary.MemoryReclaimableBytes = math.Round(summary.MemoryAverageBytes), math.Round(summary.MemoryReclaimableBytes)
	if costed {
		summary.ReclaimableMonthlyCost = pointer(roundCents(server.monthlyCost(int64(summary.CPUReclaimableMilli), int64(summary.MemoryReclaimableBytes), 1)))
		summary.Currency = server.config.CostCurrency
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items, "pagination": map[string]int{"page": 1, "pageSize": len(items), "total": len(items)},
		"summary": summary,
		"status":  Availability{Available: true, Source: "kubernetes-api+prometheus"},
	})
}

// workloadEfficiencyRow compares a workload's per-pod request with the
// average and peak usage of its own pods.
type workloadEfficiencyRow struct {
	Kind                      string   `json:"kind"`
	Name                      string   `json:"name"`
	Replicas                  int64    `json:"replicas"`
	PodsObserved              int      `json:"podsObserved"`
	ContainersWithoutRequests int      `json:"containersWithoutRequests"`
	CPURequestPerPodMilli     int64    `json:"cpuRequestPerPodMilli"`
	CPUAveragePerPodMilli     *float64 `json:"cpuAveragePerPodMilli,omitempty"`
	CPUP95PerPodMilli         *float64 `json:"cpuP95PerPodMilli,omitempty"`
	CPUEfficiency             *float64 `json:"cpuEfficiency,omitempty"`
	CPUReclaimableMilli       *float64 `json:"cpuReclaimableMilli,omitempty"`
	MemoryRequestPerPodBytes  int64    `json:"memoryRequestPerPodBytes"`
	MemoryAveragePerPodBytes  *float64 `json:"memoryAveragePerPodBytes,omitempty"`
	MemoryP95PerPodBytes      *float64 `json:"memoryP95PerPodBytes,omitempty"`
	MemoryEfficiency          *float64 `json:"memoryEfficiency,omitempty"`
	MemoryReclaimableBytes    *float64 `json:"memoryReclaimableBytes,omitempty"`
	ReclaimableMonthlyCost    *float64 `json:"reclaimableMonthlyCost,omitempty"`
}

// workloadEfficiency ranks a namespace's workloads by reclaimable capacity.
// Reclaimable = (per-pod request - busiest pod's P95 usage) x replicas, which
// matches how recommendations size a single pod.
func (server *apiServer) workloadEfficiency(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if err := (usage.Subject{Namespace: namespace, Kind: "Deployment", Workload: "x", Container: "x"}).Validate(); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", "namespace must be a DNS-1123 label")
		return
	}
	window, err := parseDuration(r.URL.Query().Get("window"), 24*time.Hour, time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "efficiency", Namespace: namespace}) {
		return
	}
	unavailable := func(reason string) {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": 1, "pageSize": 0, "total": 0},
			"status": Availability{Available: false, Source: "efficiency", Reason: reason},
		})
	}
	if server.provider == nil || server.usage == nil {
		unavailable("workload efficiency requires the Kubernetes inventory and Prometheus")
		return
	}
	inventory, err := server.provider.List(r.Context(), "workloads", CollectionQuery{Page: 1, PageSize: maxEfficiencyWorkloads, Namespace: namespace})
	if err != nil {
		server.logger.Error("workload inventory failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "the Kubernetes inventory is unavailable")
		return
	}
	if !inventory.Status.Available {
		unavailable(inventory.Status.Reason)
		return
	}
	pods, err := server.usage.PodUsage(r.Context(), namespace, window, server.now())
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	records, _ := inventory.Items.([]map[string]any)
	// Longer names claim pods first so "api" never takes "api-v2" pods.
	sort.SliceStable(records, func(left, right int) bool {
		leftName, _ := records[left]["name"].(string)
		rightName, _ := records[right]["name"].(string)
		return len(leftName) > len(rightName)
	})
	claimed := map[string]bool{}
	costed := server.config.CostConfigured()
	items := make([]workloadEfficiencyRow, 0, len(records))
	for _, record := range records {
		kind, _ := record["kind"].(string)
		name, _ := record["name"].(string)
		matcher, matchErr := usage.PodMatcher(kind, name)
		if matchErr != nil || record["namespace"] != namespace {
			continue
		}
		row := workloadEfficiencyRow{
			Kind: kind, Name: name, Replicas: numeric(record["desiredReplicas"]),
			CPURequestPerPodMilli: numeric(record["cpuRequestMilli"]), MemoryRequestPerPodBytes: numeric(record["memoryRequestBytes"]),
		}
		containers, _ := record["containers"].([]map[string]any)
		for _, container := range containers {
			if numeric(container["cpuRequestMilli"]) == 0 || numeric(container["memoryRequestBytes"]) == 0 {
				row.ContainersWithoutRequests++
			}
		}
		var cpuAverage, cpuPeak, memoryAverage, memoryPeak float64
		for pod, podUsage := range pods {
			if claimed[pod] || !matcher.MatchString(pod) {
				continue
			}
			claimed[pod] = true
			row.PodsObserved++
			cpuAverage += podUsage.CPUAverageMilli
			memoryAverage += podUsage.MemoryAverageBytes
			cpuPeak = math.Max(cpuPeak, podUsage.CPUP95Milli)
			memoryPeak = math.Max(memoryPeak, podUsage.MemoryP95Bytes)
		}
		if row.PodsObserved > 0 {
			observed := float64(row.PodsObserved)
			row.CPUAveragePerPodMilli, row.CPUP95PerPodMilli = pointer(roundMilli(cpuAverage/observed)), pointer(roundMilli(cpuPeak))
			row.MemoryAveragePerPodBytes, row.MemoryP95PerPodBytes = pointer(math.Round(memoryAverage/observed)), pointer(math.Round(memoryPeak))
			row.CPUEfficiency = ratio(cpuAverage/observed, row.CPURequestPerPodMilli)
			row.MemoryEfficiency = ratio(memoryAverage/observed, row.MemoryRequestPerPodBytes)
			replicas := float64(max(row.Replicas, 1))
			cpuReclaimable := math.Max(0, float64(row.CPURequestPerPodMilli)-cpuPeak) * replicas
			memoryReclaimable := math.Max(0, float64(row.MemoryRequestPerPodBytes)-memoryPeak) * replicas
			row.CPUReclaimableMilli, row.MemoryReclaimableBytes = pointer(roundMilli(cpuReclaimable)), pointer(math.Round(memoryReclaimable))
			if costed {
				row.ReclaimableMonthlyCost = pointer(roundCents(server.monthlyCost(int64(cpuReclaimable), int64(memoryReclaimable), 1)))
			}
		}
		items = append(items, row)
	}
	sort.SliceStable(items, func(left, right int) bool {
		value := func(row workloadEfficiencyRow) float64 {
			if row.CPUReclaimableMilli == nil {
				return -1
			}
			return *row.CPUReclaimableMilli
		}
		return value(items[left]) > value(items[right])
	})
	summary := map[string]any{"window": window.String(), "podsUnmatched": len(pods) - len(claimed)}
	if costed {
		summary["currency"] = server.config.CostCurrency
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items, "pagination": map[string]int{"page": 1, "pageSize": len(items), "total": len(items)},
		"summary": summary, "status": Availability{Available: true, Source: "kubernetes-api+prometheus"},
	})
}
