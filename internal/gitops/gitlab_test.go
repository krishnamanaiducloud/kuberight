package gitops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitLab mirrors the GitLab REST endpoints the provider uses, for the
// project "platform/apps/deploy" in a nested group.
type fakeGitLab struct {
	files    map[string]string // branch -> content
	requests []map[string]any
	token    string
	paths    []string
}

func newFakeGitLab(t *testing.T) (*fakeGitLab, *httptest.Server) {
	forge := &fakeGitLab{files: map[string]string{"main": manifest}}
	project := "/api/v4/projects/platform%2Fapps%2Fdeploy"
	file := "apps%2Fshop%20api.yaml"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forge.token = r.Header.Get("PRIVATE-TOKEN")
		forge.paths = append(forge.paths, r.Method+" "+r.URL.EscapedPath())
		write := func(status int, value any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch path := r.URL.EscapedPath(); {
		case r.Method == http.MethodGet && path == project+"/merge_requests":
			open := []map[string]any{}
			for _, request := range forge.requests {
				if request["source_branch"] == r.URL.Query().Get("source_branch") {
					open = append(open, request)
				}
			}
			write(http.StatusOK, open)
		case r.Method == http.MethodGet && path == project+"/repository/files/"+file:
			content, ok := forge.files[r.URL.Query().Get("ref")]
			if !ok {
				write(http.StatusNotFound, map[string]string{"message": "404 File Not Found"})
				return
			}
			write(http.StatusOK, map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64", "blob_id": "b"})
		case r.Method == http.MethodPost && path == project+"/repository/commits":
			var body struct {
				Branch      string              `json:"branch"`
				StartBranch string              `json:"start_branch"`
				Actions     []map[string]string `json:"actions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, exists := forge.files[body.Branch]; exists && body.StartBranch != "" {
				write(http.StatusBadRequest, map[string]string{"message": "A branch called '" + body.Branch + "' already exists."})
				return
			}
			if len(body.Actions) != 1 || body.Actions[0]["file_path"] != "apps/shop api.yaml" || body.Actions[0]["action"] != "update" {
				write(http.StatusBadRequest, map[string]string{"message": "unexpected actions"})
				return
			}
			content, _ := base64.StdEncoding.DecodeString(body.Actions[0]["content"])
			forge.files[body.Branch] = string(content)
			write(http.StatusCreated, map[string]string{"id": "c0ffee"})
		case r.Method == http.MethodPost && path == project+"/merge_requests":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["iid"] = len(forge.requests) + 1
			body["web_url"] = "https://gitlab.example/platform/apps/deploy/-/merge_requests/1"
			forge.requests = append(forge.requests, body)
			write(http.StatusCreated, body)
		default:
			write(http.StatusNotFound, map[string]string{"message": "404 Not Found " + r.Method + " " + path})
		}
	}))
	t.Cleanup(server.Close)
	return forge, server
}

func gitlabRequest() Request {
	request := pullRequestFor(rightsize)
	request.Repository, request.Path = "platform/apps/deploy", "apps/shop api.yaml"
	return request
}

func TestGitLabOpensOneMergeRequestPerBranch(t *testing.T) {
	forge, server := newFakeGitLab(t)
	provider, err := NewProvider("gitlab", server.URL+"/api/v4", "glpat-secret")
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.OpenPullRequest(context.Background(), gitlabRequest())
	if err != nil || result.Number != 1 || result.Existing || !strings.Contains(result.URL, "merge_requests/1") {
		t.Fatalf("%+v err=%v paths=%v", result, err, forge.paths)
	}
	if forge.token != "glpat-secret" || !strings.Contains(forge.files["kuberight/rec_1"], "cpu: 1600m") || forge.files["main"] != manifest {
		t.Fatalf("token=%q branch content:\n%s", forge.token, forge.files["kuberight/rec_1"])
	}
	request := forge.requests[0]
	if request["target_branch"] != "main" || request["remove_source_branch"] != true || request["description"] != "evidence" {
		t.Fatalf("merge request: %v", request)
	}
	again, err := provider.OpenPullRequest(context.Background(), gitlabRequest())
	if err != nil || !again.Existing || len(forge.requests) != 1 {
		t.Fatalf("repeat: %+v err=%v", again, err)
	}
}

func TestGitLabReusesABranchLeftByAnEarlierAttempt(t *testing.T) {
	forge, server := newFakeGitLab(t)
	forge.files["kuberight/rec_1"] = manifest
	provider, _ := NewGitLab(server.URL+"/api/v4", "glpat-secret")
	result, err := provider.OpenPullRequest(context.Background(), gitlabRequest())
	if err != nil || result.Number != 1 || !strings.Contains(forge.files["kuberight/rec_1"], "memory: 410Mi") {
		t.Fatalf("%+v err=%v paths=%v", result, err, forge.paths)
	}
}
