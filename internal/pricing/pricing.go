// Package pricing supplies the CPU and memory unit prices used to turn
// request changes into monthly cost estimates. Prices come from operator
// configuration or, when configured, from OpenCost allocation data, which
// reflects the cluster's real cloud or custom pricing. Either way the result
// is a request-based estimate, never billing or realized savings.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Price sources.
const (
	SourceConfigured = "configured"
	SourceOpenCost   = "opencost"
	// SourceOpenCostCluster is OpenCost's cluster-wide blended price, used
	// for a namespace OpenCost has no allocation for.
	SourceOpenCostCluster = "opencost-cluster"
)

const bytesPerGiB = 1 << 30

// Prices are hourly unit prices for one namespace.
type Prices struct {
	CPUCoreHour   float64 `json:"cpuCoreHour"`
	MemoryGiBHour float64 `json:"memoryGiBHour"`
	Source        string  `json:"source"`
}

// Available reports whether both prices are known.
func (prices Prices) Available() bool { return prices.CPUCoreHour > 0 || prices.MemoryGiBHour > 0 }

// Pricer returns the prices for a namespace.
type Pricer interface {
	Prices(ctx context.Context, namespace string) Prices
}

// Static serves operator-configured prices for every namespace.
type Static struct{ CPUCoreHour, MemoryGiBHour float64 }

// Prices implements Pricer.
func (static Static) Prices(context.Context, string) Prices {
	return Prices{CPUCoreHour: static.CPUCoreHour, MemoryGiBHour: static.MemoryGiBHour, Source: SourceConfigured}
}

// OpenCostOptions configures the OpenCost client.
type OpenCostOptions struct {
	Endpoint   string
	AllowHTTP  bool
	Window     time.Duration
	CacheTTL   time.Duration
	Timeout    time.Duration
	HTTPClient *http.Client
	// Fallback serves a namespace when OpenCost is unreachable or has no
	// usable allocation for it.
	Fallback Pricer
	Now      func() time.Time
	Logger   *slog.Logger
}

const (
	// retryAfterFailure is how soon a failed OpenCost query is retried; the
	// cache TTL only applies to successful responses.
	retryAfterFailure = time.Minute
	// maximumStaleness bounds how long the last good OpenCost prices are used
	// while OpenCost is unavailable.
	maximumStaleness = 6 * time.Hour
)

// OpenCost derives effective per-namespace prices from OpenCost's
// allocation API: cpuCost / cpuCoreHours and ramCost / ramGiBHours.
type OpenCost struct {
	endpoint url.URL
	options  OpenCostOptions
	client   *http.Client

	mu          sync.Mutex
	attemptedAt time.Time
	succeededAt time.Time
	prices      map[string]Prices
	cluster     Prices
	lastError   error
}

const maximumResponseBytes = 8 << 20

// ErrInvalidEndpoint rejects an unsafe OpenCost URL.
var ErrInvalidEndpoint = errors.New("invalid OpenCost endpoint")

