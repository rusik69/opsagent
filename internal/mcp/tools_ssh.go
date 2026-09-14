package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rusik69/opsagent/internal/model"
)

func (s *Server) toolsSSH() []toolReg {
	return []toolReg{
		{
			tool: mcp.NewTool("list_hosts",
				mcp.WithDescription("List all hosts that can be diagnosed via SSH.")),
			handler: s.handleListHosts,
		},
		{
			tool: mcp.NewTool("list_commands",
				mcp.WithDescription("List the limited, read-only diagnostic commands available to run on hosts.")),
			handler: s.handleListCommands,
		},
		{
			tool: mcp.NewTool("run_command",
				mcp.WithDescription("Run an allowlisted read-only diagnostic command on a host. Only commands listed by list_commands may be executed, and only with the parameters they accept. This tool never modifies the host."),
				mcp.WithString("host", mcp.Required(), mcp.Description("Host name as configured")),
				mcp.WithString("command_id", mcp.Required(), mcp.Description("ID of an allowlisted command")),
				mcp.WithObject("params", mcp.Description("Command parameters (validated against the allowlist)")),
			),
			handler: s.handleRunCommand,
		},
	}
}

func (s *Server) handleListHosts(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var sb strings.Builder
	for _, h := range s.deps.Resolver.Hosts() {
		fmt.Fprintf(&sb, "%s\t%s:%d (%s)\n", h.Name, h.Address, h.Port, h.User)
	}
	if sb.Len() == 0 {
		return resultText("no hosts configured"), nil
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}

func (s *Server) handleListCommands(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var sb strings.Builder
	for _, c := range s.deps.Executor.Allowlist().List() {
		fmt.Fprintf(&sb, "%s\t%s\n  template: %s\n", c.ID, c.Description, c.Template)
		if len(c.Params) > 0 {
			var names []string
			for p := range c.Params {
				names = append(names, p)
			}
			fmt.Fprintf(&sb, "  params: %s\n", strings.Join(names, ", "))
		}
	}
	return resultText(strings.TrimSuffix(sb.String(), "\n")), nil
}

func (s *Server) handleRunCommand(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := request.GetArguments()
	host := strArgs(args, "host", "")
	commandID := strArgs(args, "command_id", "")
	if host == "" || commandID == "" {
		return resultErr("host and command_id are required"), nil
	}
	params := map[string]string{}
	if raw, ok := args["params"].(map[string]any); ok {
		for k, v := range raw {
			params[k] = fmt.Sprint(v)
		}
	}
	var incidentID *int64
	if id := IncidentIDFrom(ctx); id != 0 {
		incidentID = &id
	}
	run, err := s.deps.Executor.Run(ctx, incidentID, host, commandID, params)
	if err != nil {
		msg := fmt.Sprintf("error: %v", err)
		if run != nil && run.Stderr != "" {
			msg += "\n" + strings.TrimSpace(run.Stderr)
		}
		return resultErr(msg), nil
	}
	return resultText(formatRun(run)), nil
}

func formatRun(r *model.CommandRun) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "$ %s\n", r.Command)
	if r.Stdout != "" {
		sb.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			sb.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		fmt.Fprintf(&sb, "[stderr] %s\n", strings.TrimSpace(r.Stderr))
	}
	fmt.Fprintf(&sb, "[exit: %s, %dms]", r.Status, r.DurationMS)
	return sb.String()
}
