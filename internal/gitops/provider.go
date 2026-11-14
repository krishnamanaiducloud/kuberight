package gitops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Request describes one pull request: Update rewrites the file on the base
// branch; the result is committed to Branch and proposed against BaseBranch.
type Request struct {
	Repository    string // owner/name
	Path          string
	BaseBranch    string
	Branch        string
	Title, Body   string
	CommitMessage string
	Update        func([]byte) ([]byte, error)
}

// Result is the opened, or already open, pull request.
type Result struct {
	URL      string `json:"url"`
	Number   int    `json:"number"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

// Provider opens pull requests on a Git hosting service.
type Provider interface {
	Name() string
	OpenPullRequest(context.Context, Request) (Result, error)
}

// ProviderError is an unexpected response from the hosting service.
type ProviderError struct {
	Operation string
	Status    int
	Message   string
}

func (err *ProviderError) Error() string {
	return fmt.Sprintf("%s: the Git provider returned %d: %s", err.Operation, err.Status, err.Message)
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

// ValidateTarget rejects repository names, paths, and branches that could
// escape the intended file or API resource.
func ValidateTarget(repository, path, branch string) error {
	if !repositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return fmt.Errorf("repository %q must be owner/name", repository)
	}
	if path == "" || strings.HasPrefix(path, "/") || len(path) > 1024 || strings.Contains(path, "\\") {
		return fmt.Errorf("path %q must be relative to the repository root", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("path %q must not contain empty, . or .. segments", path)
		}
	}
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("branch %q is not a valid branch name", branch)
	}
	return nil
}

const (
	maxResponseBytes = 8 << 20
	maxManifestBytes = 1 << 20
)

// client is the HTTP plumbing shared by the providers.
type client struct {
	base          *url.URL
	authorization string
	http          *http.Client
}

func newClient(apiURL, authorization string) (*client, error) {
	base, err := url.Parse(strings.TrimRight(apiURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("the Git provider URL %q is invalid", apiURL)
	}
	return &client{base: base, authorization: authorization, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (c *client) do(ctx context.Context, operation, method, path string, query url.Values, body, out any, accepted ...int) (int, error) {
	target := *c.base
	target.Path = c.base.Path + path
	target.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", c.authorization)
	request.Header.Set("User-Agent", "kuberight")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return response.StatusCode, fmt.Errorf("%s: %w", operation, err)
	}
	for _, status := range accepted {
		if response.StatusCode == status {
			if out != nil && len(payload) > 0 {
				if err := json.Unmarshal(payload, out); err != nil {
					return response.StatusCode, fmt.Errorf("%s: decode response: %w", operation, err)
				}
			}
			return response.StatusCode, nil
		}
	}
	var problem struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &problem)
	if problem.Message == "" {
		problem.Message = strings.TrimSpace(string(payload[:min(len(payload), 200)]))
	}
	return response.StatusCode, &ProviderError{Operation: operation, Status: response.StatusCode, Message: problem.Message}
}

func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

type fileContent struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	SHA      string `json:"sha"`
}

func (file fileContent) decode() ([]byte, error) {
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected file encoding %q", file.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode file: %w", err)
	}
	if len(decoded) > maxManifestBytes {
		return nil, errors.New("the manifest is larger than 1 MiB")
	}
	return decoded, nil
}

type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// GitHub opens pull requests through the GitHub REST API (github.com or
// GitHub Enterprise Server, whose API URL ends in /api/v3).
type GitHub struct{ client *client }

// NewGitHub returns a GitHub provider. apiURL is https://api.github.com for
// github.com.
func NewGitHub(apiURL, token string) (*GitHub, error) {
	c, err := newClient(apiURL, "Bearer "+token)
	if err != nil {
		return nil, err
	}
	return &GitHub{client: c}, nil
}

// Name implements Provider.
func (*GitHub) Name() string { return "github" }

// OpenPullRequest implements Provider. It is idempotent per branch: an open
// pull request for the branch is returned instead of a new one.
func (provider *GitHub) OpenPullRequest(ctx context.Context, request Request) (Result, error) {
	c, repository := provider.client, "/repos/"+request.Repository
	owner := strings.SplitN(request.Repository, "/", 2)[0]
	var open []pullRequest
	if _, err := c.do(ctx, "list pull requests", http.MethodGet, repository+"/pulls", url.Values{"head": {owner + ":" + request.Branch}, "state": {"open"}}, nil, &open, http.StatusOK); err != nil {
		return Result{}, err
	}
	if len(open) > 0 {
		return Result{URL: open[0].HTMLURL, Number: open[0].Number, Branch: request.Branch, Existing: true}, nil
	}
	var base struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := c.do(ctx, "read base branch", http.MethodGet, repository+"/git/ref/heads/"+escapePath(request.BaseBranch), nil, nil, &base, http.StatusOK); err != nil {
		return Result{}, err
	}
	var file fileContent
	if _, err := c.do(ctx, "read manifest", http.MethodGet, repository+"/contents/"+escapePath(request.Path), url.Values{"ref": {request.BaseBranch}}, nil, &file, http.StatusOK); err != nil {
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
	status, err := c.do(ctx, "create branch", http.MethodPost, repository+"/git/refs", nil,
		map[string]string{"ref": "refs/heads/" + request.Branch, "sha": base.Object.SHA}, nil, http.StatusCreated, http.StatusUnprocessableEntity)
	if err != nil {
		return Result{}, err
	}
	fileSHA := file.SHA
	if status == http.StatusUnprocessableEntity {
		// The branch survives an earlier attempt; commit on top of it.
		var existing fileContent
		if _, err := c.do(ctx, "read manifest on branch", http.MethodGet, repository+"/contents/"+escapePath(request.Path), url.Values{"ref": {request.Branch}}, nil, &existing, http.StatusOK); err != nil {
			return Result{}, err
		}
		fileSHA = existing.SHA
	}
	if _, err := c.do(ctx, "commit manifest", http.MethodPut, repository+"/contents/"+escapePath(request.Path), nil, map[string]string{
		"message": request.CommitMessage, "content": base64.StdEncoding.EncodeToString(updated), "sha": fileSHA, "branch": request.Branch,
	}, nil, http.StatusOK, http.StatusCreated); err != nil {
		return Result{}, err
	}
	var created pullRequest
	if _, err := c.do(ctx, "open pull request", http.MethodPost, repository+"/pulls", nil, map[string]any{
		"title": request.Title, "head": request.Branch, "base": request.BaseBranch, "body": request.Body, "maintainer_can_modify": true,
	}, &created, http.StatusCreated); err != nil {
		return Result{}, err
	}
	return Result{URL: created.HTMLURL, Number: created.Number, Branch: request.Branch}, nil
}

// Gitea opens pull requests through the Gitea or Forgejo API.
type Gitea struct{ client *client }

// NewGitea returns a Gitea or Forgejo provider; apiURL ends in /api/v1.
func NewGitea(apiURL, token string) (*Gitea, error) {
	c, err := newClient(apiURL, "token "+token)
	if err != nil {
		return nil, err
	}
	return &Gitea{client: c}, nil
}

// Name implements Provider.
func (*Gitea) Name() string { return "gitea" }

// OpenPullRequest implements Provider.
func (provider *Gitea) OpenPullRequest(ctx context.Context, request Request) (Result, error) {
	c, repository := provider.client, "/repos/"+request.Repository
	for page := 1; page <= 10; page++ {
		var open []pullRequest
		if _, err := c.do(ctx, "list pull requests", http.MethodGet, repository+"/pulls", url.Values{"state": {"open"}, "page": {fmt.Sprint(page)}, "limit": {"50"}}, nil, &open, http.StatusOK); err != nil {
			return Result{}, err
		}
		for _, pull := range open {
			if pull.Head.Ref == request.Branch {
				return Result{URL: pull.HTMLURL, Number: pull.Number, Branch: request.Branch, Existing: true}, nil
			}
		}
		if len(open) < 50 {
			break
		}
	}
	var file fileContent
	if _, err := c.do(ctx, "read manifest", http.MethodGet, repository+"/contents/"+escapePath(request.Path), url.Values{"ref": {request.BaseBranch}}, nil, &file, http.StatusOK); err != nil {
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
	commit := map[string]string{"message": request.CommitMessage, "content": base64.StdEncoding.EncodeToString(updated), "sha": file.SHA, "branch": request.BaseBranch, "new_branch": request.Branch}
	_, err = c.do(ctx, "commit manifest", http.MethodPut, repository+"/contents/"+escapePath(request.Path), nil, commit, nil, http.StatusOK, http.StatusCreated)
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr.Status == http.StatusUnprocessableEntity {
		// The branch survives an earlier attempt; commit on top of it.
		var existing fileContent
		if _, err := c.do(ctx, "read manifest on branch", http.MethodGet, repository+"/contents/"+escapePath(request.Path), url.Values{"ref": {request.Branch}}, nil, &existing, http.StatusOK); err != nil {
			return Result{}, err
		}
		commit = map[string]string{"message": request.CommitMessage, "content": base64.StdEncoding.EncodeToString(updated), "sha": existing.SHA, "branch": request.Branch}
		_, err = c.do(ctx, "commit manifest", http.MethodPut, repository+"/contents/"+escapePath(request.Path), nil, commit, nil, http.StatusOK, http.StatusCreated)
	}
	if err != nil {
		return Result{}, err
	}
	var created pullRequest
	if _, err := c.do(ctx, "open pull request", http.MethodPost, repository+"/pulls", nil, map[string]any{
		"title": request.Title, "head": request.Branch, "base": request.BaseBranch, "body": request.Body,
	}, &created, http.StatusCreated); err != nil {
		return Result{}, err
	}
	return Result{URL: created.HTMLURL, Number: created.Number, Branch: request.Branch}, nil
}

// NewProvider builds the configured provider.
func NewProvider(kind, apiURL, token string) (Provider, error) {
	switch kind {
	case "github":
		return NewGitHub(apiURL, token)
	case "gitea":
		return NewGitea(apiURL, token)
	}
	return nil, fmt.Errorf("unsupported Git provider %q", kind)
}
