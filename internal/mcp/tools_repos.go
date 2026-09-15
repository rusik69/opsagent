package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) toolsRepos() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("list_repos",
				mcp.WithDescription("List the Puppet/Ansible configuration repos the agent can read.")),
			handler: s.handleListRepos,
		},
		{
			tool: mcp.NewTool("sync_repos",
				mcp.WithDescription("Clone or pull the configuration repos.")),
			handler: s.handleSyncRepos,
		},
		{
			tool: mcp.NewTool("get_host_config",
				mcp.WithDescription("Look up configuration for a host from the Puppet/Ansible repos (inventory, host_vars, hieradata, manifests)."),
				mcp.WithString("host", mcp.Required(), mcp.Description("Host name")),
			),
			handler: s.handleHostConfig,
		},
		{
			tool: mcp.NewTool("search_repos",
				mcp.WithDescription("Search all configuration repos for a text pattern and return file:line matches."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Text to search for")),
				mcp.WithString("max_results", mcp.Description("Maximum number of matches")),
			),
			handler: s.handleSearchRepos,
		},
		{
			tool: mcp.NewTool("read_repo_file",
				mcp.WithDescription("Read a specific file from a configuration repo (path relative to repo root)."),
				mcp.WithString("repo", mcp.Required(), mcp.Description("Repo name from list_repos")),
				mcp.WithString("path", mcp.Required(), mcp.Description("File path relative to the repo root")),
			),
			handler: s.handleReadRepoFile,
		},
	}
}

func (s *Server) handleListRepos(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var sb strings.Builder
	for _, r := range s.deps.Repos.Repos() {
		fmt.Fprintf(&sb, "%s\t(%s)\t%s\n", r.Name, r.Type, r.Root)
	}
	if sb.Len() == 0 {
		return resultText("no repos configured"), nil
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}

func (s *Server) handleSyncRepos(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out, err := s.deps.Repos.Sync(ctx)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	return resultText(out), nil
}

func (s *Server) handleHostConfig(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	host := strArgs(request.GetArguments(), "host", "")
	out := s.deps.Repos.HostConfig(host)
	return resultText(out), nil
}

func (s *Server) handleSearchRepos(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q := strArgs(request.GetArguments(), "query", "")
	max := intArgs(request.GetArguments(), "max_results", 30)
	matches := s.deps.Repos.Search(q, max)
	if len(matches) == 0 {
		return resultText(fmt.Sprintf("no matches for %q", q)), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es) for %q:\n", len(matches), q)
	for _, m := range matches {
		text := m.Content
		if m.Context != "" {
			text = m.Context
		}
		fmt.Fprintf(&sb, "%s\t%s:%d\t%s\n", m.Repo, m.Path, m.Line, text)
	}
	return resultText(sb.String()), nil
}

func (s *Server) handleReadRepoFile(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	repo := strArgs(request.GetArguments(), "repo", "")
	path := strArgs(request.GetArguments(), "path", "")
	out, err := s.deps.Repos.FileSnippet(repo, path, 64*1024)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	return resultText(out), nil
}
