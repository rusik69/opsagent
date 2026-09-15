package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerResources registers MCP resource templates and prompts so external
// MCP clients (e.g. opencode) can read incidents and repos directly and use
// the diagnose prompt instead of only calling tools.
func (s *Server) registerResources(mcpServer *server.MCPServer) {
	mcpServer.AddResourceTemplate(
		mcp.NewResourceTemplate("incident://{id}", "Incident",
			mcp.WithTemplateDescription("Details of a single incident by numeric id"),
			mcp.WithTemplateMIMEType("text/plain"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			raw := strings.TrimPrefix(req.Params.URI, "incident://")
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("invalid incident id %q", raw)
			}
			inc, err := s.deps.Store.GetIncident(ctx, id)
			if err != nil {
				return nil, err
			}
			if inc == nil {
				return nil, fmt.Errorf("incident %d not found", id)
			}
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     formatIncident(inc),
			}}, nil
		},
	)

	mcpServer.AddResourceTemplate(
		mcp.NewResourceTemplate("repo://{name}", "Config repo",
			mcp.WithTemplateDescription("Information about a configured Puppet/Ansible/docs repo"),
			mcp.WithTemplateMIMEType("text/plain"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			name := strings.TrimPrefix(req.Params.URI, "repo://")
			for _, r := range s.deps.Repos.Repos() {
				if r.Name == name {
					return []mcp.ResourceContents{mcp.TextResourceContents{
						URI:      req.Params.URI,
						MIMEType: "text/plain",
						Text:     fmt.Sprintf("%s\t(%s)\t%s", r.Name, r.Type, r.Root),
					}}, nil
				}
			}
			return nil, fmt.Errorf("repo %q not found", name)
		},
	)

	mcpServer.AddPrompt(
		mcp.NewPrompt("diagnose",
			mcp.WithPromptDescription("Start a diagnosis for an incident by id"),
			mcp.WithArgument("incident_id", mcp.RequiredArgument()),
		),
		func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			raw := req.Params.Arguments["incident_id"]
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("invalid incident_id %q", raw)
			}
			inc, err := s.deps.Store.GetIncident(ctx, id)
			if err != nil {
				return nil, err
			}
			if inc == nil {
				return nil, fmt.Errorf("incident %d not found", id)
			}
			text := fmt.Sprintf("Diagnose incident #%d:\n\n%s", inc.ID, formatIncident(inc))
			return &mcp.GetPromptResult{
				Messages: []mcp.PromptMessage{
					mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(text)),
				},
			}, nil
		},
	)
}
