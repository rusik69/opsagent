package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to the GitLab REST API to create branches, commits and merge
// requests. All mutation is confined to MR creation.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// Defaults used when a tool call omits them.
	DefaultProjectID   string
	SourceBranchPrefix string
	TargetBranch       string
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL:            strings.TrimSuffix(baseURL, "/"),
		token:              token,
		http:               &http.Client{Timeout: 60 * time.Second},
		SourceBranchPrefix: "opsagent-fix",
		TargetBranch:       "main",
	}
}

type FileChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type MRResult struct {
	IID   int    `json:"iid"`
	URL   string `json:"web_url"`
	Title string `json:"title"`
}

func (c *Client) enabled() bool { return c.baseURL != "" && c.token != "" }

func (c *Client) escProject(projectID string) string {
	if projectID == "" {
		projectID = c.DefaultProjectID
	}
	return url.PathEscape(projectID)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab %s %s: %s", method, path, summarizeGitLabError(data, resp.StatusCode))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("gitlab parse response: %w", err)
		}
	}
	return nil
}

func summarizeGitLabError(data []byte, status int) string {
	var e struct {
		Message any `json:"message"`
	}
	if err := json.Unmarshal(data, &e); err == nil && e.Message != nil {
		if s, ok := e.Message.(string); ok {
			return fmt.Sprintf("%d: %s", status, truncate(s, 300))
		}
		return fmt.Sprintf("%d: %v", status, truncate(fmt.Sprint(e.Message), 300))
	}
	return fmt.Sprintf("%d: %s", status, truncate(string(data), 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// CreateBranch creates branch from ref if it does not already exist.
func (c *Client) CreateBranch(ctx context.Context, projectID, branch, ref string) error {
	if !c.enabled() {
		return fmt.Errorf("gitlab is not configured (base_url and token required)")
	}
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	if ref == "" {
		ref = c.TargetBranch
	}
	if exists, err := c.BranchExists(ctx, projectID, branch); err == nil && exists {
		return nil
	}
	err := c.do(ctx, http.MethodPost, "/projects/"+c.escProject(projectID)+"/repository/branches",
		map[string]string{"branch": branch, "ref": ref}, nil)
	if err != nil {
		return fmt.Errorf("create branch: %w", err)
	}
	return nil
}

func (c *Client) BranchExists(ctx context.Context, projectID, branch string) (bool, error) {
	var out struct {
		Name string `json:"name"`
	}
	err := c.do(ctx, http.MethodGet, "/projects/"+c.escProject(projectID)+"/repository/branches/"+url.PathEscape(branch), nil, &out)
	if err != nil {
		return false, nil // treat as missing; create will surface real errors
	}
	return out.Name != "", nil
}

// UpsertFile writes content to path on branch with a commit message. GitLab
// uses POST to create a new file and PUT to update an existing one; we try
// POST first (the common case for a fresh fix branch) and fall back to PUT if
// the file already exists.
func (c *Client) UpsertFile(ctx context.Context, projectID, branch, path, content, commitMsg string) error {
	if !c.enabled() {
		return fmt.Errorf("gitlab is not configured (base_url and token required)")
	}
	fileURL := "/projects/" + c.escProject(projectID) + "/repository/files/" + url.PathEscape(path)
	body := map[string]string{
		"branch":         branch,
		"content":        content,
		"commit_message": commitMsg,
	}
	err := c.do(ctx, http.MethodPost, fileURL, body, nil)
	if err == nil {
		return nil
	}
	if !isGitLabErrorCode(err, http.StatusBadRequest, http.StatusNotFound) {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	if err := c.do(ctx, http.MethodPut, fileURL, body, nil); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

// isGitLabErrorCode reports whether err was a GitLab API error with one of the
// given HTTP status codes. It is used to distinguish "file already exists"
// (BadRequest) from transport or authorization failures that must not trigger
// a PUT retry.
func isGitLabErrorCode(err error, codes ...int) bool {
	if err == nil {
		return false
	}
	for _, code := range codes {
		if strings.Contains(err.Error(), fmt.Sprintf("%d:", code)) {
			return true
		}
	}
	return false
}

// CreateMR opens a merge request from source to target.
func (c *Client) CreateMR(ctx context.Context, projectID, sourceBranch, targetBranch, title, description string) (*MRResult, error) {
	if !c.enabled() {
		return nil, fmt.Errorf("gitlab is not configured (base_url and token required)")
	}
	if targetBranch == "" {
		targetBranch = c.TargetBranch
	}
	var out MRResult
	err := c.do(ctx, http.MethodPost, "/projects/"+c.escProject(projectID)+"/merge_requests",
		map[string]string{
			"source_branch":        sourceBranch,
			"target_branch":        targetBranch,
			"title":                title,
			"description":          description,
			"remove_source_branch": "true",
		}, &out)
	if err != nil {
		return nil, fmt.Errorf("create MR: %w", err)
	}
	return &out, nil
}

// OpenMRWithChanges is the high-level helper used by the MCP tool: it ensures
// the source branch exists, writes the given file changes, and opens an MR.
// It returns the MR web URL.
func (c *Client) OpenMRWithChanges(ctx context.Context, projectID, sourceBranch, targetBranch, title, description string, files []FileChange) (*MRResult, error) {
	if sourceBranch == "" {
		sourceBranch = c.SourceBranchPrefix + "-" + slug(title)
	}
	if err := c.CreateBranch(ctx, projectID, sourceBranch, targetBranch); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := c.UpsertFile(ctx, projectID, sourceBranch, f.Path, f.Content, title); err != nil {
			return nil, err
		}
	}
	return c.CreateMR(ctx, projectID, sourceBranch, targetBranch, title, description)
}

// GetMRState returns the state of a merge request: opened, closed, or merged.
func (c *Client) GetMRState(ctx context.Context, projectID string, iid int) (string, error) {
	if !c.enabled() {
		return "", fmt.Errorf("gitlab is not configured (base_url and token required)")
	}
	var out struct {
		State string `json:"state"`
	}
	err := c.do(ctx, http.MethodGet, "/projects/"+c.escProject(projectID)+"/merge_requests/"+fmt.Sprintf("%d", iid), nil, &out)
	if err != nil {
		return "", err
	}
	return out.State, nil
}

// ParseMRID extracts the merge request iid from a GitLab MR web URL.
func ParseMRID(url string) (int, bool) {
	i := strings.LastIndex(url, "/merge_requests/")
	if i < 0 {
		return 0, false
	}
	tail := url[i+len("/merge_requests/"):]
	// Ignore trailing fragments like "#note_123".
	for j, r := range tail {
		if r < '0' || r > '9' {
			tail = tail[:j]
			break
		}
	}
	if tail == "" {
		return 0, false
	}
	id, err := strconv.Atoi(tail)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func slug(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if sb.Len() > 0 && sb.String()[sb.Len()-1] != '-' {
				sb.WriteRune('-')
			}
		}
	}
	out := strings.Trim(sb.String(), "-")
	if out == "" {
		out = "fix"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
