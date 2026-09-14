package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rusik69/opsagent/internal/gitlab"
	"github.com/rusik69/opsagent/internal/model"
)

func (s *Server) toolsGitLab() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("create_gitlab_mr",
				mcp.WithDescription("Create a GitLab merge request that fixes a problem. Provide a title, a description explaining the fix, and one or more file changes (path + new file content). The tool creates a source branch, commits the changes, and opens the MR, returning its URL. This is the ONLY tool that modifies anything outside the agent's own memory."),
				mcp.WithString("title", mcp.Required(), mcp.Description("MR title")),
				mcp.WithString("description", mcp.Required(), mcp.Description("MR description: what changed and why")),
				mcp.WithArray("files", mcp.Required(), mcp.Description("Array of {path, content} file changes")),
				mcp.WithString("project_id", mcp.Description("GitLab project ID or namespaced path (defaults to configured project)")),
				mcp.WithString("target_branch", mcp.Description("Target branch (defaults to configured value, usually main)")),
			),
			handler: s.handleCreateGitLabMR,
		},
	}
}

func (s *Server) handleCreateGitLabMR(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.GitLab == nil {
		return resultErr("gitlab is not configured"), nil
	}
	args := request.GetArguments()
	title := strArgs(args, "title", "")
	description := strArgs(args, "description", "")
	if title == "" || description == "" {
		return resultErr("title and description are required"), nil
	}
	items, ok := strSliceArgs(args, "files")
	if !ok {
		return resultErr("files must be an array of {path, content} objects"), nil
	}
	files := []gitlab.FileChange{}
	for _, m := range items {
		path := strArgs(m, "path", "")
		content := strArgs(m, "content", "")
		if path == "" {
			continue
		}
		files = append(files, gitlab.FileChange{Path: path, Content: content})
	}
	if len(files) == 0 {
		return resultErr("at least one file change with a path is required"), nil
	}
	projectID := strArgs(args, "project_id", "")
	target := strArgs(args, "target_branch", "")
	source := strArgs(args, "source_branch", "")

	mr, err := s.deps.GitLab.OpenMRWithChanges(ctx, projectID, source, target, title, description, files)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}

	// Record the MR link and solution on the incident if this call happened
	// during a diagnosis.
	if id := IncidentIDFrom(ctx); id != 0 {
		_ = s.deps.Store.UpdateIncidentSolution(ctx, id, description, mr.URL)
		_, _ = s.deps.Store.AddEvent(ctx, id, model.EventMRCreated, mr.URL)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "MR created: %s\n", mr.URL)
	fmt.Fprintf(&sb, "title: %s\n", mr.Title)
	fmt.Fprintf(&sb, "files changed: %d", len(files))
	return resultText(sb.String()), nil
}
