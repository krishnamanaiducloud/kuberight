package usage

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/metrics"
)

// maxNamespaceMatcherBytes keeps the namespace matcher well inside the
// Prometheus client's expression bound. Larger fleets are filtered after
// the query instead.
const maxNamespaceMatcherBytes = 4 << 10

// NamespaceUsage summarizes all containers in a namespace over a window.
// CPU values are millicores and memory values are bytes.
type NamespaceUsage struct {
	CPUAverageMilli    float64 `json:"cpuAverageMilli"`
	CPUP95Milli        float64 `json:"cpuP95Milli"`
	MemoryAverageBytes float64 `json:"memoryAverageBytes"`
	MemoryP95Bytes     float64 `json:"memoryP95Bytes"`
}

func namespaceSelector(namespaces []string) string {
	selector := `container!="",container!="POD"`
	valid := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		if len(namespace) <= 63 && dnsLabel.MatchString(namespace) {
			valid = append(valid, namespace)
		}
	}
	matcher := strings.Join(valid, "|")
	if len(valid) > 0 && len(matcher) <= maxNamespaceMatcherBytes {
		selector += ",namespace=~" + strconv.Quote(matcher)
	}
	return selector
}

// subqueryStep keeps each namespace subquery to about 500 evaluations.
func subqueryStep(window time.Duration) time.Duration {
	return max(5*time.Minute, (window / 500).Truncate(time.Minute))
}

// NamespaceExpressions returns the average and P95 expressions for CPU and
// memory, in that order. Exported for tests and operator troubleshooting.
func NamespaceExpressions(namespaces []string, window time.Duration) [4]string {
	selector := namespaceSelector(namespaces)
	step := subqueryStep(window)
	subquery := "[" + promDuration(window) + ":" + promDuration(step) + "]"
	cpu := "sum by (namespace) (rate(container_cpu_usage_seconds_total{" + selector + "}[" + rateWindow(step) + "]))"
	memory := "sum by (namespace) (container_memory_working_set_bytes{" + selector + "})"
	return [4]string{
		"avg_over_time((" + cpu + ")" + subquery + ") * 1000",
		"quantile_over_time(0.95, (" + cpu + ")" + subquery + ") * 1000",
		"avg_over_time((" + memory + ")" + subquery + ")",
		"quantile_over_time(0.95, (" + memory + ")" + subquery + ")",
	}
}

// NamespaceUsage reads per-namespace usage. Only the requested namespaces are
// returned, so a namespace-scoped deployment cannot disclose other tenants'
// usage even when the matcher had to be dropped for length.
func (source *Source) NamespaceUsage(ctx context.Context, namespaces []string, window time.Duration, end time.Time) (map[string]NamespaceUsage, error) {
	if end.IsZero() {
		end = source.now()
	}
	end = end.UTC().Truncate(time.Second)
	allowed := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		allowed[namespace] = struct{}{}
	}
	result := make(map[string]NamespaceUsage, len(namespaces))
	for index, expression := range NamespaceExpressions(namespaces, window) {
		values, err := source.instantVector(ctx, expression, end, "namespace")
		if err != nil {
			return nil, fmt.Errorf("namespace usage query %d: %w", index+1, err)
		}
		for namespace, value := range values {
			if _, ok := allowed[namespace]; !ok {
				continue
			}
			usage := result[namespace]
			switch index {
			case 0:
				usage.CPUAverageMilli = value
			case 1:
				usage.CPUP95Milli = value
			case 2:
				usage.MemoryAverageBytes = value
			default:
				usage.MemoryP95Bytes = value
			}
			result[namespace] = usage
		}
	}
	return result, nil
}

// instantVector evaluates an expression at one instant and keys the result
// by the given label. Series without the label are ignored.
func (source *Source) instantVector(ctx context.Context, expression string, at time.Time, label string) (map[string]float64, error) {
	response, err := source.client.QueryRange(ctx, metrics.RangeQuery{Expression: expression, Start: at, End: at, Step: time.Minute})
	if err != nil {
		return nil, err
	}
	values := make(map[string]float64, len(response.Series))
	for _, series := range response.Series {
		key := series.Labels[label]
		if key == "" || len(series.Samples) == 0 {
			continue
		}
		values[key] = max(series.Samples[len(series.Samples)-1].Value, 0)
	}
	return values, nil
}

// PodUsage is one pod's usage over a window. CPU is millicores, memory bytes.
type PodUsage = NamespaceUsage

// PodExpressions mirror NamespaceExpressions, aggregated by pod within one
// namespace so per-pod requests can be compared with per-pod usage.
func PodExpressions(namespace string, window time.Duration) [4]string {
	selector := `container!="",container!="POD",namespace=` + strconv.Quote(namespace)
	step := subqueryStep(window)
	subquery := "[" + promDuration(window) + ":" + promDuration(step) + "]"
	cpu := "sum by (pod) (rate(container_cpu_usage_seconds_total{" + selector + "}[" + rateWindow(step) + "]))"
	memory := "sum by (pod) (container_memory_working_set_bytes{" + selector + "})"
	return [4]string{
		"avg_over_time((" + cpu + ")" + subquery + ") * 1000",
		"quantile_over_time(0.95, (" + cpu + ")" + subquery + ") * 1000",
		"avg_over_time((" + memory + ")" + subquery + ")",
		"quantile_over_time(0.95, (" + memory + ")" + subquery + ")",
	}
}

// PodUsage reads per-pod usage in one namespace.
func (source *Source) PodUsage(ctx context.Context, namespace string, window time.Duration, end time.Time) (map[string]PodUsage, error) {
	if len(namespace) > 63 || !dnsLabel.MatchString(namespace) {
		return nil, fmt.Errorf("%w: namespace must be a DNS-1123 label", ErrInvalidSubject)
	}
	if end.IsZero() {
		end = source.now()
	}
	end = end.UTC().Truncate(time.Second)
	result := map[string]PodUsage{}
	for index, expression := range PodExpressions(namespace, window) {
		values, err := source.instantVector(ctx, expression, end, "pod")
		if err != nil {
			return nil, fmt.Errorf("pod usage query %d: %w", index+1, err)
		}
		for pod, value := range values {
			usage := result[pod]
			switch index {
			case 0:
				usage.CPUAverageMilli = value
			case 1:
				usage.CPUP95Milli = value
			case 2:
				usage.MemoryAverageBytes = value
			default:
				usage.MemoryP95Bytes = value
			}
			result[pod] = usage
		}
	}
	return result, nil
}

// PodMatcher reports whether a pod name belongs to a workload, using the
// same controller naming rules as usage queries.
func PodMatcher(kind, workload string) (*regexp.Regexp, error) {
	suffix, ok := podSuffixes[kind]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported kind %q", ErrInvalidSubject, kind)
	}
	return regexp.Compile("^" + regexp.QuoteMeta(workload) + suffix + "$")
}
