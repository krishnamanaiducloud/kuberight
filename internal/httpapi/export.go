package httpapi

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// deliveryStats is implemented by notifiers that count deliveries.
type deliveryStats interface {
	Stats() (delivered, failed, dropped uint64)
}

// renderIntegrationMetrics appends webhook counters and, when enabled, one
// gauge per container recommendation so Grafana, Coralogix, Datadog, and
// other OpenMetrics consumers can chart and alert on KubeRight's advice.
// /metrics is unauthenticated, so recommendation export is opt-in.
func (server *apiServer) renderIntegrationMetrics(ctx context.Context, w io.Writer) {
	if stats, ok := server.notifier.(deliveryStats); ok && server.notifier != nil {
		delivered, failed, dropped := stats.Stats()
		_, _ = fmt.Fprintln(w, "# HELP kuberight_webhook_events_total Webhook events by outcome.")
		_, _ = fmt.Fprintln(w, "# TYPE kuberight_webhook_events_total counter")
		_, _ = fmt.Fprintf(w, "kuberight_webhook_events_total{outcome=\"delivered\"} %d\n", delivered)
		_, _ = fmt.Fprintf(w, "kuberight_webhook_events_total{outcome=\"failed\"} %d\n", failed)
		_, _ = fmt.Fprintf(w, "kuberight_webhook_events_total{outcome=\"dropped\"} %d\n", dropped)
	}
	if !server.config.ExportRecommendationMetrics {
		return
	}
	latest, err := server.latestRecommendations(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(w, "# kuberight recommendation export unavailable: store error")
		return
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type gauge struct{ name, help string }
	gauges := []gauge{
		{"kuberight_recommendation_cpu_request_millicores", "Current and recommended CPU request per container."},
		{"kuberight_recommendation_memory_request_bytes", "Current and recommended memory request per container."},
		{"kuberight_recommendation_cpu_limit_millicores", "Current and recommended CPU limit per container; absent when unset."},
		{"kuberight_recommendation_memory_limit_bytes", "Current and recommended memory limit per container; absent when unset."},
	}
	for gaugeIndex, item := range gauges {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", item.name, item.help, item.name)
		for _, key := range keys {
			record := latest[key]
			values := [][2]int64{
				{record.CurrentCPURequestMilli, record.CPURequestMilli},
				{record.CurrentMemoryRequestBytes, record.MemoryRequestBytes},
				{record.CurrentCPULimitMilli, record.CPULimitMilli},
				{record.CurrentMemoryLimitBytes, record.MemoryLimitBytes},
			}[gaugeIndex]
			labels := fmt.Sprintf("cluster=\"%s\",namespace=\"%s\",kind=\"%s\",workload=\"%s\",container=\"%s\"",
				prometheusLabel(record.Cluster), prometheusLabel(record.Namespace), prometheusLabel(record.Kind), prometheusLabel(record.Workload), prometheusLabel(record.Container))
			for index, state := range []string{"current", "recommended"} {
				if values[index] > 0 {
					_, _ = fmt.Fprintf(w, "%s{%s,state=\"%s\"} %d\n", item.name, labels, state, values[index])
				}
			}
		}
	}
	_, _ = fmt.Fprintln(w, "# HELP kuberight_recommendation_info Review status, risk, and confidence of each container's newest recommendation.")
	_, _ = fmt.Fprintln(w, "# TYPE kuberight_recommendation_info gauge")
	for _, key := range keys {
		record := latest[key]
		_, _ = fmt.Fprintf(w, "kuberight_recommendation_info{cluster=\"%s\",namespace=\"%s\",kind=\"%s\",workload=\"%s\",container=\"%s\",review_status=\"%s\",risk=\"%s\",confidence=\"%s\",source=\"%s\"} 1\n",
			prometheusLabel(record.Cluster), prometheusLabel(record.Namespace), prometheusLabel(record.Kind), prometheusLabel(record.Workload), prometheusLabel(record.Container),
			prometheusLabel(record.ReviewStatus), prometheusLabel(record.Risk), prometheusLabel(record.Confidence), prometheusLabel(record.Source))
	}
	if server.config.CostConfigured() {
		_, _ = fmt.Fprintln(w, "# HELP kuberight_recommendation_monthly_savings Estimated monthly change in requested cost; negative values are increases.")
		_, _ = fmt.Fprintln(w, "# TYPE kuberight_recommendation_monthly_savings gauge")
		for _, key := range keys {
			record := latest[key]
			replicas := max(record.Replicas, 1)
			savings := server.monthlyCost(record.CurrentCPURequestMilli, record.CurrentMemoryRequestBytes, replicas) - server.monthlyCost(record.CPURequestMilli, record.MemoryRequestBytes, replicas)
			_, _ = fmt.Fprintf(w, "kuberight_recommendation_monthly_savings{cluster=\"%s\",namespace=\"%s\",kind=\"%s\",workload=\"%s\",container=\"%s\",currency=\"%s\"} %s\n",
				prometheusLabel(record.Cluster), prometheusLabel(record.Namespace), prometheusLabel(record.Kind), prometheusLabel(record.Workload), prometheusLabel(record.Container),
				prometheusLabel(server.config.CostCurrency), strconv.FormatFloat(roundCents(savings), 'f', -1, 64))
		}
	}
}
