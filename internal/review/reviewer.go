package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/rusik69/opsagent/internal/agent"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/store"
)

// Config controls the self-improvement review loop.
type Config struct {
	Limit         int
	MinIncidents  int
	MaxSummaryLen int
}

// Reviewer periodically reviews completed incidents and turns what it learns
// into memories and instructions for the diagnosis agent.
type Reviewer struct {
	store  *store.Store
	client *agent.Client
	mcp    *mcp.Server
	cfg    Config
}

func New(st *store.Store, client *agent.Client, mcpSrv *mcp.Server, cfg Config) *Reviewer {
	if cfg.Limit <= 0 {
		cfg.Limit = 50
	}
	if cfg.MinIncidents <= 0 {
		cfg.MinIncidents = 3
	}
	if cfg.MaxSummaryLen <= 0 {
		cfg.MaxSummaryLen = 4000
	}
	return &Reviewer{store: st, client: client, mcp: mcpSrv, cfg: cfg}
}

const reviewPrompt = `You are the opsagent retrospective reviewer. You analyze a batch of past incidents and their diagnoses to improve the agent.

For each incident consider: host, title, severity, status, root cause, confidence, whether a fix MR was merged (outcome), whether the incident recurred, and the diagnosis summary.

Produce a concise retrospective with:
- Recurring root causes and affected services/hosts
- Patterns where diagnosis was weak or wrong (low confidence, errors, recurrences)
- Concrete improvements: 1-3 memories (store_memory, topic = host or service or alert type, content = the lesson) and 0-2 self-improvement instructions (store_instruction with priority 1-10)
- Do NOT invent details. Only use evidence from the incidents below.
- End with a plain-text summary of the retrospective.

Incidents to review:
%s
`

// Run performs one review pass. It returns nil when there are not enough
// completed incidents yet.
func (r *Reviewer) Run(ctx context.Context) (*model.Retrospective, error) {
	incs, err := r.store.ListCompletedIncidents(ctx, r.cfg.Limit)
	if err != nil {
		return nil, err
	}
	if len(incs) < r.cfg.MinIncidents {
		return nil, nil
	}

	history := r.buildHistory(ctx, incs)
	messages := []agent.Message{
		{Role: "system", Content: "You are the opsagent retrospective reviewer. Follow the user instructions exactly."},
		{Role: "user", Content: fmt.Sprintf(reviewPrompt, history)},
	}
	tools := []agent.Tool{
		{Name: "store_memory", Description: "Store a reusable lesson learned.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"topic", "content"}}},
		{Name: "store_instruction", Description: "Store a self-improvement instruction.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"content": map[string]any{"type": "string"}, "priority": map[string]any{"type": "integer"}}, "required": []string{"content"}}},
	}

	retro := &model.Retrospective{
		WindowStart:      incs[len(incs)-1].CreatedAt,
		WindowEnd:        incs[0].CreatedAt,
		IncidentsReviewd: len(incs),
	}
	var summaryParts []string

	for step := 0; step < 6; step++ {
		msg, err := r.client.Completion(ctx, messages, tools)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
		if len(msg.ToolCalls) == 0 {
			if msg.Content != "" {
				summaryParts = append(summaryParts, msg.Content)
			}
			break
		}
		for _, tc := range msg.ToolCalls {
			args := agent.ParseToolArgs(tc.Func.Args)
			switch tc.Func.Name {
			case "store_memory":
				topic := str(args, "topic")
				content := str(args, "content")
				if topic == "" || content == "" {
					continue
				}
				exists, _ := r.store.MemoryExists(ctx, topic)
				if exists {
					continue
				}
				if _, err := r.store.CreateMemory(ctx, &model.Memory{Topic: topic, Content: content, Tags: []string{"review"}}); err != nil {
					continue
				}
				retro.MemoriesCreated++
				summaryParts = append(summaryParts, "memory: "+topic)
			case "store_instruction":
				content := str(args, "content")
				if content == "" {
					continue
				}
				exists, _ := r.store.InstructionExists(ctx, content)
				if exists {
					continue
				}
				priority := intArg(args, "priority", 5)
				if _, err := r.store.CreateInstruction(ctx, &model.Instruction{Content: content, Priority: priority, Source: "review"}); err != nil {
					continue
				}
				retro.InstructionsCreated++
				summaryParts = append(summaryParts, "instruction: "+content)
			}
			messages = append(messages, agent.Message{Role: "tool", ToolCallID: tc.ID, Content: "stored"})
		}
	}

	summary := strings.Join(summaryParts, "\n")
	if len(summary) > r.cfg.MaxSummaryLen {
		summary = summary[:r.cfg.MaxSummaryLen]
	}
	retro.Summary = summary
	return r.store.CreateRetrospective(ctx, retro)
}

func (r *Reviewer) buildHistory(ctx context.Context, incs []*model.Incident) string {
	var sb strings.Builder
	for _, inc := range incs {
		d, _ := r.store.GetDiagnosis(ctx, inc.ID)
		fmt.Fprintf(&sb, "#%d [%s] %s host=%s sev=%s root_cause=%q confidence=%q resolved_via=%q",
			inc.ID, inc.Status, inc.Title, inc.Host, inc.Severity, inc.RootCause, inc.Confidence, inc.ResolvedVia)
		if inc.MRURL != "" {
			fmt.Fprintf(&sb, " mr=%s", inc.MRURL)
		}
		if d != nil {
			if d.Summary != "" {
				fmt.Fprintf(&sb, "\n  summary: %s", d.Summary)
			}
			if len(d.Steps) > 0 {
				fmt.Fprintf(&sb, "\n  steps: %d (%s)", len(d.Steps), d.Steps[0].Tool)
			}
		}
		if has, _ := r.store.HasEvent(ctx, inc.ID, model.EventRecurrence); has {
			sb.WriteString("\n  RECURRED")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
func str(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}
