// Package notify delivers recommendation lifecycle events to external tools
// such as Slack, Microsoft Teams workflows, PagerDuty, Argo Events, or CI
// pipelines. Delivery is asynchronous and never blocks an API request.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Event types.
const (
	RecommendationCreated  = "recommendation.created"
	RecommendationApproved = "recommendation.approved"
	RecommendationRejected = "recommendation.rejected"
)

// Event describes one lifecycle change of a recommendation.
type Event struct {
	Type                      string    `json:"type"`
	Timestamp                 time.Time `json:"timestamp"`
	Actor                     string    `json:"actor"`
	RecommendationID          string    `json:"recommendationId"`
	Cluster                   string    `json:"cluster,omitempty"`
	Namespace                 string    `json:"namespace"`
	Kind                      string    `json:"kind,omitempty"`
	Workload                  string    `json:"workload"`
	Container                 string    `json:"container"`
	CurrentCPURequestMilli    int64     `json:"currentCpuRequestMilli"`
	CPURequestMilli           int64     `json:"cpuRequestMilli"`
	CurrentMemoryRequestBytes int64     `json:"currentMemoryRequestBytes"`
	MemoryRequestBytes        int64     `json:"memoryRequestBytes"`
	Risk                      string    `json:"risk"`
	Confidence                string    `json:"confidence"`
	Comment                   string    `json:"comment,omitempty"`
}

// Notifier accepts events without blocking.
type Notifier interface {
	Notify(Event)
}

// Options configures a Webhook.
type Options struct {
	URL       string
	Secret    string
	Format    string
	AllowHTTP bool
	Timeout   time.Duration
	QueueSize int
	Attempts  int
	Backoff   time.Duration
	Logger    *slog.Logger
	Client    *http.Client
}

// Webhook posts events to one URL from a background worker.
type Webhook struct {
	url       string
	secret    []byte
	format    string
	client    *http.Client
	queue     chan Event
	attempts  int
	backoff   time.Duration
	logger    *slog.Logger
	done      chan struct{}
	closeOnce sync.Once
	delivered atomic.Uint64
	failed    atomic.Uint64
	dropped   atomic.Uint64
}

// NewWebhook validates the endpoint and starts the delivery worker.
func NewWebhook(options Options) (*Webhook, error) {
	target, err := url.Parse(options.URL)
	if err != nil || !target.IsAbs() || target.Host == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("webhook URL must be an absolute URL without credentials or fragment")
	}
	if target.Scheme != "https" && (target.Scheme != "http" || !options.AllowHTTP) {
		return nil, errors.New("webhook URL must use HTTPS unless HTTP is explicitly allowed")
	}
	if options.Format == "" {
		options.Format = "json"
	}
	if options.Format != "json" && options.Format != "slack" {
		return nil, errors.New("webhook format must be json or slack")
	}
	client := &http.Client{}
	if options.Client != nil {
		clone := *options.Client
		client = &clone
	}
	client.Timeout = max(options.Timeout, time.Second)
	if options.Timeout == 0 {
		client.Timeout = 5 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	webhook := &Webhook{
		url: target.String(), secret: []byte(options.Secret), format: options.Format, client: client,
		queue: make(chan Event, max(options.QueueSize, 1)), attempts: max(options.Attempts, 1), backoff: options.Backoff,
		logger: logger, done: make(chan struct{}),
	}
	if options.QueueSize == 0 {
		webhook.queue = make(chan Event, 256)
	}
	if options.Attempts == 0 {
		webhook.attempts = 4
	}
	if options.Backoff == 0 {
		webhook.backoff = time.Second
	}
	go webhook.run()
	return webhook, nil
}

// Notify enqueues an event, dropping it with a log entry when the queue is
// full so a slow receiver can never stall the API.
func (webhook *Webhook) Notify(event Event) {
	select {
	case webhook.queue <- event:
	default:
		webhook.dropped.Add(1)
		webhook.logger.Warn("webhook queue is full; event dropped", "type", event.Type, "recommendation_id", event.RecommendationID)
	}
}

