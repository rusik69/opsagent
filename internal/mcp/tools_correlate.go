package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rusik69/opsagent/internal/model"
)

func (s *Server) toolsCorrelate() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("get_related_incidents",
				mcp.WithDescription("Return incidents correlated with the given incident and the correlation groups it belongs to."),
				mcp.WithString("id", mcp.Required(), mcp.Description("Incident id")),
			),
			handler: s.handleGetRelated,
		},
		{
			tool: mcp.NewTool("correlate_incidents",
				mcp.WithDescription("Explicitly link an incident to other related incidents (e.g. a shared root cause discovered during diagnosis)."),
				mcp.WithString("incident_id", mcp.Required(), mcp.Description("Primary incident id")),
				mcp.WithArray("related_incident_ids", mcp.Required(), mcp.Description("Array of incident ids believed to be related")),
				mcp.WithString("reason", mcp.Description("Short reason, e.g. 'host down causes all alerts on it'")),
			),
			handler: s.handleCorrelateIncidents,
		},
		{
			tool: mcp.NewTool("set_incident_outcome",
				mcp.WithDescription("Record the outcome of a diagnosis: root cause, confidence, and how it was resolved. Call this at the end of a diagnosis."),
				mcp.WithString("incident_id", mcp.Required(), mcp.Description("Incident id")),
				mcp.WithString("root_cause", mcp.Description("Root cause in a few words")),
				mcp.WithString("confidence", mcp.Description("high, medium or low")),
				mcp.WithString("resolved_via", mcp.Description("Optional: how resolved (mr_merged, manual, auto)")),
			),
			handler: s.handleSetOutcome,
		},
		{
			tool: mcp.NewTool("get_incident_history",
				mcp.WithDescription("Return the append-only timeline of events for an incident."),
				mcp.WithString("id", mcp.Required(), mcp.Description("Incident id")),
			),
			handler: s.handleGetHistory,
		},
		{
			tool: mcp.NewTool("list_retrospectives",
				mcp.WithDescription("List past self-improvement reviews over incident history, with the memories and instructions they produced.")),
			handler: s.handleListRetrospectives,
		},
		{
			tool: mcp.NewTool("add_note",
				mcp.WithDescription("Append a free-text note to an incident's timeline (visible to operators and in future diagnoses)."),
				mcp.WithString("incident_id", mcp.Required(), mcp.Description("Incident id")),
				mcp.WithString("note", mcp.Required(), mcp.Description("Note text")),
			),
			handler: s.handleAddNote,
		},
	}
}

func (s *Server) handleAddNote(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	id := int64(intArgs(args, "incident_id", 0))
	note := strArgs(args, "note", "")
	if id <= 0 || note == "" {
		return resultErr("incident_id and note are required"), nil
	}
	if _, err := s.deps.Store.AddEvent(ctx, id, model.EventNote, note); err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	return resultText(fmt.Sprintf("note added to incident #%d", id)), nil
}

func (s *Server) handleGetRelated(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := int64(intArgs(request.GetArguments(), "id", 0))
	if id <= 0 {
		return resultErr("invalid incident id"), nil
	}
	groups, err := s.deps.Store.GroupByIncident(ctx, id)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	related, err := s.deps.Store.RelatedIncidentIDs(ctx, id)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	var sb strings.Builder
	if len(groups) == 0 {
		sb.WriteString("no correlation groups")
	} else {
		fmt.Fprintf(&sb, "groups (%d):\n", len(groups))
		for _, g := range groups {
			fmt.Fprintf(&sb, "- %s (%s)\n", g.Label, g.Kind)
		}
	}
	if len(related) > 0 {
		fmt.Fprintf(&sb, "\nrelated incidents: %v\n", related)
		items := []*model.Incident{}
		for _, rid := range related {
			if inc, err := s.deps.Store.GetIncident(ctx, rid); err == nil && inc != nil {
				items = append(items, inc)
			}
		}
		for _, inc := range items {
			fmt.Fprintf(&sb, "  #%d [%s] %s %q\n", inc.ID, inc.Status, inc.Host, inc.Title)
		}
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}

func (s *Server) handleCorrelateIncidents(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.deps.Correlate == nil {
		return resultErr("correlation is not configured"), nil
	}
	args := request.GetArguments()
	primary := int64(intArgs(args, "incident_id", 0))
	if primary <= 0 {
		return resultErr("incident_id is required"), nil
	}
	raw, ok := args["related_incident_ids"].([]any)
	if !ok {
		return resultErr("related_incident_ids must be an array of ids"), nil
	}
	related := []int64{}
	for _, r := range raw {
		if f, ok := r.(float64); ok && int64(f) > 0 {
			related = append(related, int64(f))
		}
	}
	if len(related) == 0 {
		return resultErr("related_incident_ids must contain at least one id"), nil
	}
	reason := strArgs(args, "reason", "")
	g, err := s.deps.Correlate.LinkByAgent(ctx, primary, related, reason)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	return resultText(fmt.Sprintf("correlated %d incidents into group %q (kind=%s)", 1+len(related), g.Label, g.Kind)), nil
}

func (s *Server) handleSetOutcome(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	id := int64(intArgs(args, "incident_id", 0))
	if id <= 0 {
		return resultErr("incident_id is required"), nil
	}
	rootCause := strArgs(args, "root_cause", "")
	confidence := strArgs(args, "confidence", "")
	resolvedVia := strArgs(args, "resolved_via", "")
	if rootCause == "" && confidence == "" && resolvedVia == "" {
		return resultErr("nothing to record (root_cause, confidence, resolved_via all empty)"), nil
	}
	if err := s.deps.Store.UpdateIncidentOutcome(ctx, id, rootCause, confidence, resolvedVia); err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	_, _ = s.deps.Store.AddEvent(ctx, id, model.EventOutcomeSet, fmt.Sprintf("root cause: %s; confidence: %s; resolved via: %s", rootCause, confidence, resolvedVia))
	return resultText(fmt.Sprintf("outcome recorded for incident #%d", id)), nil
}

func (s *Server) handleGetHistory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := int64(intArgs(request.GetArguments(), "id", 0))
	if id <= 0 {
		return resultErr("invalid incident id"), nil
	}
	events, err := s.deps.Store.ListEvents(ctx, id)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	if len(events) == 0 {
		return resultText("no events recorded yet"), nil
	}
	var sb strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sb, "%s  %s  %s\n", e.CreatedAt.Format("2006-01-02 15:04:05"), e.Kind, e.Detail)
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}

func (s *Server) handleListRetrospectives(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	items, err := s.deps.Store.ListRetrospectives(ctx, 20)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	if len(items) == 0 {
		return resultText("no retrospectives yet"), nil
	}
	var sb strings.Builder
	for _, r := range items {
		fmt.Fprintf(&sb, "== retrospective #%d (%s) ==\n%s\n", r.ID, r.CreatedAt.Format("2006-01-02 15:04"), r.Summary)
		sb.WriteString("\n")
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}
