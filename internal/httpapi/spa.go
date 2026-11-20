package httpapi

import (
	"bytes"
	"errors"
	"html"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// staticTypes names the media type of every file the dashboard ships. The
// distroless image has no MIME table, and Go has no built-in type for .ico
// or .webmanifest, so they are set here rather than sniffed.
var staticTypes = map[string]string{
	".css":         "text/css; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".ico":         "image/x-icon",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".mjs":         "text/javascript; charset=utf-8",
	".png":         "image/png",
	".svg":         "image/svg+xml",
	".txt":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
}

// staticExtensions are requests for files, never dashboard routes. When such
// a file does not exist the answer is 404, not the dashboard's HTML, so a
// browser never caches a page as an icon. Workload names in routes can
// contain dots, so only these extensions count.
var staticExtensions = map[string]bool{
	".avif": true, ".css": true, ".gif": true, ".ico": true, ".jpeg": true, ".jpg": true,
	".js": true, ".json": true, ".map": true, ".mjs": true, ".png": true, ".svg": true,
	".txt": true, ".webmanifest": true, ".webp": true, ".woff": true, ".woff2": true, ".xml": true,
}

var baseTag = regexp.MustCompile(`<base\s+href="[^"]*"\s*/?>`)

type spaHandler struct {
	root     string
	index    []byte
	modified time.Time
}

func newSPAHandler(root, basePath string) (http.Handler, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.New("could not resolve KUBERIGHT_WEB_ROOT")
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, errors.New("KUBERIGHT_WEB_ROOT does not exist")
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil || !info.IsDir() {
		return nil, errors.New("KUBERIGHT_WEB_ROOT must be a directory")
	}
	indexPath := filepath.Join(resolvedRoot, "index.html")
	if info, err = os.Stat(indexPath); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("KUBERIGHT_WEB_ROOT must contain index.html")
	}
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, errors.New("KUBERIGHT_WEB_ROOT/index.html is not readable")
	}
	return &spaHandler{root: resolvedRoot, index: withBaseHref(raw, basePath), modified: info.ModTime()}, nil
}

// withBaseHref points <base href> at the URL prefix, so the dashboard's
// relative asset, icon, and manifest URLs resolve under it on every route.
func withBaseHref(index []byte, basePath string) []byte {
	tag := []byte(`<base href="` + html.EscapeString(basePath+"/") + `" />`)
	if baseTag.Match(index) {
		return baseTag.ReplaceAllLiteral(index, tag)
	}
	return bytes.Replace(index, []byte("<head>"), append([]byte("<head>\n    "), tag...), 1)
}

func (handler *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		respondError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "the request method is not allowed")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
		respondError(w, r, http.StatusNotFound, "not_found", "the requested resource was not found")
		return
	}
	clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	for _, segment := range strings.Split(clean, "/") {
		if strings.HasPrefix(segment, ".") {
			respondError(w, r, http.StatusNotFound, "not_found", "the requested resource was not found")
			return
		}
	}
	if clean == "" || clean == "." || clean == "index.html" {
		handler.serveIndex(w, r)
		return
	}
	requested := filepath.Join(handler.root, filepath.FromSlash(clean))
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil || !withinRoot(handler.root, resolved) {
		handler.notFoundOrIndex(w, r, clean)
		return
	}
	file, err := os.Open(resolved)
	if err != nil {
		handler.notFoundOrIndex(w, r, clean)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		handler.notFoundOrIndex(w, r, clean)
		return
	}
	extension := strings.ToLower(filepath.Ext(resolved))
	if contentType, ok := staticTypes[extension]; ok {
		w.Header().Set("Content-Type", contentType)
	} else if contentType := mime.TypeByExtension(extension); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if strings.HasPrefix(clean, "assets/") {
		// Vite puts a content hash in every file name under assets/.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Icons and the manifest keep stable names; revalidate hourly so a
		// new release's icon shows after a cache refresh.
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

// notFoundOrIndex answers a path with no file: 404 for a file request, and
// the dashboard for a client-side route.
func (handler *spaHandler) notFoundOrIndex(w http.ResponseWriter, r *http.Request, clean string) {
	if strings.HasPrefix(clean, "assets/") || staticExtensions[strings.ToLower(path.Ext(clean))] {
		w.Header().Set("Cache-Control", "no-store")
		respondError(w, r, http.StatusNotFound, "not_found", "the requested file was not found")
		return
	}
	handler.serveIndex(w, r)
}

func (handler *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", handler.modified, bytes.NewReader(handler.index))
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// withBasePath serves the application under a URL prefix. A request under
// the prefix has it removed; one without it (a probe, or a proxy that
// already rewrote the path) is served as is, and the bare prefix redirects
// to the dashboard.
func withBasePath(next http.Handler, basePath string) http.Handler {
	if basePath == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == basePath {
			target := basePath + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
			return
		}
		if strings.HasPrefix(r.URL.Path, basePath+"/") {
			stripped := r.Clone(r.Context())
			stripped.URL.Path = strings.TrimPrefix(r.URL.Path, basePath)
			if r.URL.RawPath != "" {
				stripped.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, basePath)
			}
			stripped.RequestURI = stripped.URL.RequestURI()
			next.ServeHTTP(w, stripped)
			return
		}
		next.ServeHTTP(w, r)
	})
}