// Close stops accepting events and waits for queued deliveries until ctx ends.
func (webhook *Webhook) Close(ctx context.Context) error {
	webhook.closeOnce.Do(func() { close(webhook.queue) })
	select {
	case <-webhook.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats reports delivery counters.
func (webhook *Webhook) Stats() (delivered, failed, dropped uint64) {
	return webhook.delivered.Load(), webhook.failed.Load(), webhook.dropped.Load()
}

func (webhook *Webhook) run() {
	defer close(webhook.done)
	for event := range webhook.queue {
		if err := webhook.deliver(event); err != nil {
			webhook.failed.Add(1)
			webhook.logger.Error("webhook delivery failed", "type", event.Type, "recommendation_id", event.RecommendationID, "error", err)
			continue
		}
		webhook.delivered.Add(1)
	}
}

func (webhook *Webhook) body(event Event) ([]byte, error) {
	if webhook.format == "slack" {
		return json.Marshal(map[string]string{"text": SlackText(event)})
	}
	return json.Marshal(event)
}

// SlackText renders an event as one human-readable line.
func SlackText(event Event) string {
	subject := fmt.Sprintf("%s/%s/%s", event.Namespace, event.Workload, event.Container)
	switch event.Type {
	case RecommendationApproved:
		return fmt.Sprintf("KubeRight: %s approved the recommendation for %s (CPU %dm → %dm, memory %d MiB → %d MiB).", event.Actor, subject,
			event.CurrentCPURequestMilli, event.CPURequestMilli, event.CurrentMemoryRequestBytes>>20, event.MemoryRequestBytes>>20)
	case RecommendationRejected:
		return fmt.Sprintf("KubeRight: %s rejected the recommendation for %s: %s", event.Actor, subject, event.Comment)
	default:
		return fmt.Sprintf("KubeRight: new %s-risk recommendation for %s awaits review (CPU %dm → %dm, memory %d MiB → %d MiB).", event.Risk, subject,
			event.CurrentCPURequestMilli, event.CPURequestMilli, event.CurrentMemoryRequestBytes>>20, event.MemoryRequestBytes>>20)
	}
}

// Signature returns the hex HMAC-SHA256 of "timestamp.body". Receivers should
// recompute it and reject stale timestamps to prevent replay.
func Signature(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

type permanentError struct{ error }

func (webhook *Webhook) deliver(event Event) error {
	body, err := webhook.body(event)
	if err != nil {
		return err
	}
	random := make([]byte, 12)
	_, _ = rand.Read(random)
	delivery := hex.EncodeToString(random)
	var lastErr error
	for attempt := range webhook.attempts {
		if attempt > 0 {
			time.Sleep(webhook.backoff << (attempt - 1))
		}
		lastErr = webhook.post(event.Type, delivery, body)
		var permanent permanentError
		if lastErr == nil || errors.As(lastErr, &permanent) {
			return lastErr
		}
	}
	return lastErr
}

func (webhook *Webhook) post(eventType, delivery string, body []byte) error {
	request, err := http.NewRequest(http.MethodPost, webhook.url, bytes.NewReader(body))
	if err != nil {
		return permanentError{err}
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "KubeRight-Webhook")
	request.Header.Set("X-KubeRight-Event", eventType)
	request.Header.Set("X-KubeRight-Delivery", delivery)
	request.Header.Set("X-KubeRight-Timestamp", timestamp)
	if len(webhook.secret) > 0 {
		request.Header.Set("X-KubeRight-Signature", "sha256="+Signature(webhook.secret, timestamp, body))
	}
	response, err := webhook.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return fmt.Errorf("receiver returned HTTP %d", response.StatusCode)
	default:
		return permanentError{fmt.Errorf("receiver rejected the event with HTTP %d", response.StatusCode)}
	}
}
