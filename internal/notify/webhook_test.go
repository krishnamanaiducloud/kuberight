package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestWebhook(t *testing.T, handler http.HandlerFunc, format string) *Webhook {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	webhook, err := NewWebhook(Options{URL: server.URL, Secret: "0123456789abcdef", Format: format, AllowHTTP: true, Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return webhook
}

func closeWebhook(t *testing.T, webhook *Webhook) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := webhook.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

var sample = Event{Type: RecommendationApproved, Actor: "bob", RecommendationID: "rec_1", Namespace: "shop", Workload: "checkout", Container: "api",
	CurrentCPURequestMilli: 1000, CPURequestMilli: 800, CurrentMemoryRequestBytes: 1 << 30, MemoryRequestBytes: 768 << 20, Risk: "low"}

func TestWebhookSignsAndDeliversJSON(t *testing.T) {
	var mu sync.Mutex
	var got Event
	var verified bool
	webhook := newTestWebhook(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		_ = json.Unmarshal(body, &got)
		expected := "sha256=" + Signature([]byte("0123456789abcdef"), r.Header.Get("X-KubeRight-Timestamp"), body)
		verified = r.Header.Get("X-KubeRight-Signature") == expected && r.Header.Get("X-KubeRight-Event") == RecommendationApproved && r.Header.Get("X-KubeRight-Delivery") != ""
	}, "json")
	webhook.Notify(sample)
	closeWebhook(t, webhook)
	mu.Lock()
	defer mu.Unlock()
	if !verified || got.RecommendationID != "rec_1" || got.CPURequestMilli != 800 {
		t.Fatalf("verified=%v event=%#v", verified, got)
	}
	if delivered, failed, _ := webhook.Stats(); delivered != 1 || failed != 0 {
		t.Fatalf("delivered=%d failed=%d", delivered, failed)
	}
}

func TestWebhookSlackFormat(t *testing.T) {
	var text atomic.Value
	webhook := newTestWebhook(t, func(_ http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		text.Store(payload["text"])
	}, "slack")
	webhook.Notify(sample)
	closeWebhook(t, webhook)
	if message, _ := text.Load().(string); !strings.Contains(message, "bob approved") || !strings.Contains(message, "1000m → 800m") || !strings.Contains(message, "1024 MiB → 768 MiB") {
		t.Fatalf("slack text = %q", message)
	}
}

func TestWebhookRetriesTransientFailuresOnly(t *testing.T) {
	var calls atomic.Int32
	transient := newTestWebhook(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}, "json")
	transient.Notify(sample)
	closeWebhook(t, transient)
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want 3", calls.Load())
	}
	var rejected atomic.Int32
	permanent := newTestWebhook(t, func(w http.ResponseWriter, _ *http.Request) {
		rejected.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}, "json")
	permanent.Notify(sample)
	closeWebhook(t, permanent)
	if rejected.Load() != 1 {
		t.Fatalf("a 400 response was retried %d times", rejected.Load())
	}
	if _, failed, _ := permanent.Stats(); failed != 1 {
		t.Fatalf("failed=%d", failed)
	}
}

func TestWebhookDropsWhenQueueIsFull(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(server.Close)
	webhook, err := NewWebhook(Options{URL: server.URL, AllowHTTP: true, QueueSize: 1, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	// One event can be in flight and one queued; the rest must be dropped
	// immediately rather than block the caller.
	for range 5 {
		webhook.Notify(sample)
	}
	close(release)
	closeWebhook(t, webhook)
	if _, _, dropped := webhook.Stats(); dropped < 3 {
		t.Fatalf("dropped=%d, want at least 3", dropped)
	}
}

func TestNewWebhookRejectsUnsafeTargets(t *testing.T) {
	for _, target := range []string{"", "hooks.slack.com/x", "http://receiver/x", "https://user:pw@receiver/x", "https://receiver/x#frag"} {
		if _, err := NewWebhook(Options{URL: target}); err == nil {
			t.Fatalf("accepted %q", target)
		}
	}
	if _, err := NewWebhook(Options{URL: "https://receiver/x", Format: "xml"}); err == nil {
		t.Fatal("accepted unknown format")
	}
}
