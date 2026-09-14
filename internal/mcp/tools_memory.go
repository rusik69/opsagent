package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rusik69/opsagent/internal/model"
)

func (s *Server) toolsMemory() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("recall_memory",
				mcp.WithDescription("Recall past learnings relevant to a topic (host, service, alert type)."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Topic to search memory for")),
			),
			handler: s.handleRecallMemory,
		},
		{
			tool: mcp.NewTool("store_memory",
				mcp.WithDescription("Store a lesson learned that should be recalled for future incidents."),
				mcp.WithString("topic", mcp.Required(), mcp.Description("Short topic label, e.g. service or host")),
				mcp.WithString("content", mcp.Required(), mcp.Description("The lesson or observation")),
				mcp.WithString("tags", mcp.Description("Comma-separated tags")),
			),
			handler: s.handleStoreMemory,
		},
		{
			tool: mcp.NewTool("store_instruction",
				mcp.WithDescription("Store a self-improvement instruction for the agent to follow in future diagnoses."),
				mcp.WithString("content", mcp.Required(), mcp.Description("Instruction text")),
				mcp.WithString("priority", mcp.Description("Priority, lower number = higher priority (default 5)")),
			),
			handler: s.handleStoreInstruction,
		},
		{
			tool: mcp.NewTool("list_instructions",
				mcp.WithDescription("List the self-improvement instructions the agent should follow.")),
			handler: s.handleListInstructions,
		},
	}
}

func (s *Server) handleRecallMemory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	q := strArgs(request.GetArguments(), "query", "")
	if q == "" {
		return resultText("no query given"), nil
	}
	items, err := s.deps.Store.SearchMemories(ctx, q, 10)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	if len(items) == 0 {
		return resultText(fmt.Sprintf("no memory matching %q", q)), nil
	}
	var sb strings.Builder
	for _, m := range items {
		fmt.Fprintf(&sb, "== %s (%s) ==\n%s\n", m.Topic, strings.Join(m.Tags, ","), m.Content)
		sb.WriteString("\n")
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}

func (s *Server) handleStoreMemory(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	topic := strArgs(request.GetArguments(), "topic", "")
	content := strArgs(request.GetArguments(), "content", "")
	if topic == "" || content == "" {
		return resultErr("topic and content are required"), nil
	}
	tags := []string{}
	if t := strArgs(request.GetArguments(), "tags", ""); t != "" {
		for _, tag := range strings.Split(t, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				tags = append(tags, tag)
			}
		}
	}
	mem, err := s.deps.Store.CreateMemory(ctx, &model.Memory{Topic: topic, Content: content, Tags: tags})
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	return resultText(fmt.Sprintf("memory stored (id %d)", mem.ID)), nil
}

func (s *Server) handleStoreInstruction(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	content := strArgs(request.GetArguments(), "content", "")
	if content == "" {
		return resultErr("content is required"), nil
	}
	priority := intArgs(request.GetArguments(), "priority", 5)
	ins, err := s.deps.Store.CreateInstruction(ctx, &model.Instruction{
		Content:  content,
		Priority: priority,
		Source:   "agent",
	})
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	return resultText(fmt.Sprintf("instruction stored (id %d, priority %d)", ins.ID, priority)), nil
}

func (s *Server) handleListInstructions(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	items, err := s.deps.Store.ListInstructions(ctx, 50)
	if err != nil {
		return resultErr(fmt.Sprintf("error: %v", err)), nil
	}
	if len(items) == 0 {
		return resultText("no instructions stored"), nil
	}
	var sb strings.Builder
	for _, i := range items {
		applied := "not applied"
		if i.Applied {
			applied = "applied"
		}
		fmt.Fprintf(&sb, "[p%d] %s (%s)\n", i.Priority, i.Content, applied)
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}
