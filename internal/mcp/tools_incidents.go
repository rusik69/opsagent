package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rusik69/opsagent/internal/model"
)

func (s *Server) toolsIncidents() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("get_incident",
				mcp.WithDescription("Get details of a specific incident by id."),
				mcp.WithString("id", mcp.Required(), mcp.Description("Incident id")),
			),
			handler: s.handleGetIncident,
		},
		{
			tool: mcp.NewTool("list_incidents",
				mcp.WithDescription("List recent incidents, optionally filtered by status."),
				mcp.WithString("status", mcp.Description("Filter: open, diagnosing, diagnosed, resolved, cancelled")),
				mcp.WithString("limit", mcp.Description("Maximum number of incidents")),
			),
			handler: s.handleListIncidents,
		},
	}
}

func (s *Server) handleGetIncident(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := intArgs(request.GetArguments(), "id", 0)
	if id <= 0 {
		return resultErr("invalid incident id"), nil
	}
	inc, err := s.deps.Store.GetIncident(ctx, int64(id))
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	if inc == nil {
		return resultErr("incident not found"), nil
	}
	return resultText(formatIncident(inc)), nil
}

func (s *Server) handleListIncidents(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	status := strArgs(request.GetArguments(), "status", "")
	limit := intArgs(request.GetArguments(), "limit", 20)
	incs, err := s.deps.Store.ListIncidents(ctx, status, limit)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	var sb strings.Builder
	for _, inc := range incs {
		fmt.Fprintf(&sb, "#%d [%s] %s (%s) %q\n", inc.ID, inc.Status, inc.Host, inc.Severity, inc.Title)
	}
	return resultText(sb.String()), nil
}

func formatIncident(inc *model.Incident) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "id: %d\nsource: %s\nexternal_id: %s\nhost: %s\nseverity: %s\nstatus: %s\ntitle: %s\nmessage: %s\ncreated: %s\n",
		inc.ID, inc.Source, inc.ExternalID, inc.Host, inc.Severity, inc.Status, inc.Title, inc.Message, inc.CreatedAt.Format("2006-01-02 15:04:05"))
	if len(inc.Labels) > 0 {
		fmt.Fprintf(&sb, "labels: %v\n", inc.Labels)
	}
	return sb.String()
}
