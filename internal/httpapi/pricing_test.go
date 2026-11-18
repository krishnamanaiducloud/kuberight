package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/pricing"
	"github.com/kuberight/kuberight/internal/repository"
)

type namespacePricer map[string]pricing.Prices

func (prices namespacePricer) Prices(_ context.Context, namespace string) pricing.Prices {
	if value, ok := prices[namespace]; ok {
		return value
	}
	return pricing.Prices{CPUCoreHour: 0.04, MemoryGiBHour: 0.005, Source: pricing.SourceConfigured}
}

func TestSavingsUsePerNamespacePricesAndReportTheirSource(t *testing.T) {
	pricer := namespacePricer{"shop": {CPUCoreHour: 0.08, MemoryGiBHour: 0.01, Source: pricing.SourceOpenCost}}
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.OpenCostURL, cfg.AllowInsecureUpstreams, cfg.CostCurrency = "http://opencost.opencost.svc:9003", true, "USD"
	}, Dependencies{Pricer: pricer})
	fresh := fixedNow.Add(time.Hour)
	for _, namespace := range []string{"shop", "billing"} {
		recommendation := optimizer.Recommendation{
			Namespace: namespace, Kind: "Deployment", Workload: "api", Container: "app", Replicas: 1, Confidence: "high", Risk: "low",
			CurrentCPURequestMilli: 1000, CurrentMemoryRequestBytes: 2 << 30, CPURequestMilli: 500, MemoryRequestBytes: 1 << 30,
			Provenance: optimizer.Provenance{ExpiresAt: &fresh},
		}
		if _, _, err := api.store.CreateRecommendation(context.Background(), recommendation, repository.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	response, body := api.do(t, http.MethodGet, "/api/v1/savings?pageSize=25", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	byNamespace := map[string]map[string]any{}
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		byNamespace[item["namespace"].(string)] = item
	}
	// shop: (0.5 core x 0.08 + 1 GiB x 0.01) x 730 = 36.5; billing: (0.02 + 0.005) x 730 = 18.25.
	if shop := byNamespace["shop"]; shop["theoreticalMonthlyChange"].(float64) != 36.5 || shop["priceSource"] != pricing.SourceOpenCost {
		t.Fatalf("shop = %v", shop)
	}
	if billing := byNamespace["billing"]; billing["theoreticalMonthlyChange"].(float64) != 18.25 || billing["priceSource"] != pricing.SourceConfigured {
		t.Fatalf("billing = %v", billing)
	}
	summary := body["summary"].(map[string]any)
	if summary["theoreticalMonthlyChange"].(float64) != 54.75 || summary["realized"].(map[string]any)["available"] != false {
		t.Fatalf("summary = %v", summary)
	}
}

func TestSavingsReportUnpricedCostsAsUnavailableNotZero(t *testing.T) {
	// OpenCost is configured but has no price for this namespace, and no
	// fallback prices are set.
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.OpenCostURL, cfg.AllowInsecureUpstreams = "http://opencost.opencost.svc:9003", true
	}, Dependencies{Pricer: namespacePricer{"shop": {Source: pricing.SourceConfigured}}})
	fresh := fixedNow.Add(time.Hour)
	recommendation := optimizer.Recommendation{
		Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "app", Replicas: 2, Confidence: "high", Risk: "low",
		CurrentCPURequestMilli: 1000, CurrentMemoryRequestBytes: 2 << 30, CPURequestMilli: 500, MemoryRequestBytes: 1 << 30,
		Provenance: optimizer.Provenance{ExpiresAt: &fresh},
	}
	if _, _, err := api.store.CreateRecommendation(context.Background(), recommendation, repository.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, body := api.do(t, http.MethodGet, "/api/v1/savings?pageSize=25", "alice", "")
	item := body["items"].([]any)[0].(map[string]any)
	if item["costAvailable"] != false || item["currentMonthlyCost"] != nil || !strings.Contains(item["costUnavailableReason"].(string), "OpenCost") {
		t.Fatalf("an unpriced item was reported as a cost: %v", item)
	}
	if body["status"].(map[string]any)["source"] != "opencost_unit_prices" {
		t.Fatalf("status = %v", body["status"])
	}
}