// NewOpenCost validates the endpoint and applies defaults.
func NewOpenCost(options OpenCostOptions) (*OpenCost, error) {
	endpoint, err := url.Parse(options.Endpoint)
	if err != nil || !endpoint.IsAbs() || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: use an absolute URL without credentials, query, or fragment", ErrInvalidEndpoint)
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	if endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !options.AllowHTTP) {
		return nil, fmt.Errorf("%w: HTTPS is required unless cleartext HTTP is explicitly allowed", ErrInvalidEndpoint)
	}
	if options.Window <= 0 {
		options.Window = 7 * 24 * time.Hour
	}
	if options.CacheTTL <= 0 {
		options.CacheTTL = 15 * time.Minute
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	if options.Fallback == nil {
		options.Fallback = Static{}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: options.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &OpenCost{endpoint: *endpoint, options: options, client: client}, nil
}

// allocation is the subset of an OpenCost allocation used for pricing.
type allocation struct {
	CPUCoreHours float64 `json:"cpuCoreHours"`
	CPUCost      float64 `json:"cpuCost"`
	RAMByteHours float64 `json:"ramByteHours"`
	RAMCost      float64 `json:"ramCost"`
}

func (item allocation) prices(source string) (Prices, bool) {
	prices := Prices{Source: source}
	if item.CPUCoreHours > 0 && item.CPUCost >= 0 {
		prices.CPUCoreHour = item.CPUCost / item.CPUCoreHours
	}
	if item.RAMByteHours > 0 && item.RAMCost >= 0 {
		prices.MemoryGiBHour = item.RAMCost / (item.RAMByteHours / bytesPerGiB)
	}
	for _, value := range []float64{prices.CPUCoreHour, prices.MemoryGiBHour} {
		// A price over 1000 per core- or GiB-hour is a unit error, not a price.
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1000 {
			return Prices{}, false
		}
	}
	return prices, prices.CPUCoreHour > 0 && prices.MemoryGiBHour > 0
}

func (opencost *OpenCost) fetch(ctx context.Context) (map[string]Prices, Prices, error) {
	request := opencost.endpoint
	request.Path = strings.TrimSuffix(request.Path, "/") + "/allocation"
	query := url.Values{}
	query.Set("window", fmt.Sprintf("%dh", int(opencost.options.Window.Hours())))
	query.Set("aggregate", "namespace")
	query.Set("accumulate", "true")
	request.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(ctx, opencost.options.Timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.String(), nil)
	if err != nil {
		return nil, Prices{}, err
	}
	httpRequest.Header.Set("Accept", "application/json")
	response, err := opencost.client.Do(httpRequest)
	if err != nil {
		return nil, Prices{}, fmt.Errorf("OpenCost allocation request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, Prices{}, fmt.Errorf("OpenCost allocation returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return nil, Prices{}, err
	}
	if len(body) > maximumResponseBytes {
		return nil, Prices{}, errors.New("OpenCost allocation response exceeds 8 MiB")
	}
	var envelope struct {
		Code int                     `json:"code"`
		Data []map[string]allocation `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, Prices{}, fmt.Errorf("decode OpenCost allocation: %w", err)
	}
	if envelope.Code != 0 && envelope.Code != http.StatusOK {
		return nil, Prices{}, fmt.Errorf("OpenCost allocation reported code %d", envelope.Code)
	}
	prices := map[string]Prices{}
	var total allocation
	for _, set := range envelope.Data {
		for namespace, item := range set {
			if strings.HasPrefix(namespace, "__") {
				// __idle__, __unallocated__, and similar are not namespaces.
				continue
			}
			// OpenCost reports small negative hours and costs for pods that
			// started inside the window; they carry no pricing information.
			total.CPUCoreHours += math.Max(0, item.CPUCoreHours)
			total.CPUCost += math.Max(0, item.CPUCost)
			total.RAMByteHours += math.Max(0, item.RAMByteHours)
			total.RAMCost += math.Max(0, item.RAMCost)
			if value, ok := item.prices(SourceOpenCost); ok {
				prices[namespace] = value
			}
		}
	}
	cluster, ok := total.prices(SourceOpenCostCluster)
	if !ok {
		return nil, Prices{}, errors.New("OpenCost returned no priced CPU and memory allocation")
	}
	return prices, cluster, nil
}

// Prices implements Pricer. A successful OpenCost response is reused for
// the cache TTL; a failure is retried after a minute. The last good prices
// keep serving through an outage of up to six hours, after which the
// configured prices apply. Every failed attempt is logged.
func (opencost *OpenCost) Prices(ctx context.Context, namespace string) Prices {
	opencost.mu.Lock()
	defer opencost.mu.Unlock()
	now := opencost.options.Now()
	due := opencost.attemptedAt.IsZero() ||
		(opencost.lastError == nil && now.Sub(opencost.attemptedAt) >= opencost.options.CacheTTL) ||
		(opencost.lastError != nil && now.Sub(opencost.attemptedAt) >= retryAfterFailure)
	if due {
		// The query must not inherit a caller's short or canceled deadline.
		fetchContext := context.WithoutCancel(ctx)
		prices, cluster, err := opencost.fetch(fetchContext)
		opencost.attemptedAt, opencost.lastError = now, err
		if err == nil {
			opencost.prices, opencost.cluster, opencost.succeededAt = prices, cluster, now
		} else {
			opencost.options.Logger.Warn("OpenCost pricing is unavailable; using the last good or configured prices", "error", err)
		}
	}
	if !opencost.succeededAt.IsZero() && now.Sub(opencost.succeededAt) <= maximumStaleness {
		if value, ok := opencost.prices[namespace]; ok {
			return value
		}
		if opencost.cluster.Available() {
			return opencost.cluster
		}
	}
	return opencost.options.Fallback.Prices(ctx, namespace)
}

// Status reports the last successful fetch and the last error for the
// integrations view.
func (opencost *OpenCost) Status() (time.Time, error) {
	opencost.mu.Lock()
	defer opencost.mu.Unlock()
	return opencost.succeededAt, opencost.lastError
}
