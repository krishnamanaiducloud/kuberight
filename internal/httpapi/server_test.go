package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type identityAccess struct{}

func (identityAccess) Authenticate(_ context.Context, token string) (Principal, error) {
	return Principal{Username: token}, nil
}

func (identityAccess) Authorize(context.Context, Principal, AccessAttributes) error { return nil }
func (identityAccess) Ready(context.Context) error                                  { return nil }

func TestRecommendationIdempotencyIsIdentityScopedAndPayloadBound(t *testing.T) {
	cfg := fallbackConfig()
	handler, err := NewHandler(cfg, Dependencies{Access: identityAccess{}})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	firstBody := `{"namespace":"shop","workload":"api","container":"app","cpuRequestMilli":500,"memoryRequestBytes":536870912,"cpuSamplesMilli":[100,120],"memorySamplesBytes":[200000000,210000000]}`
	changedBody := `{"namespace":"shop","workload":"worker","container":"app","cpuRequestMilli":500,"memoryRequestBytes":536870912,"cpuSamplesMilli":[100,120],"memorySamplesBytes":[200000000,210000000]}`

	if status := recommendationRequest(t, handler, "alice", "shared-key", firstBody); status != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", status, http.StatusCreated)
	}
	if status := recommendationRequest(t, handler, "alice", "shared-key", firstBody); status != http.StatusOK {
		t.Fatalf("replay status = %d, want %d", status, http.StatusOK)
	}
	if status := recommendationRequest(t, handler, "alice", "shared-key", changedBody); status != http.StatusConflict {
		t.Fatalf("changed payload status = %d, want %d", status, http.StatusConflict)
	}
	if status := recommendationRequest(t, handler, "bob", "shared-key", changedBody); status != http.StatusCreated {
		t.Fatalf("second identity status = %d, want %d", status, http.StatusCreated)
	}
}

func TestAuthenticatedRequestLogIncludesSubject(t *testing.T) {
	var logs bytes.Buffer
	cfg := fallbackConfig()
	handler, err := NewHandler(cfg, Dependencies{
		Access: identityAccess{},
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	request.Header.Set("Authorization", "Bearer alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(logs.String(), `"subject":"alice"`) {
		t.Fatalf("authenticated subject missing from request log: %s", logs.String())
	}
}

func TestSavingsEndpointReportsUnavailableWithoutOpenCost(t *testing.T) {
	cfg := fallbackConfig()
	handler, err := NewHandler(cfg, Dependencies{Access: identityAccess{}})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/savings?page=1&pageSize=25&sort=name", nil)
	request.Header.Set("Authorization", "Bearer alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var envelope struct {
		Items  []any        `json:"items"`
		Status Availability `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Status.Available {
		t.Fatal("savings must not be reported as available without an OpenCost provider")
	}
	if len(envelope.Items) != 0 {
		t.Fatalf("unverified savings items = %d, want 0", len(envelope.Items))
	}
}

func recommendationRequest(t *testing.T, handler http.Handler, identity, key, body string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/recommendations", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+identity)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code
}
