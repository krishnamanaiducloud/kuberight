package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

const apiGroup = "kuberight.io"

type Availability struct {
	Available bool   `json:"available"`
	Source    string `json:"source"`
	Reason    string `json:"reason,omitempty"`
}

type CollectionResult struct {
	Items  any
	Total  int
	Status Availability
}

type CollectionProvider interface {
	List(context.Context, string, CollectionQuery) (CollectionResult, error)
	Ready(context.Context) error
}

type CollectionQuery struct {
	Page      int
	PageSize  int
	Namespace string
	Cluster   string
	Filter    string
	Sort      string
	Order     string
}

type Dependencies struct {
	Access     AccessController
	Repository *repository.Memory
	Provider   CollectionProvider
	Logger     *slog.Logger
}

type apiServer struct {
	config     config.Config
	access     AccessController
	repository *repository.Memory
	provider   CollectionProvider
	logger     *slog.Logger
	metrics    *apiMetrics
}

type unavailableAccess struct{ err error }

func (access unavailableAccess) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, newAccessError(http.StatusServiceUnavailable, "authentication_unavailable", "identity verification is unavailable")
}
func (access unavailableAccess) Authorize(context.Context, Principal, AccessAttributes) error {
	return newAccessError(http.StatusServiceUnavailable, "authorization_unavailable", "permission evaluation is unavailable")
}
func (access unavailableAccess) Ready(context.Context) error { return access.err }

// NewHandler builds the API and returns an error rather than silently starting
// with an invalid security configuration.
func NewHandler(cfg config.Config, dependencies Dependencies) (http.Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	access := dependencies.Access
	if access == nil {
		var err error
		access, err = configuredAccess(cfg)
		if err != nil {
			return nil, err
		}
	}
	repo := dependencies.Repository
	if repo == nil {
		repo = repository.NewMemory(10_000)
	}
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.Default()
	}
	server := &apiServer{
		config:     cfg,
		access:     access,
		repository: repo,
		provider:   dependencies.Provider,
		logger:     logger,
		metrics:    newAPIMetrics(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /readyz", server.ready)
	mux.HandleFunc("GET /metrics", server.prometheusMetrics)
	mux.HandleFunc("GET /api/v1/version", server.version)
	mux.HandleFunc("GET /api/v1/overview", server.overview)
	mux.HandleFunc("GET /api/v1/recommendations", server.listRecommendations)
	mux.HandleFunc("POST /api/v1/recommendations", server.calculateRecommendation)
	mux.HandleFunc("POST /api/v1/recommendations:calculate", server.calculateRecommendation)
	mux.HandleFunc("GET /api/v1/recommendations/{id}", server.getRecommendation)
	mux.HandleFunc("GET /api/v1/policies", server.policies)
	mux.HandleFunc("GET /api/v1/audit", server.audit)
	mux.HandleFunc("GET /api/v1/settings/integrations", server.integrations)
	for _, resource := range []string{"clusters", "namespaces", "workloads", "metrics/history", "savings", "forecasts", "autoscalers", "approvals", "gitops/changes"} {
		resource := resource
		mux.HandleFunc("GET /api/v1/"+resource, func(w http.ResponseWriter, r *http.Request) {
			server.collection(w, r, resource)
		})
	}
	if cfg.WebRoot != "" {
		spa, err := newSPAHandler(cfg.WebRoot)
		if err != nil {
			return nil, err
		}
		mux.Handle("/", spa)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			respondError(w, r, http.StatusNotFound, "not_found", "the requested resource was not found")
		})
	}

	var handler http.Handler = mux
	handler = server.withAuthentication(handler)
	handler = server.withRequestLimits(handler)
	handler = withRecovery(handler, logger)
	handler = withTelemetry(handler, logger, server.metrics)
	handler = withSecurityHeaders(handler)
	handler = withRequestID(handler)
	return handler, nil
}

// Handler preserves the original package entry point for embedders. Invalid
// configuration remains observable via /readyz and fails closed for all APIs.
func Handler() http.Handler {
	cfg, err := config.Load()
	if err == nil {
		if handler, handlerErr := NewHandler(cfg, Dependencies{}); handlerErr == nil {
			return handler
		} else {
			err = handlerErr
		}
	}
	fallback := fallbackConfig()
	handler, _ := NewHandler(fallback, Dependencies{Access: unavailableAccess{err: err}})
	return handler
}

func fallbackConfig() config.Config {
	return config.Config{
		ListenAddress:     ":8080",
		AuthMode:          "static",
		APIToken:          strings.Repeat("x", 32),
		OperationMode:     config.ModeRecommend,
		MaxBodyBytes:      1 << 20,
		RequestTimeout:    15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		ShutdownTimeout:   20 * time.Second,
		ClusterID:         "local",
	}
}

func NewHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}
}

// Server preserves the original constructor while new code should use
// NewHTTPServer with a fully validated Config.
func Server(address string) *http.Server {
	cfg := fallbackConfig()
	cfg.ListenAddress = address
	return NewHTTPServer(cfg, Handler())
}

