package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/repository"
)

// KubeRight has no sign-in, like Goldilocks: every request is served, with
// or without credentials, and nothing ever asks for them.
func TestEveryRequestIsServedWithoutSignIn(t *testing.T) {
	handler, err := NewHandler(fallbackConfig(), Dependencies{Repository: repository.NewMemory(100)})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "Bearer not-a-real-token", "Basic Zm9vOmJhcg=="} {
		for _, path := range []string{"/api/v1/version", "/api/v1/overview", "/api/v1/recommendations?namespace=payments"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if header != "" {
				request.Header.Set("Authorization", header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s with Authorization %q: status %d body %s", path, header, response.Code, response.Body.String())
			}
			if response.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("%s must not ask for credentials", path)
			}
		}
	}
}

// Recommendations created and reviewed without sign-in are recorded as the
// anonymous identity, and the same caller may review what it created.
func TestAnonymousCallerCreatesAndReviewsRecommendations(t *testing.T) {
	handler, err := NewHandler(fallbackConfig(), Dependencies{Repository: repository.NewMemory(100), Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if method == http.MethodPost {
			request.Header.Set("Idempotency-Key", "noauth-"+strings.ReplaceAll(path, "/", "-"))
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		decoded := map[string]any{}
		_ = json.Unmarshal(response.Body.Bytes(), &decoded)
		return response, decoded
	}
	response, created := send(http.MethodPost, "/api/v1/recommendations", `{"namespace":"shop","workload":"api","container":"app","cpuRequestMilli":500,"memoryRequestBytes":536870912,"cpuSamplesMilli":[100,120],"memorySamplesBytes":[200000000,210000000]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %v", response.Code, created)
	}
	if created["createdBy"] != anonymousPrincipal.Username {
		t.Fatalf("createdBy = %v, want %s", created["createdBy"], anonymousPrincipal.Username)
	}
	response, reviewed := send(http.MethodPost, "/api/v1/recommendations/"+created["id"].(string)+"/review", `{"decision":"reject","comment":"checked by hand"}`)
	if response.Code != http.StatusOK || reviewed["reviewedBy"] != anonymousPrincipal.Username {
		t.Fatalf("review: status %d body %v", response.Code, reviewed)
	}
}

func TestOpenAccessAllowsEverything(t *testing.T) {
	principal, err := openAccess{}.Authenticate(t.Context(), "")
	if err != nil || principal.Username != "system:anonymous" {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	if err := (openAccess{}).Authorize(t.Context(), principal, AccessAttributes{Verb: "update", Resource: "recommendations"}); err != nil {
		t.Fatal(err)
	}
}
