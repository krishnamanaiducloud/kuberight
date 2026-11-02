package usage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

// maxHotspots bounds each fleet query so a large cluster stays within the
// Prometheus client's series limit.
const maxHotspots = 200

// ContainerHotspot is one pod container's throttling and spike behavior.
// Absent measurements are nil, never zero.
type ContainerHotspot struct {
	Namespace           string   `json:"namespace"`
	Pod                 string   `json:"pod"`
	Container           string   `json:"container"`
	ThrottlingRatio     *float64 `json:"throttlingRatio,omitempty"`
	ThrottlingPeakRatio *float64 `json:"throttlingPeakRatio,omitempty"`
	SpikeRatio          *float64 `json:"spikeRatio,omitempty"`
	CPUMedianMilli      *float64 `json:"cpuMedianMilli,omitempty"`
	CPUPeakMilli        *float64 `json:"cpuPeakMilli,omitempty"`
}

var hotspotLabels = []string{"namespace", "pod", "container"}

// HotspotExpressions returns the fleet queries in order: average throttling,
// peak-interval throttling, peak-to-median CPU ratio, median CPU, and peak CPU.
// The last two are restricted to the containers ranked by the ratio.
func HotspotExpressions(namespaces []string, window time.Duration) [5]string {
	selector := namespaceSelector(namespaces)
	step := subqueryStep(window)
	rate := rateWindow(step)
	by := "sum by (namespace, pod, container) "
	subquery := "[" + promDuration(window) + ":" + promDuration(step) + "]"
	cpu := by + "(rate(container_cpu_usage_seconds_total{" + selector + "}[" + rate + "]))"
	ratio := "max_over_time((" + cpu + ")" + subquery + ") / clamp_min(quantile_over_time(0.5, (" + cpu + ")" + subquery + "), 0.001)"
	top := fmt.Sprintf("topk(%d, %s)", maxHotspots, ratio)
	return [5]string{
		fmt.Sprintf("topk(%d, %s(increase(container_cpu_cfs_throttled_periods_total{%s}[%s])) / %s(increase(container_cpu_cfs_periods_total{%s}[%s])))",
			maxHotspots, by, selector, promDuration(window), by, selector, promDuration(window)),
		fmt.Sprintf("topk(%d, quantile_over_time(0.95, (%s(rate(container_cpu_cfs_throttled_periods_total{%s}[%s])) / %s(rate(container_cpu_cfs_periods_total{%s}[%s])))%s))",
			maxHotspots, by, selector, rate, by, selector, rate, subquery),
		top,
		"(quantile_over_time(0.5, (" + cpu + ")" + subquery + ") * 1000) and on (namespace, pod, container) (" + top + ")",
		"(max_over_time((" + cpu + ")" + subquery + ") * 1000) and on (namespace, pod, container) (" + top + ")",
	}
}

// Hotspots returns containers with the most throttling or the spikiest CPU
// in the given namespaces. Only requested namespaces are returned.
func (source *Source) Hotspots(ctx context.Context, namespaces []string, window time.Duration, end time.Time) ([]ContainerHotspot, error) {
	if len(namespaces) == 0 {
		return []ContainerHotspot{}, nil
	}
	if end.IsZero() {
		end = source.now()
	}
	end = end.UTC().Truncate(time.Second)
	allowed := make(map[string]bool, len(namespaces))
	for _, namespace := range namespaces {
		allowed[namespace] = true
	}
	byKey := map[string]*ContainerHotspot{}
	order := []string{}
	for index, expression := range HotspotExpressions(namespaces, window) {
		response, err := source.client.QueryRange(ctx, metrics.RangeQuery{Expression: expression, Start: end, End: end, Step: time.Minute})
		if err != nil {
			return nil, fmt.Errorf("hotspot query %d: %w", index+1, err)
		}
		for _, series := range response.Series {
			namespace, pod, container := series.Labels["namespace"], series.Labels["pod"], series.Labels["container"]
			if !allowed[namespace] || pod == "" || container == "" || len(series.Samples) == 0 {
				continue
			}
			key := strings.Join([]string{namespace, pod, container}, "\x00")
			spot := byKey[key]
			if spot == nil {
				spot = &ContainerHotspot{Namespace: namespace, Pod: pod, Container: container}
				byKey[key] = spot
				order = append(order, key)
			}
			value := max(series.Samples[len(series.Samples)-1].Value, 0)
			switch index {
			case 0:
				value = min(value, 1)
				spot.ThrottlingRatio = &value
			case 1:
				value = min(value, 1)
				spot.ThrottlingPeakRatio = &value
			case 2:
				spot.SpikeRatio = &value
			case 3:
				spot.CPUMedianMilli = &value
			default:
				spot.CPUPeakMilli = &value
			}
		}
	}
	spots := make([]ContainerHotspot, 0, len(order))
	for _, key := range order {
		spots = append(spots, *byKey[key])
	}
	return spots, nil
}
