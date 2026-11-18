package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/gitops"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

type fakeGitOps struct {
	requests []gitops.Request
	edited   []byte
	err      error
	// content replaces the default manifest in Git.
	content []byte
}

func (*fakeGitOps) Name() string { return "fake" }

func (provider *fakeGitOps) OpenPullRequest(_ context.Context, request gitops.Request) (gitops.Result, error) {
	provider.requests = append(provider.requests, request)
	if provider.err != nil {
		return gitops.Result{}, provider.err
	}
	content := provider.content
	if content == nil {
		content = []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout
spec:
  template:
    spec:
      containers:
        - name: api
          resources:
            requests:
              cpu: "1"
              memory: 1Gi
            limits:
              cpu: "2"
              memory: 2Gi
`)
	}
	edited, err := request.Update(content)
	if err != nil {
		return gitops.Result{}, err
	}
	provider.edited = edited
	return gitops.Result{URL: "https://git.example/acme/deploy/pull/7", Number: 7, Branch: request.Branch}, nil
}

func approvedCheckout(t *testing.T, store repository.Store) repository.RecommendationRecord {
	t.Helper()
	recommendation := optimizer.Recommendation{
		Cluster: "prod", Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api",
		CPURequestMilli: 800, CPULimitMilli: 1600, MemoryRequestBytes: 820 << 20, MemoryLimitBytes: 1640 << 20,
		CurrentCPURequestMilli: 1000, CurrentCPULimitMilli: 2000, CurrentMemoryRequestBytes: 1 << 30, CurrentMemoryLimitBytes: 2 << 30,
		Confidence: "high", Risk: "low", Warnings: []string{}, DataQuality: optimizer.DataQuality{Issues: []string{}},
	}
	record, _, err := store.CreateRecommendation(context.Background(), recommendation, repository.CreateOptions{CreatedBy: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if record, err = store.ReviewRecommendation(context.Background(), record.ID, repository.ReviewDecision{Status: repository.ReviewApproved, Reviewer: "bob", At: fixedNow}); err != nil {
		t.Fatal(err)
	}
	return record
}

func gitopsSpec(annotations map[string]string) WorkloadSpec {
	spec := checkoutSpec()
	spec.Annotations = annotations
	return spec
}

func TestPullRequestEditsTheAnnotatedManifest(t *testing.T) {
	provider := &fakeGitOps{}
	api := newTestAPI(t, nil, Dependencies{GitOps: provider, Provider: fakeInventory{spec: gitopsSpec(map[string]string{
		GitOpsPathAnnotation: "apps/shop/checkout.yaml", GitOpsRepositoryAnnotation: "acme/deploy",
	})}})
	record := approvedCheckout(t, api.store)
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", "")
	if response.Code != http.StatusCreated || body["url"] != "https://git.example/acme/deploy/pull/7" || body["branch"] != "kuberight/"+record.ID || body["baseBranch"] != "main" {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	request := provider.requests[0]
	if request.Repository != "acme/deploy" || request.Path != "apps/shop/checkout.yaml" || !strings.Contains(request.Body, "| CPU request | 1 | 800m |") {
		t.Fatalf("request: %+v", request)
	}
	for _, want := range []string{`cpu: "800m"`, "memory: 820Mi", `cpu: "1600m"`, "memory: 1640Mi"} {
		if !strings.Contains(string(provider.edited), want) {
			t.Fatalf("edited manifest lacks %q:\n%s", want, provider.edited)
		}
	}
	last := api.access.calls[len(api.access.calls)-1]
	if last.Verb != "create" || last.Resource != "recommendations" || last.Subresource != "pullrequest" || last.Namespace != "shop" {
		t.Fatalf("authorization: %+v", last)
	}
	audit, _ := api.store.ListAuditEvents(context.Background(), 1, 10)
	if audit.Items[0].Action != "gitops.pull_request" || audit.Items[0].Outcome != "success" || audit.Items[0].Actor != "carol" {
		t.Fatalf("audit: %+v", audit.Items[0])
	}
}

func TestPullRequestRequiresApprovalTargetAndProvider(t *testing.T) {
	annotated := fakeInventory{spec: gitopsSpec(map[string]string{GitOpsPathAnnotation: "apps/checkout.yaml"})}
	// No provider configured.
	api := newTestAPI(t, nil, Dependencies{Provider: annotated})
	record := approvedCheckout(t, api.store)
	if response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", ""); response.Code != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != "gitops_not_configured" {
		t.Fatalf("no provider: %d %v", response.Code, body)
	}
	// No repository: neither an annotation nor a default.
	api = newTestAPI(t, nil, Dependencies{GitOps: &fakeGitOps{}, Provider: annotated})
	record = approvedCheckout(t, api.store)
	if response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", ""); response.Code != http.StatusConflict || body["error"].(map[string]any)["code"] != "gitops_target_missing" {
		t.Fatalf("no repository: %d %v", response.Code, body)
	}
	// A default repository applies; a path escaping the repository is refused.
	escaping := fakeInventory{spec: gitopsSpec(map[string]string{GitOpsPathAnnotation: "../secrets.yaml"})}
	api = newTestAPI(t, func(cfg *config.Config) { cfg.GitOpsRepository = "acme/deploy" }, Dependencies{GitOps: &fakeGitOps{}, Provider: escaping})
	record = approvedCheckout(t, api.store)
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("escaping path: %d", response.Code)
	}
	// Pending recommendations are not proposed.
	api = newTestAPI(t, nil, Dependencies{GitOps: &fakeGitOps{}, Provider: annotated})
	pending, _, _ := api.store.CreateRecommendation(context.Background(), optimizer.Recommendation{Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api", CPURequestMilli: 100, MemoryRequestBytes: 64 << 20, Confidence: "low", Risk: "low", Warnings: []string{}, DataQuality: optimizer.DataQuality{Issues: []string{}}}, repository.CreateOptions{})
	if response, _ := api.do(t, http.MethodPost, "/api/v1/recommendations/"+pending.ID+"/pull-request", "carol", ""); response.Code != http.StatusConflict {
		t.Fatalf("pending: %d", response.Code)
	}
}

func TestPullRequestMapsProviderOutcomes(t *testing.T) {
	inventory := fakeInventory{spec: gitopsSpec(map[string]string{GitOpsPathAnnotation: "apps/checkout.yaml", GitOpsRepositoryAnnotation: "acme/deploy"})}
	for name, test := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"already current": {gitops.ErrNoChange, http.StatusConflict, "manifest_current"},
		"wrong file":      {gitops.ErrTargetNotFound, http.StatusUnprocessableEntity, "manifest_target_missing"},
		"provider error":  {&gitops.ProviderError{Operation: "commit manifest", Status: 403, Message: "forbidden"}, http.StatusBadGateway, "gitops_provider_error"},
		"network":         {errors.New("dial tcp: timeout"), http.StatusBadGateway, "gitops_unavailable"},
	} {
		api := newTestAPI(t, nil, Dependencies{GitOps: &fakeGitOps{err: test.err}, Provider: inventory})
		record := approvedCheckout(t, api.store)
		response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", "")
		if response.Code != test.status || body["error"].(map[string]any)["code"] != test.code {
			t.Errorf("%s: %d %v", name, response.Code, body)
		}
		audit, _ := api.store.ListAuditEvents(context.Background(), 1, 10)
		if audit.Items[0].Outcome != "failure" {
			t.Errorf("%s: audit outcome %s", name, audit.Items[0].Outcome)
		}
	}
}

func TestPullRequestEditsAnAnnotatedHelmValuesKey(t *testing.T) {
	provider := &fakeGitOps{content: []byte("api:\n  image: ghcr.io/acme/api:1\n  resources: {}\n")}
	api := newTestAPI(t, nil, Dependencies{GitOps: provider, Provider: fakeInventory{spec: gitopsSpec(map[string]string{
		GitOpsPathAnnotation: "charts/shop/values-prod.yaml", GitOpsRepositoryAnnotation: "acme/deploy", GitOpsValuesKeyAnnotation: "api.resources",
	})}})
	record := approvedCheckout(t, api.store)
	response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", "")
	if response.Code != http.StatusCreated || body["valuesKey"] != "api.resources" {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
	want := "api:\n  image: ghcr.io/acme/api:1\n  resources:\n    requests:\n      cpu: 800m\n      memory: 820Mi\n    limits:\n      cpu: 1600m\n      memory: 1640Mi\n"
	if string(provider.edited) != want {
		t.Fatalf("edited values:\n%s", provider.edited)
	}
	missing := &fakeGitOps{content: []byte("worker:\n  replicas: 2\n")}
	api = newTestAPI(t, nil, Dependencies{GitOps: missing, Provider: fakeInventory{spec: gitopsSpec(map[string]string{
		GitOpsPathAnnotation: "values.yaml", GitOpsRepositoryAnnotation: "acme/deploy", GitOpsValuesKeyAnnotation: "api.resources",
	})}})
	record = approvedCheckout(t, api.store)
	if response, body := api.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", ""); response.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(body["error"].(map[string]any)["message"].(string), "no mapping for values key api.resources") {
		t.Fatalf("missing parent: status=%d body=%v", response.Code, body)
	}
	invalid := newTestAPI(t, nil, Dependencies{GitOps: &fakeGitOps{}, Provider: fakeInventory{spec: gitopsSpec(map[string]string{
		GitOpsPathAnnotation: "values.yaml", GitOpsRepositoryAnnotation: "acme/deploy", GitOpsValuesKeyAnnotation: "api.containers[0]",
	})}})
	record = approvedCheckout(t, invalid.store)
	if response, _ := invalid.do(t, http.MethodPost, "/api/v1/recommendations/"+record.ID+"/pull-request", "carol", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid key: status=%d", response.Code)
	}
}
