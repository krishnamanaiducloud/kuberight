package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kuberight/kuberight/internal/repository"
)

func TestDisabledAuthenticationServesEveryRequestAnonymously(t *testing.T) {
	cfg := fallbackConfig()
	cfg.AuthMode, cfg.APIToken = "disabled", ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(cfg, Dependencies{Repository: repository.NewMemory(100)})
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

func TestDisabledAccessAllowsEverything(t *testing.T) {
	principal, err := disabledAccess{}.Authenticate(t.Context(), "")
	if err != nil || principal.Username != "system:anonymous" {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	if err := (disabledAccess{}).Authorize(t.Context(), principal, AccessAttributes{Verb: "update", Resource: "recommendations"}); err != nil {
		t.Fatal(err)
	}
}
