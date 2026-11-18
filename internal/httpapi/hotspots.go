package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/usage"
)

// PodOwner is an exact Kubernetes controller relationship for one pod.
type PodOwner struct {
	Namespace string
	Pod       string
	Kind      string
	Workload  string
}

// PodOwnerResolver prevents metrics from being attributed by pod-name
// heuristics. The Kubernetes provider resolves ReplicaSet and Job chains to
// their Deployment and CronJob controllers.
type PodOwnerResolver interface {
	PodOwners(ctx context.Context, namespaces []string) ([]PodOwner, error)
}

const (
	hotspotThrottling         = 0.05
	hotspotPeakThrottling     = 0.25
	hotspotSevereThrottling   = 0.25
	hotspotSeverePeak         = 0.5
	hotspotSpikeRatio         = 3.0
	hotspotPinnedLimitShare   = 0.95
	hotspotWorkloadSampleSize = 10_000
)

// hotspotRow is one workload container's throttling and spike behavior,
// aggregated across its pods by the worst pod.
type hotspotRow struct {
	Namespace           string   `json:"namespace"`
	Kind                string   `json:"kind"`
	Workload            string   `json:"workload"`
	Container           string   `json:"container"`
	Pods                int      `json:"pods"`
	ThrottlingRatio     *float64 `json:"throttlingRatio,omitempty"`
	ThrottlingPeakRatio *float64 `json:"throttlingPeakRatio,omitempty"`
	SpikeRatio          *float64 `json:"spikeRatio,omitempty"`
	CPUMedianMilli      *float64 `json:"cpuMedianMilli,omitempty"`
	CPUPeakMilli        *float64 `json:"cpuPeakMilli,omitempty"`
	CPURequestMilli     int64    `json:"cpuRequestMilli"`
	CPULimitMilli       int64    `json:"cpuLimitMilli"`
	PinnedAtLimit       bool     `json:"pinnedAtLimit"`
	Severity            string   `json:"severity"`
	Issues              []string `json:"issues"`
	Advice              string   `json:"advice"`
}

func maxPointer(current, candidate *float64) *float64 {
	if candidate == nil {
		return current
	}
	if current == nil || *candidate > *current {
		value := *candidate
		return &value
	}
	return current
}

func valueOf(pointer *float64) float64 {
	if pointer == nil {
		return 0
	}
	return *pointer
}

var severityRank = map[string]int{"critical": 3, "warning": 2, "info": 1}

// classify sets issues, severity, and advice. It returns false when the
// container has no throttling, pinning, or spike problem.
func (row *hotspotRow) classify() bool {
	throttled := valueOf(row.ThrottlingRatio) >= hotspotThrottling || valueOf(row.ThrottlingPeakRatio) >= hotspotPeakThrottling
	row.PinnedAtLimit = row.CPULimitMilli > 0 && valueOf(row.CPUPeakMilli) >= float64(row.CPULimitMilli)*hotspotPinnedLimitShare
	spiky := valueOf(row.SpikeRatio) >= hotspotSpikeRatio
	row.Issues = []string{}
	raise := func(severity string) {
		if severityRank[severity] > severityRank[row.Severity] {
			row.Severity = severity
		}
	}
	if throttled {
		row.Issues = append(row.Issues, fmt.Sprintf("throttled in %.1f%% of periods overall and %.1f%% in peak intervals", valueOf(row.ThrottlingRatio)*100, valueOf(row.ThrottlingPeakRatio)*100))
		if valueOf(row.ThrottlingRatio) >= hotspotSevereThrottling || valueOf(row.ThrottlingPeakRatio) >= hotspotSeverePeak {
			raise("critical")
		} else {
			raise("warning")
		}
	}
	if row.PinnedAtLimit {
		row.Issues = append(row.Issues, "usage reaches the CPU limit, so measured usage is capped")
		raise("warning")
	}
	if spiky {
		row.Issues = append(row.Issues, fmt.Sprintf("peaks reach %.1fx median usage", valueOf(row.SpikeRatio)))
		raise("info")
		if row.CPURequestMilli > 0 && valueOf(row.CPUPeakMilli) > float64(row.CPURequestMilli) {
			row.Issues = append(row.Issues, fmt.Sprintf("peaks exceed the request by %.0f%%", (valueOf(row.CPUPeakMilli)/float64(row.CPURequestMilli)-1)*100))
		}
	}
	switch {
	case throttled:
		row.Advice = "Raise or remove the CPU limit before any CPU reduction; generate a recommendation to size it from peaks."
	case row.PinnedAtLimit:
		row.Advice = "Raise the CPU limit; true demand is hidden while usage is capped."
	case spiky && row.CPULimitMilli > 0 && valueOf(row.CPUPeakMilli) > float64(row.CPULimitMilli)*0.8:
		row.Advice = "Spikes approach the CPU limit; keep the limit above the largest spike or remove it."
	case spiky:
		row.Advice = "Size the request from P99 or scale on demand so spikes do not depend on spare node capacity."
	}
	return len(row.Issues) > 0
}

