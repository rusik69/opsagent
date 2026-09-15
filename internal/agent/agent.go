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

	var total Usage

	llmTools, err := a.toolsForLLM()
	if err != nil {
		a.finish(ctx, d, "error", fmt.Sprintf("failed to load tools: %v", err), total)
		return d, err
	}

	step := 0
	toolCalls := 0
	seen := map[string]bool{}
	for {
		step++
		if step > a.maxSteps {
			// Force a final textual answer: without tools the LLM cannot call
			// anything and must produce the report.
			msg, usage, err := a.client.CompletionWithUsage(ctx, messages, nil)
			total.add(usage)
			if err != nil {
				a.finish(ctx, d, "error", fmt.Sprintf("llm error: %v", err), total)
				return d, err
			}
			messages = append(messages, msg)
			a.finish(ctx, d, "done", msg.Content, total)
			return d, nil
		}
		msg, usage, err := a.client.CompletionWithUsage(ctx, messages, llmTools)
		total.add(usage)
		if err != nil {
			a.finish(ctx, d, "error", fmt.Sprintf("llm error: %v", err), total)
			return d, err
		}
		messages = append(messages, msg)

		if len(msg.ToolCalls) == 0 {
			// Final answer: the diagnosis report.
			a.finish(ctx, d, "done", msg.Content, total)
			a.maybeReflect(ctx, incident, d, messages, llmTools)
			return d, nil
		}

		runCtx := mcp.WithIncident(ctx, incident.ID)
		for _, tc := range msg.ToolCalls {
			sig := tc.Func.Name + "\x00" + string(tc.Func.Args)
			if seen[sig] {
				// The model repeated an identical call; re-running it would
				// waste steps and tokens, so nudge it forward instead.
				messages = append(messages, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "duplicate of an earlier identical call; not re-run. Use the previous result.",
				})
				continue
			}
			seen[sig] = true
			if toolCalls >= maxToolCalls {
				messages = append(messages, Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Content:    "tool call budget exhausted; stop calling tools and produce the final report now.",
				})
				continue
			}
			toolCalls++

			args := ParseToolArgs(tc.Func.Args)
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
			output = truncateOutput(output, maxToolOutput)
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    output,
			})
			a.appendStep(d, step, tc.Func.Name, truncateOutput(string(tc.Func.Args), maxToolInput), output)
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

func (a *Agent) finish(ctx context.Context, d *model.Diagnosis, status, report string, usage Usage) {
	if report == "" {
		report = "No final report produced."
	}
	d.Status = status
	d.Report = report
	d.Summary = summarize(report, 500)
	d.Logs = formatUsage(usage)
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
		_, _ = a.mcp.Call(runCtx, tc.Func.Name, ParseToolArgs(tc.Func.Args))
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

// ParseToolArgs converts a tool call's arguments field into a map. OpenAI
// sends arguments as a JSON-encoded string (e.g. "{\"host\":\"web-01\"}");
// some providers send an object. Both forms are handled. It is exported so the
// review package can reuse the same parsing logic.
func ParseToolArgs(raw json.RawMessage) map[string]any {
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

// maxToolOutput caps how much of a single tool result is fed back to the LLM
// and persisted, so a verbose command cannot blow up the model context.
const maxToolOutput = 8000

// maxToolInput caps the tool-call arguments persisted with a diagnosis step.
const maxToolInput = 4000

// maxToolCalls bounds the total number of tool executions in one diagnosis,
// independent of the step count, to contain runaway loops.
const maxToolCalls = 40

// add accumulates usage counters.
func (u *Usage) add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
}

// formatUsage renders accumulated usage for persistence, or "" when the
// provider did not report any.
func formatUsage(u Usage) string {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return ""
	}
	return fmt.Sprintf("tokens: prompt=%d completion=%d total=%d", u.PromptTokens, u.CompletionTokens, u.TotalTokens)
}

// truncateOutput truncates s to at most max runes, appending a marker when it
// does so. It is rune-aware so multi-byte output is never split.
func truncateOutput(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\u2026[truncated]"
}

func summarize(s string, n int) string {
	one := strings.Join(strings.Fields(s), " ")
	if len(one) <= n {
		return one
	}
	return one[:n] + "..."
}
