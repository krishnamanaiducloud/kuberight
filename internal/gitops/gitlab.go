package gitops

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
)

// GitLab opens merge requests through the GitLab REST API (gitlab.com or
// self-managed). The repository may be a nested group path.
type GitLab struct{ client *client }

// NewGitLab returns a GitLab provider; apiURL is https://gitlab.com/api/v4
// or https://<host>/api/v4.
func NewGitLab(apiURL, token string) (*GitLab, error) {
	c, err := newClient(apiURL, "PRIVATE-TOKEN", token)
	if err != nil {
		return nil, err
	}
	return &GitLab{client: c}, nil
}

// Name implements Provider.
func (*GitLab) Name() string { return "gitlab" }

type gitlabMergeRequest struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	SourceBranch string `json:"source_branch"`
}

// OpenPullRequest implements Provider with a merge request. The branch and
// its commit are created in one call by the commits API.
func (provider *GitLab) OpenPullRequest(ctx context.Context, request Request) (Result, error) {
	c := provider.client
	// The project ID is the URL-encoded full path; the client escapes the
	// percent sign when it builds the request path.
	project := "/projects/" + url.PathEscape(request.Repository)
	var open []gitlabMergeRequest
	if _, err := c.do(ctx, "list merge requests", http.MethodGet, project+"/merge_requests",
		url.Values{"source_branch": {request.Branch}, "state": {"opened"}}, nil, &open, http.StatusOK); err != nil {
		return Result{}, err
	}
	if len(open) > 0 {
		return Result{URL: open[0].WebURL, Number: open[0].IID, Branch: request.Branch, Existing: true}, nil
	}
	var file fileContent
	if _, err := c.do(ctx, "read manifest", http.MethodGet, project+"/repository/files/"+url.PathEscape(request.Path),
		url.Values{"ref": {request.BaseBranch}}, nil, &file, http.StatusOK); err != nil {
		return Result{}, err
	}
	content, err := file.decode()
	if err != nil {
		return Result{}, err
	}
	updated, err := request.Update(content)
	if err != nil {
		return Result{}, err
	}
	commit := map[string]any{
		"branch": request.Branch, "start_branch": request.BaseBranch, "commit_message": request.CommitMessage,
		"actions": []map[string]string{{"action": "update", "file_path": request.Path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(updated)}},
	}
	_, err = c.do(ctx, "commit manifest", http.MethodPost, project+"/repository/commits", nil, commit, nil, http.StatusCreated)
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr.Status == http.StatusBadRequest {
		// The branch survives an earlier attempt; commit on top of it.
		delete(commit, "start_branch")
		_, err = c.do(ctx, "commit manifest", http.MethodPost, project+"/repository/commits", nil, commit, nil, http.StatusCreated)
	}
	if err != nil {
		return Result{}, err
	}
	var created gitlabMergeRequest
	if _, err := c.do(ctx, "open merge request", http.MethodPost, project+"/merge_requests", nil, map[string]any{
		"source_branch": request.Branch, "target_branch": request.BaseBranch, "title": request.Title,
		"description": request.Body, "remove_source_branch": true,
	}, &created, http.StatusCreated); err != nil {
		return Result{}, err
	}
	return Result{URL: created.WebURL, Number: created.IID, Branch: request.Branch}, nil
}
