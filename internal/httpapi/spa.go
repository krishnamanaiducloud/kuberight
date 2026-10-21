package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type spaHandler struct {
	root      string
	indexPath string
}

func newSPAHandler(root string) (http.Handler, error) {
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
	return &spaHandler{root: resolvedRoot, indexPath: indexPath}, nil
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
	candidate := handler.indexPath
	if clean != "" && clean != "." {
		requested := filepath.Join(handler.root, filepath.FromSlash(clean))
		if resolved, err := filepath.EvalSymlinks(requested); err == nil && withinRoot(handler.root, resolved) {
			if info, statErr := os.Stat(resolved); statErr == nil && info.Mode().IsRegular() {
				candidate = resolved
			}
		}
	}
	file, err := os.Open(candidate)
	if err != nil {
		respondError(w, r, http.StatusNotFound, "not_found", "the requested resource was not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal_error", "static content is unavailable")
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(candidate)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if candidate == handler.indexPath {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

var _ io.ReadSeeker = (*os.File)(nil)
