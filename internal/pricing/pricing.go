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
}

// OpenCost derives effective per-namespace prices from OpenCost's
// allocation API: cpuCost / cpuCoreHours and ramCost / ramGiBHours.
type OpenCost struct {
	endpoint url.URL
	options  OpenCostOptions
	client   *http.Client

	mu        sync.Mutex
	fetchedAt time.Time
	prices    map[string]Prices
	cluster   Prices
	lastError error
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
			total.CPUCoreHours += item.CPUCoreHours
			total.CPUCost += item.CPUCost
			total.RAMByteHours += item.RAMByteHours
			total.RAMCost += item.RAMCost
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

// Prices implements Pricer. OpenCost is queried at most once per cache TTL;
// failures fall back to configured prices until the next attempt.
func (opencost *OpenCost) Prices(ctx context.Context, namespace string) Prices {
	opencost.mu.Lock()
	defer opencost.mu.Unlock()
	now := opencost.options.Now()
	if opencost.fetchedAt.IsZero() || now.Sub(opencost.fetchedAt) >= opencost.options.CacheTTL {
		prices, cluster, err := opencost.fetch(ctx)
		opencost.fetchedAt, opencost.lastError = now, err
		if err == nil {
			opencost.prices, opencost.cluster = prices, cluster
		}
	}
	if value, ok := opencost.prices[namespace]; ok {
		return value
	}
	if opencost.cluster.Available() && opencost.lastError == nil {
		return opencost.cluster
	}
	return opencost.options.Fallback.Prices(ctx, namespace)
}

// Status reports the last fetch for the integrations view.
func (opencost *OpenCost) Status() (time.Time, error) {
	opencost.mu.Lock()
	defer opencost.mu.Unlock()
	return opencost.fetchedAt, opencost.lastError
}
