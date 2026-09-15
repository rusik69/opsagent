package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rusik69/opsagent/internal/correlate"
	"github.com/rusik69/opsagent/internal/gitlab"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
)

// Deps are the internal services the MCP server exposes as tools.
type Deps struct {
	Executor  *sshx.Executor
	Repos     *repos.Manager
	Store     *store.Store
	Resolver  sshx.HostResolver
	GitLab    *gitlab.Client
	Correlate *correlate.Engine
}

// toolReg pairs a tool definition with its handler.
type toolReg struct {
	tool    mcp.Tool
	handler server.ToolHandlerFunc
}

// Server wraps an MCP server with the opsagent tool set.
type Server struct {
	deps Deps
	mcp  *server.MCPServer
}

// NewServer builds the MCP server and registers all tools.
func NewServer(name, version string, deps Deps) (*Server, error) {
	s := &Server{deps: deps}
	mcpServer := server.NewMCPServer(name, version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
		server.WithLogging(),
	)
	s.mcp = mcpServer
	s.registerResources(mcpServer)

	regs := []toolReg{}
	regs = append(regs, s.toolsSSH()...)
	regs = append(regs, s.toolsRepos()...)
	regs = append(regs, s.toolsDocs()...)
	regs = append(regs, s.toolsIncidents()...)
	regs = append(regs, s.toolsMemory()...)
	regs = append(regs, s.toolsGitLab()...)
	regs = append(regs, s.toolsCorrelate()...)
	for _, r := range regs {
		mcpServer.AddTool(r.tool, r.handler)
	}
	return s, nil
}

// MCPServer returns the underlying MCP server (for HTTP transport mounting).
func (s *Server) MCPServer() *server.MCPServer { return s.mcp }

// Call executes an MCP tool in-process using the same code path external
// clients use over the wire.
func (s *Server) Call(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(1),
		Params:  mcp.CallToolParams{Name: tool, Arguments: args},
	}
	req.Method = string(mcp.MethodToolsCall)
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal mcp call: %w", err)
	}
	resp := s.mcp.HandleMessage(ctx, raw)
	switch r := resp.(type) {
	case mcp.JSONRPCResponse:
		data, err := json.Marshal(r.Result)
		if err != nil {
			return nil, err
		}
		var ctr mcp.CallToolResult
		if err := json.Unmarshal(data, &ctr); err != nil {
			return nil, err
		}
		return &ctr, nil
	case mcp.JSONRPCError:
		return nil, errors.New(r.Error.Message)
	default:
		return nil, fmt.Errorf("unexpected mcp response type %T", resp)
	}
}

// CallText executes a tool and returns the textual content of its result.
func (s *Server) CallText(ctx context.Context, tool string, args map[string]any) (string, error) {
	res, err := s.Call(ctx, tool, args)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", errors.New(textOf(res))
	}
	return textOf(res), nil
}

func textOf(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if t, ok := c.(mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// arg helpers
func strArgs(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return def
}

func intArgs(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

func boolArgs(args map[string]any, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

// strSliceArgs extracts an array-of-objects argument without panicking on
// malformed input. It returns the items that are plain string/object maps.
func strSliceArgs(args map[string]any, key string) ([]map[string]any, bool) {
	raw, ok := args[key]
	if !ok {
		return nil, false
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := []map[string]any{}
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, len(out) > 0 || len(arr) == 0
}

func resultText(s string) *mcp.CallToolResult {
	return mcp.NewToolResultText(s)
}

func resultErr(s string) *mcp.CallToolResult {
	return mcp.NewToolResultError(s)
}
