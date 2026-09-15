package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rusik69/opsagent/internal/model"
)

const systemPromptTemplate = `You are the ops agent. You diagnose infrastructure incidents by running a limited set of READ-ONLY diagnostic commands over SSH, by reading the Puppet/Ansible configuration repositories that define the hosts, and by searching the documentation repos. When a fix is clear you propose it as a GitLab merge request.

Hard constraints:
- You may ONLY run commands exposed as MCP tools. Never invent commands.
- Every SSH command you run is read-only. You must never attempt to modify a host, restart services, or change configuration directly.
- Use run_command with command_id from list_commands and only the parameters those commands accept.
- Use get_host_config and search_repos to understand what the host is supposed to look like (services, packages, files) before jumping to conclusions.
- Use search_docs to consult documentation when a component or behaviour is unfamiliar. Prefer documented behaviour over guessing.
- Compare observed state against expected configuration to identify the cause.
- The ONLY mutation you may perform is create_gitlab_mr. Use it to propose a config fix with exact file paths and corrected content. Never use it for anything unrelated to the fix, and never include secrets.

Instructions you must follow:
%INSTRUCTIONS%

Memory relevant to this type of incident:
%MEMORY%

Host configuration context:
%HOSTCONFIG%

Workflow:
1. Recall memory and gather host configuration context.
2. Search documentation if a component/behaviour is unfamiliar.
3. Run a small number of targeted read-only commands to gather evidence.
4. When you have enough evidence, stop calling tools and produce the final diagnosis report.
5. If a concrete, low-risk config fix is clear, call create_gitlab_mr and reference the returned MR URL in the report.
6. Before producing the final report, call set_incident_outcome with the root cause and your confidence (high/medium/low). If other open incidents share the same root cause, link them with correlate_incidents.

The final report MUST include:
- Root cause hypothesis (what and why)
- Evidence supporting it (command outputs, config files, docs)
- Confidence level (high/medium/low)
- Recommended next steps for a human operator (read-only suggestions only)
- MR URL if a merge request was created`

func (a *Agent) systemPrompt(inc *model.Incident) Message {
	instr := a.loadInstructions()
	mem := a.recallFor(inc)
	hostCfg := a.hostConfigFor(inc.Host)
	agentsRules := a.loadRulesFile()

	p := systemPromptTemplate
	p = strings.ReplaceAll(p, "%INSTRUCTIONS%", instr)
	p = strings.ReplaceAll(p, "%MEMORY%", mem)
	p = strings.ReplaceAll(p, "%HOSTCONFIG%", hostCfg)
	if agentsRules != "" {
		p += "\n\n## Agent rules and paths (from " + a.instructionsFile + ")\n" + agentsRules
	}
	return Message{Role: "system", Content: p}
}

// loadRulesFile returns the AGENTS.md-style instructions file (rules and
// paths) configured in agent.instructions_file. The file is cached and only
// re-read when its mtime or size changes, so edits still take effect without a
// restart while avoiding a read on every diagnosis.
func (a *Agent) loadRulesFile() string {
	if a.instructionsFile == "" {
		return ""
	}
	fi, err := os.Stat(a.instructionsFile)
	if err != nil {
		return a.rulesCache
	}
	a.rulesMu.Lock()
	defer a.rulesMu.Unlock()
	if fi.ModTime().Equal(a.rulesModTime) && fi.Size() == a.rulesFileSize {
		return a.rulesCache
	}
	data, err := os.ReadFile(a.instructionsFile)
	if err != nil {
		return a.rulesCache
	}
	a.rulesCache = strings.TrimSpace(string(data))
	a.rulesModTime = fi.ModTime()
	a.rulesFileSize = fi.Size()
	return a.rulesCache
}

func (a *Agent) incidentPrompt(inc *model.Incident) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `Incident #%d to diagnose:
- host: %s
- severity: %s
- title: %s
- message: %s
- labels: %v
`, inc.ID, inc.Host, inc.Severity, inc.Title, inc.Message, inc.Labels)
	if len(inc.Tags) > 0 {
		fmt.Fprintf(&sb, "- tags: %v\n", inc.Tags)
	}
	if inc.Owner != "" {
		fmt.Fprintf(&sb, "- owner: %s\n", inc.Owner)
	}
	if inc.Team != "" {
		fmt.Fprintf(&sb, "- team: %s\n", inc.Team)
	}
	sb.WriteString("\nDiagnose this incident now.")
	return sb.String()
}

// maxInjectedInstructions caps how many not-yet-applied instructions are
// injected into a diagnosis prompt, keeping the system prompt bounded.
const maxInjectedInstructions = 20

func (a *Agent) loadInstructions() string {
	items, err := a.store.ListInstructions(context.Background(), maxInjectedInstructions*3)
	if err != nil || len(items) == 0 {
		return "None."
	}
	var sb strings.Builder
	injected := 0
	for _, i := range items {
		if i.Applied {
			continue
		}
		if injected >= maxInjectedInstructions {
			fmt.Fprintf(&sb, "- ... (%d further instructions not shown)\n", len(items)-injected)
			break
		}
		fmt.Fprintf(&sb, "- (priority %d) %s\n", i.Priority, i.Content)
		injected++
	}
	if sb.Len() == 0 {
		return "None."
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func (a *Agent) recallFor(inc *model.Incident) string {
	queries := []string{inc.Host, inc.Title}
	if t, ok := inc.Labels["alertname"]; ok {
		queries = append(queries, t)
	}
	seen := map[string]bool{}
	var sb strings.Builder
	for _, q := range queries {
		if q == "" {
			continue
		}
		items, err := a.store.SearchMemories(context.Background(), q, 5)
		if err != nil {
			continue
		}
		for _, m := range items {
			key := fmt.Sprintf("%d", m.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			fmt.Fprintf(&sb, "- [%s] %s\n", m.Topic, m.Content)
		}
	}
	if sb.Len() == 0 {
		return "None."
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// hostConfigFor is injected from repos via a callback set in New.
func (a *Agent) hostConfigFor(host string) string {
	if a.hostConfigFn != nil {
		if out := a.hostConfigFn(host); out != "" {
			return out
		}
	}
	return "No configuration context available."
}

// toolsForLLM converts the MCP tool list into OpenAI function tool schemas.
func (a *Agent) toolsForLLM() ([]Tool, error) {
	tools := []Tool{}
	for name, st := range a.mcp.MCPServer().ListTools() {
		schema := map[string]any{
			"type":       st.Tool.InputSchema.Type,
			"properties": st.Tool.InputSchema.Properties,
		}
		if len(st.Tool.InputSchema.Required) > 0 {
			schema["required"] = st.Tool.InputSchema.Required
		}
		tools = append(tools, Tool{
			Name:        name,
			Description: st.Tool.Description,
			Parameters:  schema,
		})
	}
	return tools, nil
}
