package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) toolsDocs() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("search_docs",
				mcp.WithDescription("Search the documentation repos for a query and return file:line matches with surrounding content."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Text to search for in the documentation")),
				mcp.WithString("max_results", mcp.Description("Maximum number of matches")),
			),
			handler: s.handleSearchDocs,
		},
		{
			tool: mcp.NewTool("list_docs",
				mcp.WithDescription("List the documentation repos available to search.")),
			handler: s.handleListDocs,
		},
	}
}

func (s *Server) handleSearchDocs(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q := strArgs(request.GetArguments(), "query", "")
	if q == "" {
		return resultErr("query is required"), nil
	}
	max := intArgs(request.GetArguments(), "max_results", 30)
	matches := s.deps.Repos.DocsSearch(q, max)
	if len(matches) == 0 {
		return resultText(fmt.Sprintf("no documentation matches for %q", q)), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d doc match(es) for %q:\n", len(matches), q)
	for _, m := range matches {
		text := m.Content
		if m.Context != "" {
			text = m.Context
		}
		fmt.Fprintf(&sb, "%s\t%s:%d\t%s\n", m.Repo, m.Path, m.Line, text)
	}
	return resultText(sb.String()), nil
}

func (s *Server) handleListDocs(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var sb strings.Builder
	for _, r := range s.deps.Repos.Repos() {
		if r.Type != "docs" {
			continue
		}
		fmt.Fprintf(&sb, "%s\t(%s)\t%s\n", r.Name, r.Type, r.Root)
	}
	if sb.Len() == 0 {
		return resultText("no documentation repos configured (add repos with type: docs)"), nil
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}
