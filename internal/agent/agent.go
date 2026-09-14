package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/store"
)

// Agent drives the diagnosis of incidents using an LLM that calls the
// internal MCP server. All commands it may run are read-only and limited to
// the allowlist.
type Agent struct {
	client           *Client
	mcp              *mcp.Server
	store            *store.Store
	maxSteps         int
	timeout          time.Duration
	hostConfigFn     func(host string) string
	instructionsFile string
}

type Options struct {
	MaxSteps int
	Timeout  time.Duration
	// HostConfig optionally provides configuration context from the
	// Puppet/Ansible repos for a given host.
	HostConfig func(host string) string
	// InstructionsFile is a path to an AGENTS.md-style file with rules and
	// paths that is injected verbatim into every LLM request.
	InstructionsFile string
}

func New(client *Client, mcpServer *mcp.Server, st *store.Store, opts Options) *Agent {
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 12
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Minute
	}
	return &Agent{client: client, mcp: mcpServer, store: st, maxSteps: opts.MaxSteps, timeout: opts.Timeout, hostConfigFn: opts.HostConfig, instructionsFile: opts.InstructionsFile}
}

// Diagnose runs the agent loop for an incident and persists the diagnosis.
func (a *Agent) Diagnose(ctx context.Context, incident *model.Incident) (*model.Diagnosis, error) {
	d, err := a.store.CreateDiagnosis(ctx, &model.Diagnosis{
		IncidentID: incident.ID,
		Status:     "running",
	})
	if err != nil {
		return nil, err
	}
	if err := a.store.UpdateIncidentStatus(ctx, incident.ID, model.IncidentDiagnosing); err != nil {
		return nil, err
	}
	_, _ = a.store.AddEvent(ctx, incident.ID, model.EventDiagnosisStart, fmt.Sprintf("diagnosis started (max steps %d)", a.maxSteps))

	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	messages := []Message{}
	messages = append(messages, a.systemPrompt(incident))
	messages = append(messages, Message{Role: "user", Content: a.incidentPrompt(incident)})

	llmTools, err := a.toolsForLLM()
	if err != nil {
		a.finish(ctx, d, "error", fmt.Sprintf("failed to load tools: %v", err), messages)
		return d, err
	}

	step := 0
	for {
		step++
		if step > a.maxSteps {
			// Force a final textual answer: without tools the LLM cannot call
			// anything and must produce the report.
			msg, err := a.client.Completion(ctx, messages, nil)
			if err != nil {
				a.finish(ctx, d, "error", fmt.Sprintf("llm error: %v", err), messages)
				return d, err
			}
			messages = append(messages, msg)
			a.finish(ctx, d, "done", msg.Content, messages)
			return d, nil
		}
		msg, err := a.client.Completion(ctx, messages, llmTools)
		if err != nil {
			a.finish(ctx, d, "error", fmt.Sprintf("llm error: %v", err), messages)
			return d, err
		}
		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			// Final answer: the diagnosis report.
			a.finish(ctx, d, "done", msg.Content, messages)
			a.maybeReflect(ctx, incident, d, messages, llmTools)
			return d, nil
		}

		runCtx := mcp.WithIncident(ctx, incident.ID)
		for _, tc := range msg.ToolCalls {
			args := parseToolArgs(tc.Func.Args)
			res, err := a.mcp.Call(runCtx, tc.Func.Name, args)
			output := ""
			if err != nil {
				output = "error: " + err.Error()
			} else {
				output = textContent(res.Content)
				if res.IsError {
					output = "tool error: " + output
				}
			}
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
			a.appendStep(d, step, tc.Func.Name, string(tc.Func.Args), output)
		}
	}
}

func (a *Agent) appendStep(d *model.Diagnosis, step int, tool, input, output string) {
	d.Steps = append(d.Steps, model.DiagnosisStep{
		Step:      step,
		Tool:      tool,
		Input:     input,
		Output:    output,
		Timestamp: time.Now().UTC(),
	})
}

func (a *Agent) finish(ctx context.Context, d *model.Diagnosis, status, report string, messages []Message) {
	if report == "" {
		report = "No final report produced."
	}
	d.Status = status
	d.Report = report
	d.Summary = summarize(report, 500)
	_ = a.store.UpdateDiagnosis(ctx, d)
	switch status {
	case "done":
		_ = a.store.UpdateIncidentStatusIfActive(ctx, d.IncidentID, model.IncidentDiagnosed)
		_, _ = a.store.AddEvent(ctx, d.IncidentID, model.EventDiagnosed, d.Summary)
	default:
		_ = a.store.UpdateIncidentStatusIfActive(ctx, d.IncidentID, model.IncidentError)
		_, _ = a.store.AddEvent(ctx, d.IncidentID, model.EventError, d.Summary)
	}
}

// maybeReflect asks the LLM whether the diagnosis revealed a reusable lesson
// and stores it as memory or an instruction if so.
func (a *Agent) maybeReflect(ctx context.Context, incident *model.Incident, d *model.Diagnosis, messages []Message, tools []Tool) {
	reflection := Message{Role: "user", Content: `Reflect on this diagnosis. If there is a clear, reusable lesson or improvement instruction for future diagnoses of this kind, call store_memory or store_instruction. Otherwise reply with the single word: none.`}
	msgs := []Message{}
	msgs = append(msgs, messages[:len(messages)-1]...)
	msgs = append(msgs, reflection)
	msg, err := a.client.Completion(ctx, msgs, tools)
	if err != nil || len(msg.ToolCalls) == 0 {
		return
	}
	runCtx := mcp.WithIncident(ctx, incident.ID)
	for _, tc := range msg.ToolCalls {
		if tc.Func.Name != "store_memory" && tc.Func.Name != "store_instruction" {
			continue
		}
		_, _ = a.mcp.Call(runCtx, tc.Func.Name, parseToolArgs(tc.Func.Args))
	}
}

func textContent(content []mcpsdk.Content) string {
	for _, c := range content {
		if t, ok := c.(mcpsdk.TextContent); ok && t.Text != "" {
			return t.Text
		}
	}
	return fmt.Sprintf("%v", content)
}

// parseToolArgs converts a tool call's arguments field into a map. OpenAI
// sends arguments as a JSON-encoded string (e.g. "{\"host\":\"web-01\"}");
// some providers send an object. Both forms are handled.
func parseToolArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err == nil {
		return args
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			args = nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	return args
}

func summarize(s string, n int) string {
	one := strings.Join(strings.Fields(s), " ")
	if len(one) <= n {
		return one
	}
	return one[:n] + "..."
}