func (server *apiServer) withRequestLimits(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > server.config.MaxBodyBytes {
			respondError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, server.config.MaxBodyBytes)
		}
		ctx, cancel := context.WithTimeout(r.Context(), server.config.RequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isPublicPath(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/metrics" || !strings.HasPrefix(path, "/api/")
}

func (server *apiServer) withAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		bearer, ok := extractBearer(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kuberight"`)
			respondError(w, r, http.StatusUnauthorized, "unauthorized", "a bearer token is required")
			return
		}
		principal, err := server.access.Authenticate(r.Context(), bearer)
		if err != nil {
			status, code, message := accessErrorDetails(err)
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="kuberight", error="invalid_token"`)
			}
			respondError(w, r, status, code, message)
			return
		}
		recordTelemetrySubject(r.Context(), principal.Username)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, principal)))
	})
}

func (server *apiServer) authorize(w http.ResponseWriter, r *http.Request, attributes AccessAttributes) bool {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		respondError(w, r, http.StatusUnauthorized, "unauthorized", "an authenticated identity is required")
		return false
	}
	if attributes.APIGroup == "" {
		attributes.APIGroup = apiGroup
	}
	if err := server.access.Authorize(r.Context(), principal, attributes); err != nil {
		status, code, message := accessErrorDetails(err)
		respondError(w, r, status, code, message)
		return false
	}
	return true
}

func parsePagination(r *http.Request) (int, int, error) {
	page, pageSize := 1, 50
	var err error
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1_000_000 {
			return 0, 0, errors.New("page must be an integer between 1 and 1000000")
		}
	}
	if raw := r.URL.Query().Get("pageSize"); raw != "" {
		pageSize, err = strconv.Atoi(raw)
		if err != nil || pageSize < 1 || pageSize > 200 {
			return 0, 0, errors.New("pageSize must be an integer between 1 and 200")
		}
	}
	return page, pageSize, nil
}

func resourceName(resource string) string {
	return strings.ReplaceAll(resource, "/", "-")
}

func (server *apiServer) prometheusMetrics(w http.ResponseWriter, _ *http.Request) {
	server.metrics.render(w)
}

func (server *apiServer) health(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (server *apiServer) ready(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"authentication": "ready"}
	ready := true
	if err := server.access.Ready(r.Context()); err != nil {
		checks["authentication"] = "unavailable"
		ready = false
	}
	if server.provider != nil {
		if err := server.provider.Ready(r.Context()); err != nil {
			checks["dataProvider"] = "unavailable"
			ready = false
		} else {
			checks["dataProvider"] = "ready"
		}
	} else {
		checks["dataProvider"] = "not_configured"
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	respondJSON(w, status, map[string]any{"status": state, "checks": checks})
}

func (server *apiServer) version(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "version"}) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{
		"version": Version, "commit": Commit, "buildDate": BuildDate, "mode": server.config.OperationMode,
	})
}

func (server *apiServer) overview(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "overview"}) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"version": Version,
		"mode":    server.config.OperationMode,
		"recommendations": map[string]any{
			"total":  server.repository.Count(),
			"source": "in_process_history",
		},
		"inventory":   Availability{Available: server.provider != nil, Source: "kubernetes", Reason: availabilityReason(server.provider != nil, "inventory provider is not configured")},
		"generatedAt": time.Now().UTC(),
	})
}

func availabilityReason(available bool, reason string) string {
	if available {
		return ""
	}
	return reason
}

func (server *apiServer) listRecommendations(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	query := r.URL.Query()
	options := repository.ListOptions{
		Page: page, PageSize: pageSize, Namespace: query.Get("namespace"), Workload: query.Get("workload"),
		Confidence: query.Get("confidence"), Risk: query.Get("risk"), Filter: query.Get("filter"), Sort: query.Get("sort"), Order: query.Get("order"),
	}
	if options.Sort == "" {
		options.Sort = "createdAt"
	}
	if options.Order == "" {
		options.Order = "desc"
	}
	if !contains([]string{"createdAt", "namespace", "workload", "confidence", "risk"}, options.Sort) || !contains([]string{"asc", "desc"}, options.Order) {
		respondError(w, r, http.StatusBadRequest, "invalid_sort", "sort or order is not supported")
		return
	}
	if options.Confidence != "" && !contains([]string{"low", "medium", "high"}, options.Confidence) || options.Risk != "" && !contains([]string{"low", "medium", "high"}, options.Risk) {
		respondError(w, r, http.StatusBadRequest, "invalid_filter", "confidence and risk filters must be low, medium, or high")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "recommendations", Namespace: options.Namespace}) {
		return
	}
	result := server.repository.List(options)
	respondJSON(w, http.StatusOK, map[string]any{
		"items":      result.Items,
		"pagination": map[string]int{"page": result.Page, "pageSize": result.PageSize, "total": result.Total},
		"status":     Availability{Available: true, Source: "in_process_history"},
	})
}

func (server *apiServer) getRecommendation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) < 5 || len(id) > 64 || !strings.HasPrefix(id, "rec_") {
		respondError(w, r, http.StatusBadRequest, "invalid_id", "recommendation identifier is invalid")
		return
	}
	record, ok := server.repository.Get(id)
	if !ok {
		respondError(w, r, http.StatusNotFound, "not_found", "recommendation was not found")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "get", Resource: "recommendations", Namespace: record.Namespace, Name: id}) {
		return
	}
	respondJSON(w, http.StatusOK, record)
}

func (server *apiServer) calculateRecommendation(w http.ResponseWriter, r *http.Request) {
	if server.config.OperationMode == config.ModeObserve {
		respondError(w, r, http.StatusForbidden, "operation_disabled", "recommendation generation is disabled in observe mode")
		return
	}
	var input optimizer.Input
	if err := decodeJSON(w, r, &input); err != nil {
		writeDecodeError(w, r, err)
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "create", Resource: "recommendations", Namespace: input.Namespace}) {
		return
	}
	recommendation, err := optimizer.Recommend(input)
	if err != nil {
		respondError(w, r, http.StatusUnprocessableEntity, "invalid_recommendation_input", err.Error())
		return
	}
	payload, err := json.Marshal(input)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal_error", "could not fingerprint the recommendation request")
		return
	}
	fingerprintDigest := sha256.Sum256(payload)
	principal, ok := principalFromContext(r.Context())
	if !ok {
		respondError(w, r, http.StatusUnauthorized, "unauthorized", "an authenticated identity is required")
		return
	}
	record, created, err := server.repository.Create(recommendation, repository.CreateOptions{
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
		Scope:          principal.Username + "\x00" + principal.UID + "\x00" + input.Namespace,
		Fingerprint:    hex.EncodeToString(fingerprintDigest[:]),
	})
	if err != nil {
		if errors.Is(err, repository.ErrIdempotencyConflict) {
			respondError(w, r, http.StatusConflict, "idempotency_conflict", err.Error())
			return
		}
		respondError(w, r, http.StatusBadRequest, "invalid_idempotency_key", err.Error())
		return
	}
	action := "replay"
	if created {
		server.metrics.observeRecommendation(record.Confidence, record.Risk)
		action = "create"
	}
	_ = server.repository.AddAudit(repository.AuditEvent{
		Actor: principal.Username, Action: action, Resource: "recommendations/" + record.ID,
		Namespace: record.Namespace, Outcome: "success", RequestID: requestIDFromContext(r.Context()),
	})
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	w.Header().Set("Location", "/api/v1/recommendations/"+record.ID)
	respondJSON(w, status, record)
}

func (server *apiServer) policies(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "policies"}) {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items":      []any{map[string]any{"name": "default", "builtIn": true, "policy": optimizer.DefaultPolicy()}},
		"pagination": map[string]int{"page": 1, "pageSize": 50, "total": 1},
		"status":     Availability{Available: true, Source: "built_in_policy"},
	})
}

func (server *apiServer) audit(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "audit"}) {
		return
	}
	items, total := server.repository.ListAudit(page, pageSize)
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items, "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": total},
		"status": Availability{Available: true, Source: "in_process_audit"},
	})
}

func (server *apiServer) integrations(w http.ResponseWriter, r *http.Request) {
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: "integrations"}) {
		return
	}
	items := []map[string]any{
		{"name": "kubernetesInventory", "configured": server.provider != nil},
		{"name": "prometheus", "configured": server.config.PrometheusURL != ""},
		{"name": "gitops", "configured": false},
		{"name": "forecasting", "configured": false},
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items": items, "pagination": map[string]int{"page": 1, "pageSize": len(items), "total": len(items)},
		"status": Availability{Available: true, Source: "runtime_configuration"},
	})
}

func (server *apiServer) collection(w http.ResponseWriter, r *http.Request, resource string) {
	page, pageSize, err := parsePagination(r)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid_pagination", err.Error())
		return
	}
	query := CollectionQuery{
		Page: page, PageSize: pageSize, Namespace: r.URL.Query().Get("namespace"), Cluster: r.URL.Query().Get("cluster"),
		Filter: r.URL.Query().Get("filter"), Sort: r.URL.Query().Get("sort"), Order: r.URL.Query().Get("order"),
	}
	if query.Order != "" && query.Order != "asc" && query.Order != "desc" {
		respondError(w, r, http.StatusBadRequest, "invalid_sort", "order must be asc or desc")
		return
	}
	if !server.authorize(w, r, AccessAttributes{Verb: "list", Resource: resourceName(resource), Namespace: query.Namespace}) {
		return
	}
	if server.provider == nil {
		respondJSON(w, http.StatusOK, map[string]any{
			"items": []any{}, "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": 0},
			"status": Availability{Available: false, Source: resourceName(resource), Reason: "data provider is not configured"},
		})
		return
	}
	result, err := server.provider.List(r.Context(), resource, query)
	if err != nil {
		server.logger.Error("data provider request failed", "request_id", requestIDFromContext(r.Context()), "resource", resource, "error", err)
		respondError(w, r, http.StatusServiceUnavailable, "dependency_unavailable", "the data provider is unavailable")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"items": result.Items, "pagination": map[string]int{"page": page, "pageSize": pageSize, "total": result.Total}, "status": result.Status,
	})
}

// JSONLogger returns a production-safe structured logger for the executable.
func JSONLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
