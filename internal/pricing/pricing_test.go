package pricing

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// allocationResponse mirrors OpenCost's /allocation shape with
// aggregate=namespace and accumulate=true.
const allocationResponse = `{"code":200,"data":[{
  "shop": {"name":"shop","cpuCoreHours":336,"cpuCost":10.08,"ramByteHours":721554505728,"ramCost":2.688,"totalCost":12.768},
  "payments": {"name":"payments","cpuCoreHours":168,"cpuCost":8.4,"ramByteHours":0,"ramCost":0},
  "__idle__": {"name":"__idle__","cpuCoreHours":1000,"cpuCost":1000,"ramByteHours":1073741824000,"ramCost":1000}
}]}`

func TestOpenCostDerivesNamespacePricesAndCaches(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/allocation" || r.URL.Query().Get("aggregate") != "namespace" || r.URL.Query().Get("accumulate") != "true" || r.URL.Query().Get("window") != "168h" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		_, _ = w.Write([]byte(allocationResponse))
	}))
	defer server.Close()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	opencost, err := NewOpenCost(OpenCostOptions{Endpoint: server.URL, AllowHTTP: true, Fallback: Static{CPUCoreHour: 0.05, MemoryGiBHour: 0.01}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	shop := opencost.Prices(context.Background(), "shop")
	// 10.08 / 336 = 0.03 per core-hour; 2.688 / 672 GiB-hours = 0.004.
	if shop.Source != SourceOpenCost || math.Abs(shop.CPUCoreHour-0.03) > 1e-9 || math.Abs(shop.MemoryGiBHour-0.004) > 1e-9 {
		t.Fatalf("shop prices = %+v", shop)
	}
	// payments has no memory allocation, so the cluster blend applies;
	// __idle__ is excluded from the blend.
	payments := opencost.Prices(context.Background(), "payments")
	if payments.Source != SourceOpenCostCluster || math.Abs(payments.CPUCoreHour-18.48/504) > 1e-9 || math.Abs(payments.MemoryGiBHour-0.004) > 1e-9 {
		t.Fatalf("payments prices = %+v", payments)
	}
	opencost.Prices(context.Background(), "shop")
	if calls.Load() != 1 {
		t.Fatalf("allocation fetched %d times inside the cache TTL", calls.Load())
	}
	now = now.Add(16 * time.Minute)
	opencost.Prices(context.Background(), "shop")
	if calls.Load() != 2 {
		t.Fatalf("allocation not refreshed after the TTL: %d calls", calls.Load())
	}
}

func TestOpenCostFailuresFallBackToConfiguredPrices(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"malformed":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) },
		"no prices":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"code":200,"data":[{}]}`)) },
		"unit error": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"code":200,"data":[{"shop":{"cpuCoreHours":0.001,"cpuCost":5000,"ramByteHours":1073741824,"ramCost":1}}]}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			opencost, err := NewOpenCost(OpenCostOptions{Endpoint: server.URL, AllowHTTP: true, Fallback: Static{CPUCoreHour: 0.05, MemoryGiBHour: 0.01}})
			if err != nil {
				t.Fatal(err)
			}
			if prices := opencost.Prices(context.Background(), "shop"); prices.Source != SourceConfigured || prices.CPUCoreHour != 0.05 {
				t.Fatalf("prices = %+v", prices)
			}
			if _, lastError := opencost.Status(); lastError == nil && name != "unit error" {
				t.Fatal("the failure was not recorded")
			}
		})
	}
}

func TestOpenCostEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"", "opencost:9003", "http://opencost:9003", "https://user:pass@opencost", "https://opencost/?x=1", "ftp://opencost"} {
		if _, err := NewOpenCost(OpenCostOptions{Endpoint: endpoint}); err == nil {
			t.Fatalf("%q was accepted", endpoint)
		}
	}
	if _, err := NewOpenCost(OpenCostOptions{Endpoint: "http://opencost.opencost.svc:9003", AllowHTTP: true}); err != nil {
		t.Fatalf("explicitly allowed in-cluster HTTP was rejected: %v", err)
	}
	if prices := (Static{CPUCoreHour: 0.04}).Prices(context.Background(), "x"); prices.Source != SourceConfigured || !prices.Available() {
		t.Fatalf("static prices = %+v", prices)
	}
}

// liveAllocation is a trimmed OpenCost 1.121.4 response from a kind cluster
// with default on-prem pricing, including the negative values OpenCost
// reports for a namespace whose pods had just started.
const liveAllocation = `{"code":200,"data":[{
  "shop": {"name":"shop","cpuCoreHours":1.738,"cpuCost":0.05493,"ramByteHours":618200000,"ramCost":0.00244},
  "opencost": {"name":"opencost","cpuCoreHours":-0.00184,"cpuCost":-0.00006,"ramByteHours":-4929000,"ramCost":-0.00002},
  "monitoring": {"name":"monitoring","cpuCoreHours":0,"cpuCost":0,"ramByteHours":0,"ramCost":0}
}]}`

func TestOpenCostLiveResponseShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(liveAllocation)) }))
	defer server.Close()
	opencost, err := NewOpenCost(OpenCostOptions{Endpoint: server.URL, AllowHTTP: true, Fallback: Static{CPUCoreHour: 1, MemoryGiBHour: 1}})
	if err != nil {
		t.Fatal(err)
	}
	shop := opencost.Prices(context.Background(), "shop")
	// OpenCost's default on-prem prices: about 0.0316 per core-hour and
	// 0.0042 per GiB-hour.
	if shop.Source != SourceOpenCost || math.Abs(shop.CPUCoreHour-0.0316) > 0.0001 || math.Abs(shop.MemoryGiBHour-0.00424) > 0.00002 {
		t.Fatalf("shop = %+v", shop)
	}
	// Negative and zero allocations get the cluster blend, which is not
	// distorted by the negative values.
	for _, namespace := range []string{"opencost", "monitoring"} {
		if prices := opencost.Prices(context.Background(), namespace); prices.Source != SourceOpenCostCluster || math.Abs(prices.CPUCoreHour-shop.CPUCoreHour) > 1e-9 {
			t.Fatalf("%s = %+v", namespace, prices)
		}
	}
}
