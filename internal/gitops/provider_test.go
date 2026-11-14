package gitops

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeForge keeps one repository with one file and records every call, in
// the shape of the GitHub or Gitea REST API.
type fakeForge struct {
	mu       sync.Mutex
	gitea    bool
	files    map[string]string // branch -> content
	pulls    []map[string]any
	calls    []string
	authSeen string
}

func newFakeForge(t *testing.T, gitea bool) (*fakeForge, *httptest.Server) {
	forge := &fakeForge{gitea: gitea, files: map[string]string{"main": manifest}}
	prefix := ""
	if gitea {
		prefix = "/api/v1"
	}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, value any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	file := prefix + "/repos/acme/deploy/contents/apps/shop/api.yaml"
	mux.HandleFunc("GET "+prefix+"/repos/acme/deploy/pulls", func(w http.ResponseWriter, r *http.Request) {
		matches := []map[string]any{}
		for _, pull := range forge.pulls {
			if forge.gitea || r.URL.Query().Get("head") == "acme:"+pull["head"].(map[string]any)["ref"].(string) {
				matches = append(matches, pull)
			}
		}
		write(w, http.StatusOK, matches)
	})
	mux.HandleFunc("GET "+prefix+"/repos/acme/deploy/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"object": map[string]string{"sha": "base-sha"}})
	})
	mux.HandleFunc("GET "+file, func(w http.ResponseWriter, r *http.Request) {
		content, ok := forge.files[r.URL.Query().Get("ref")]
		if !ok {
			write(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		write(w, http.StatusOK, map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64", "sha": blobSHA(content)})
	})
	mux.HandleFunc("POST "+prefix+"/repos/acme/deploy/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		branch := strings.TrimPrefix(body["ref"], "refs/heads/")
		if _, exists := forge.files[branch]; exists {
			write(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
			return
		}
		forge.files[branch] = forge.files["main"]
		write(w, http.StatusCreated, map[string]string{"ref": body["ref"]})
	})
	mux.HandleFunc("PUT "+file, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		content, _ := base64.StdEncoding.DecodeString(body["content"])
		branch := body["branch"]
		if newBranch := body["new_branch"]; newBranch != "" {
			if _, exists := forge.files[newBranch]; exists {
				write(w, http.StatusUnprocessableEntity, map[string]string{"message": "branch already exists"})
				return
			}
			branch = newBranch
		}
		if body["sha"] != blobSHA(forge.files[body["branch"]]) {
			write(w, http.StatusConflict, map[string]string{"message": "sha mismatch"})
			return
		}
		forge.files[branch] = string(content)
		write(w, http.StatusOK, map[string]any{"commit": map[string]string{"message": body["message"]}})
	})
	mux.HandleFunc("POST "+prefix+"/repos/acme/deploy/pulls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		pull := map[string]any{"number": len(forge.pulls) + 1, "html_url": "https://forge.example/acme/deploy/pull/" + string(rune('1'+len(forge.pulls))),
			"head": map[string]any{"ref": body["head"]}, "title": body["title"], "body": body["body"], "base": body["base"]}
		forge.pulls = append(forge.pulls, pull)
		write(w, http.StatusCreated, pull)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forge.mu.Lock()
		defer forge.mu.Unlock()
		forge.calls = append(forge.calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, prefix))
		forge.authSeen = r.Header.Get("Authorization")
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return forge, server
}

func pullRequestFor(update func([]byte) ([]byte, error)) Request {
	return Request{
		Repository: "acme/deploy", Path: "apps/shop/api.yaml", BaseBranch: "main", Branch: "kuberight/rec_1",
		Title: "Rightsize Deployment/api", Body: "evidence", CommitMessage: "rightsize api", Update: update,
	}
}

func rightsize(content []byte) ([]byte, error) {
	return UpdateManifest(content, api, Resources{CPURequestMilli: 800, CPULimitMilli: 1600, MemoryRequestBytes: 410 << 20, MemoryLimitBytes: 820 << 20})
}

