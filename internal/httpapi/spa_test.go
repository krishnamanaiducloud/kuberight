package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/repository"
)

// A web root shaped like the dashboard build: index.html with a <base> tag,
// hashed assets, and the favicon set at the root.
func writeWebRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"index.html":                 "<!doctype html><html><head>\n    <base href=\"/\" />\n    <link rel=\"icon\" href=\"./favicon.svg\" /></head><body></body></html>",
		"assets/index-abc123.js":     "console.log('ok')",
		"favicon.ico":                "\x00\x00\x01\x00",
		"favicon.svg":                `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"favicon-16x16.png":          "\x89PNG",
		"favicon-32x32.png":          "\x89PNG",
		"apple-touch-icon.png":       "\x89PNG",
		"android-chrome-192x192.png": "\x89PNG",
		"android-chrome-512x512.png": "\x89PNG",
		"site.webmanifest":           `{"name":"KubeRight"}`,
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func webHandler(t *testing.T, basePath string) http.Handler {
	t.Helper()
	cfg := fallbackConfig()
	cfg.WebRoot = writeWebRoot(t)
	cfg.BasePath = basePath
	handler, err := NewHandler(cfg, Dependencies{Repository: repository.NewMemory(10)})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response
}

func TestFaviconSetIsServedWithIconTypes(t *testing.T) {
	handler := webHandler(t, "")
	for path, contentType := range map[string]string{
		"/favicon.ico":                "image/x-icon",
		"/favicon.svg":                "image/svg+xml",
		"/favicon-16x16.png":          "image/png",
		"/favicon-32x32.png":          "image/png",
		"/apple-touch-icon.png":       "image/png",
		"/android-chrome-192x192.png": "image/png",
		"/android-chrome-512x512.png": "image/png",
		"/site.webmanifest":           "application/manifest+json",
	} {
		response := get(t, handler, path)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != contentType {
			t.Errorf("%s: status %d type %q, want 200 %q", path, response.Code, response.Header().Get("Content-Type"), contentType)
		}
		if response.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Errorf("%s: Cache-Control %q", path, response.Header().Get("Cache-Control"))
		}
		if strings.Contains(response.Body.String(), "<html") {
			t.Errorf("%s returned the dashboard HTML", path)
		}
	}
	if response := get(t, handler, "/assets/index-abc123.js"); response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("hashed asset: type %q cache %q", response.Header().Get("Content-Type"), response.Header().Get("Cache-Control"))
	}
}

func TestMissingStaticFilesAreNotAnsweredWithTheDashboard(t *testing.T) {
	handler := webHandler(t, "")
	for _, path := range []string{"/favicon-64x64.png", "/apple-touch-icon-precomposed.png", "/assets/missing.js", "/assets/gone", "/robots.txt", "/recommendations/favicon.ico"} {
		response := get(t, handler, path)
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "<html") || strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: status %d type %q, want a 404 that is not HTML", path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	// Client-side routes still get the dashboard, including workload names
	// with dots.
	for _, path := range []string{"/", "/recommendations/cpu", "/workloads/shop/Deployment/api.v2"} {
		response := get(t, handler, path)
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") || response.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: status %d type %q cache %q", path, response.Code, response.Header().Get("Content-Type"), response.Header().Get("Cache-Control"))
		}
	}
}

func TestDashboardAndIconsWorkUnderABasePath(t *testing.T) {
	handler := webHandler(t, "/kuberight")
	index := get(t, handler, "/kuberight/recommendations/cpu")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `<base href="/kuberight/" />`) {
		t.Fatalf("index under the prefix: status %d body %s", index.Code, index.Body.String())
	}
	for _, path := range []string{"/kuberight/favicon.ico", "/kuberight/favicon.svg", "/kuberight/site.webmanifest"} {
		if response := get(t, handler, path); response.Code != http.StatusOK || strings.HasPrefix(response.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: status %d type %q", path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	if response := get(t, handler, "/kuberight/api/v1/version"); response.Code != http.StatusOK {
		t.Errorf("API under the prefix: status %d", response.Code)
	}
	// A proxy that strips the prefix, and the probes, use unprefixed paths.
	for _, path := range []string{"/favicon.ico", "/healthz", "/api/v1/version"} {
		if response := get(t, handler, path); response.Code != http.StatusOK {
			t.Errorf("%s without the prefix: status %d", path, response.Code)
		}
	}
	if response := get(t, handler, "/kuberight?x=1"); response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "/kuberight/?x=1" {
		t.Errorf("bare prefix: status %d location %q", response.Code, response.Header().Get("Location"))
	}
	if response := get(t, handler, "/"); !strings.Contains(response.Body.String(), `<base href="/kuberight/" />`) {
		t.Errorf("index without the prefix must still point at the prefix: %s", response.Body.String())
	}
}

func TestContentSecurityPolicyAllowsOnlyASameOriginBase(t *testing.T) {
	response := get(t, webHandler(t, ""), "/")
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "base-uri 'self'") || !strings.Contains(policy, "img-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", policy)
	}
}
