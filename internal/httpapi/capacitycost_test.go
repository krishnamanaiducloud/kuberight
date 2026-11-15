package httpapi

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

// capacityCostAPI serves six 1500m pods on three 3500m nodes, with an
// approved recommendation of 750m that frees one node.
func capacityCostAPI(t *testing.T, mutate func(*config.Config)) testAPI {
	t.Helper()
	records := []map[string]any{
		{"cluster": "local", "namespace": "shop", "kind": "Deployment", "name": "api", "uid": "uid-api", "generation": int64(7), "sourceSpecFingerprint": strings.Repeat("a", 64), "desiredReplicas": int32(6),
			"containers": []map[string]any{{"name": "app", "cpuRequestMilli": int64(1500), "memoryRequestBytes": int64(1 << 30), "cpuLimitMilli": int64(0), "memoryLimitBytes": int64(0)}}},
	}
	cluster := ClusterCapacity{Autoscaling: NodeAutoscaling{Karpenter: true, NodePools: []NodePool{{Name: "default", ConsolidationPolicy: "WhenEmptyOrUnderutilized"}}}}
	for _, name := range []string{"a", "b", "c"} {
		cluster.Nodes = append(cluster.Nodes, NodeCapacity{Name: name, CPUMilli: 3500, MemoryBytes: 16 << 30, Schedulable: true, NodeGroup: "default", NodeGroupProvider: NodeGroupKarpenter, NodeGroupMembership: MembershipVerified})
	}
	api := newTestAPI(t, mutate, Dependencies{Provider: capacityInventory{inventoryRecords: inventoryRecords{records: records}, cluster: cluster}})
	start, end, expiry := fixedNow.Add(-24*time.Hour), fixedNow.Add(-time.Minute), fixedNow.Add(time.Hour)
	created, _, err := api.store.CreateRecommendation(context.Background(), optimizer.Recommendation{
		Cluster: "local", Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "app", Confidence: "high", Risk: "low",
		CurrentCPURequestMilli: 1500, CurrentMemoryRequestBytes: 1 << 30, CPURequestMilli: 750, MemoryRequestBytes: 1 << 30,
		Provenance: optimizer.Provenance{WorkloadUID: "uid-api", WorkloadGeneration: 7, SourceSpecFingerprint: strings.Repeat("a", 64),
			ObservationStart: &start, ObservationEnd: &end, ExpiresAt: &expiry,
			QueryFingerprint: strings.Repeat("b", 64), PolicyFingerprint: strings.Repeat("c", 64), OptimizerVersion: optimizer.AlgorithmVersion},
	}, repository.CreateOptions{Source: repository.SourcePrometheus, CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.store.ReviewRecommendation(context.Background(), created.ID, repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "bob", At: fixedNow}); err != nil {
		t.Fatal(err)
	}
	return api
}

func TestCapacitySeparatesRequestNodeAndBillingSavings(t *testing.T) {
	api := capacityCostAPI(t, func(cfg *config.Config) { cfg.CostCPUCoreHour, cfg.CostMemoryGiBHour = 0.04, 0.005 })
	response, body := api.do(t, http.MethodGet, "/api/v1/capacity", "alice", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	tiers := body["costTiers"].(map[string]any)
	requests := tiers["requestReduction"].(map[string]any)
	nodes := tiers["feasibleNodeSavings"].(map[string]any)
	billing := tiers["billing"].(map[string]any)
	// 6 x 750m less CPU: 4.5 cores x 0.04 x 730 h.
	if tiers["available"] != true || math.Abs(requests["monthly"].(float64)-131.4) > 0.01 || !strings.Contains(requests["detail"].(string), "do not change the bill") {
		t.Fatalf("request reduction=%v", requests)
	}
	// One freed 3500m, 16Gi node: (3.5 x 0.04 + 16 x 0.005) x 730 h.
	if nodes["nodes"].(float64) != 1 || math.Abs(nodes["monthly"].(float64)-160.6) > 0.01 {
		t.Fatalf("node savings=%v", nodes)
	}
	if billing["measured"] != false || tiers["currency"] != "USD" {
		t.Fatalf("billing=%v tiers=%v", billing, tiers)
	}
	_, unpriced := capacityCostAPI(t, nil).do(t, http.MethodGet, "/api/v1/capacity", "alice", "")
	if tiers := unpriced["costTiers"].(map[string]any); tiers["available"] != false || tiers["billing"].(map[string]any)["measured"] != false {
		t.Fatalf("unpriced tiers=%v", tiers)
	}
}