func TestProvidersOpenOnePullRequestPerBranch(t *testing.T) {
	for _, gitea := range []bool{false, true} {
		forge, server := newFakeForge(t, gitea)
		var provider Provider
		var err error
		if gitea {
			provider, err = NewProvider("gitea", server.URL+"/api/v1", "secret")
		} else {
			provider, err = NewProvider("github", server.URL, "secret")
		}
		if err != nil {
			t.Fatal(err)
		}
		result, err := provider.OpenPullRequest(context.Background(), pullRequestFor(rightsize))
		if err != nil || result.Number != 1 || result.Existing || result.URL == "" {
			t.Fatalf("%s: %+v err=%v", provider.Name(), result, err)
		}
		if !strings.Contains(forge.files["kuberight/rec_1"], "memory: 410Mi") || forge.files["main"] != manifest {
			t.Fatalf("%s: the change must land on the branch only:\n%s", provider.Name(), forge.files["kuberight/rec_1"])
		}
		wantAuth := map[bool]string{false: "Bearer secret", true: "token secret"}[gitea]
		if forge.authSeen != wantAuth {
			t.Fatalf("%s: authorization header %q", provider.Name(), forge.authSeen)
		}
		// A second request returns the open pull request instead of a new one.
		again, err := provider.OpenPullRequest(context.Background(), pullRequestFor(rightsize))
		if err != nil || !again.Existing || again.Number != 1 || len(forge.pulls) != 1 {
			t.Fatalf("%s: repeat %+v err=%v pulls=%d", provider.Name(), again, err, len(forge.pulls))
		}
	}
}

func TestProvidersReuseABranchLeftByAnEarlierAttempt(t *testing.T) {
	for _, gitea := range []bool{false, true} {
		forge, server := newFakeForge(t, gitea)
		forge.files["kuberight/rec_1"] = manifest
		apiURL := map[bool]string{false: server.URL, true: server.URL + "/api/v1"}[gitea]
		provider, _ := NewProvider(map[bool]string{false: "github", true: "gitea"}[gitea], apiURL, "secret")
		result, err := provider.OpenPullRequest(context.Background(), pullRequestFor(rightsize))
		if err != nil || result.Number != 1 || !strings.Contains(forge.files["kuberight/rec_1"], "cpu: 1600m") {
			t.Fatalf("%s: %+v err=%v calls=%v", provider.Name(), result, err, forge.calls)
		}
	}
}

func TestProvidersStopBeforeWritingWhenTheEditFails(t *testing.T) {
	forge, server := newFakeForge(t, false)
	provider, _ := NewGitHub(server.URL, "secret")
	_, err := provider.OpenPullRequest(context.Background(), pullRequestFor(func([]byte) ([]byte, error) { return nil, ErrTargetNotFound }))
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("err=%v", err)
	}
	for _, call := range forge.calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("a write was attempted after the edit failed: %v", forge.calls)
		}
	}
	// Provider errors keep the status and message.
	missing := pullRequestFor(rightsize)
	missing.BaseBranch = "release"
	var providerErr *ProviderError
	if _, err := provider.OpenPullRequest(context.Background(), missing); !errors.As(err, &providerErr) || providerErr.Status != http.StatusNotFound {
		t.Fatalf("missing base branch: %v", err)
	}
}

func TestValidateTargetRejectsEscapes(t *testing.T) {
	if err := ValidateTarget("acme/deploy", "apps/shop/api.yaml", "main"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]string{
		{"acme", "a.yaml", "main"}, {"acme/deploy/extra", "a.yaml", "main"}, {"acme/..", "a.yaml", "main"},
		{"acme/deploy", "/etc/passwd", "main"}, {"acme/deploy", "apps/../secrets.yaml", "main"}, {"acme/deploy", "apps//a.yaml", "main"},
		{"acme/deploy", `apps\a.yaml`, "main"}, {"acme/deploy", "a.yaml", "feature/../main"}, {"acme/deploy", "a.yaml", "bad branch"},
	} {
		if err := ValidateTarget(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := NewProvider("gitlab", "https://gitlab.example", "x"); err == nil {
		t.Error("an unsupported provider was accepted")
	}
	if _, err := NewGitHub("ftp://example", "x"); err == nil {
		t.Error("a non-HTTP URL was accepted")
	}
}

// blobSHA stands in for Git's blob hash: equal content, equal SHA.
func blobSHA(content string) string {
	sum := sha1.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}