// hotspots ranks containers by CPU throttling, capped usage, and spikiness.
func (server *apiServer) hotspots(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if namespace != "" {
		if err := (usage.Subject{Namespace: namespace, Kind: "Deployment", Workload: "x", Container: "x"}).Validate(); err != nil {
			respondError(w, r, http.StatusBadRequest, "invalid_request", "namespace must be a DNS-1123 label")
			return
		}
	}
	window, err := parseDuration(r.URL.Query().Get("window"), 24*time.Hour, time.Hour)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "hotspots", Namespace: namespace}) {
		return
	}
	unavailable := func(reason string) {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": 1, "pageSize": 0, "total": 0},
			"status": Availability{Available: false, Source: "hotspots", Reason: reason},
		})
	}
	if server.provider == nil || server.usage == nil {
		unavailable("CPU hotspots require the Kubernetes inventory and Prometheus")
		return
	}
	inventory, err := server.provider.List(r.Context(), "workloads", CollectionQuery{Page: 1, PageSize: hotspotWorkloadSampleSize, Namespace: namespace})
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
	namespaces := []string{}
	seen := map[string]bool{}
	for _, record := range records {
		if name, _ := record["namespace"].(string); name != "" && !seen[name] {
			seen[name] = true
			namespaces = append(namespaces, name)
		}
	}
	sort.Strings(namespaces)
	ownerResolver, ok := server.provider.(PodOwnerResolver)
	if !ok {
		unavailable("CPU hotspots require exact Kubernetes pod-owner resolution")
		return
	}
	owners, err := ownerResolver.PodOwners(r.Context(), namespaces)
	if err != nil {
		server.logger.Error("pod owner inventory failed", "request_id", requestIDFromContext(r.Context()), "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "Kubernetes pod ownership is unavailable")
		return
	}
	ownerByPod := make(map[string]PodOwner, len(owners))
	for _, owner := range owners {
		ownerByPod[strings.Join([]string{owner.Namespace, owner.Pod}, "\x00")] = owner
	}
	workloadByKey := make(map[string]map[string]any, len(records))
	for _, record := range records {
		ns, _ := record["namespace"].(string)
		kind, _ := record["kind"].(string)
		name, _ := record["name"].(string)
		workloadByKey[strings.Join([]string{ns, kind, name}, "\x00")] = record
	}
	spots, err := server.usage.Hotspots(r.Context(), namespaces, window, server.now())
	if err != nil {
		server.usageFailure(w, r, err)
		return
	}
	rows := map[string]*hotspotRow{}
	unmatched := 0
	for _, spot := range spots {
		controller, found := ownerByPod[strings.Join([]string{spot.Namespace, spot.Pod}, "\x00")]
		if !found {
			unmatched++
			continue
		}
		owner := workloadByKey[strings.Join([]string{controller.Namespace, controller.Kind, controller.Workload}, "\x00")]
		if owner == nil {
			unmatched++
			continue
		}
		kind, name := controller.Kind, controller.Workload
		key := strings.Join([]string{spot.Namespace, kind, name, spot.Container}, "\x00")
		row := rows[key]
		if row == nil {
			row = &hotspotRow{Namespace: spot.Namespace, Kind: kind, Workload: name, Container: spot.Container}
			containers, _ := owner["containers"].([]map[string]any)
			containerFound := false
			for _, container := range containers {
				if container["name"] == spot.Container {
					row.CPURequestMilli, row.CPULimitMilli = numeric(container["cpuRequestMilli"]), numeric(container["cpuLimitMilli"])
					containerFound = true
					break
				}
			}
			if !containerFound {
				unmatched++
				continue
			}
			rows[key] = row
		}
		row.Pods++
		row.ThrottlingRatio = maxPointer(row.ThrottlingRatio, spot.ThrottlingRatio)
		row.ThrottlingPeakRatio = maxPointer(row.ThrottlingPeakRatio, spot.ThrottlingPeakRatio)
		row.SpikeRatio = maxPointer(row.SpikeRatio, spot.SpikeRatio)
		row.CPUMedianMilli = maxPointer(row.CPUMedianMilli, spot.CPUMedianMilli)
		row.CPUPeakMilli = maxPointer(row.CPUPeakMilli, spot.CPUPeakMilli)
	}
	items := make([]hotspotRow, 0, len(rows))
	summary := map[string]any{"window": window.String(), "podsUnmatched": unmatched, "throttled": 0, "pinned": 0, "spiky": 0}
	for _, row := range rows {
		for _, pointer := range []**float64{&row.ThrottlingRatio, &row.ThrottlingPeakRatio} {
			if *pointer != nil {
				rounded := math.Round(**pointer*10_000) / 10_000
				*pointer = &rounded
			}
		}
		if !row.classify() {
			continue
		}
		if valueOf(row.ThrottlingRatio) >= hotspotThrottling || valueOf(row.ThrottlingPeakRatio) >= hotspotPeakThrottling {
			summary["throttled"] = summary["throttled"].(int) + 1
		}
		if row.PinnedAtLimit {
			summary["pinned"] = summary["pinned"].(int) + 1
		}
		if valueOf(row.SpikeRatio) >= hotspotSpikeRatio {
			summary["spiky"] = summary["spiky"].(int) + 1
		}
		items = append(items, *row)
	}
	sort.SliceStable(items, func(left, right int) bool {
		if severityRank[items[left].Severity] != severityRank[items[right].Severity] {
			return severityRank[items[left].Severity] > severityRank[items[right].Severity]
		}
		if valueOf(items[left].ThrottlingPeakRatio) != valueOf(items[right].ThrottlingPeakRatio) {
			return valueOf(items[left].ThrottlingPeakRatio) > valueOf(items[right].ThrottlingPeakRatio)
		}
		return valueOf(items[left].SpikeRatio) > valueOf(items[right].SpikeRatio)
	})
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items, "pagination": map[string]int{"page": 1, "pageSize": len(items), "total": len(items)},
		"summary": summary, "status": Availability{Available: true, Source: "kubernetes-api+prometheus"},
	})
}
