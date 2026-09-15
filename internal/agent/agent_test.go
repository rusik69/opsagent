package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rusik69/opsagent/internal/incidents"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
)

// fakeLLM simulates an OpenAI-compatible /chat/completions endpoint.
// Turn 1: return a tool_call for run_command(uptime).
// Turn 2: return the final diagnosis report.
// Turn 3+ (reflection): no tool calls, plain "none".
func fakeLLM(t *testing.T, gotCmd *string) *httptest.Server {
	t.Helper()
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		turn++
		var req struct {
			Messages []Message `json:"messages"`
			Tools    []oaiTool `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		var resp chatResponse
		switch turn {
		case 1:
			resp = chatResponse{Choices: []choice{{
				Message: Message{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID: "call_1", Type: "function",
						Func: FunctionCall{Name: "run_command", Args: json.RawMessage(`{"host":"web-01","command_id":"uptime"}`)},
					}},
				},
			}}}
		case 2:
			resp = chatResponse{Choices: []choice{{
				Message: Message{Role: "assistant", Content: "REPORT: load is fine; root cause is high CPU. Confidence: medium."},
			}}}
		default:
			resp = chatResponse{Choices: []choice{{Message: Message{Role: "assistant", Content: "none"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		_ = gotCmd
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAgent(t *testing.T, gotCmd *string) (*Agent, *store.Store, *model.Incident) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/agent.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	al, err := sshx.NewAllowlist(sshx.DefaultAllowlist())
	if err != nil {
		t.Fatal(err)
	}
	runner := &sshx.FakeRunner{Outputs: map[string]string{"uptime": "load average: 8.0, 7.5, 7.0"}}
	exec := sshx.NewExecutor(al, runner, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})

	rm, err := repos.NewManager(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv, err := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}

	llm := NewClient(fakeLLM(t, gotCmd).URL+"/v1", "test", "fake-model")
	ag := New(llm, mcpSrv, st, Options{MaxSteps: 3, Timeout: 0})

	incSvc := incidents.NewService(st)
	inc, err := incSvc.CreateGeneric(context.Background(), incidents.GenericIncident{
		Host: "web-01", Title: "high CPU", Severity: "critical", Message: "load high",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag, st, inc
}

func TestDiagnosePipeline(t *testing.T) {
	ag, st, inc := newAgent(t, nil)
	d, err := ag.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if d.Status != "done" {
		t.Fatalf("expected status done, got %q", d.Status)
	}
	if !strings.Contains(d.Report, "REPORT") {
		t.Fatalf("expected report, got %q", d.Report)
	}
	if len(d.Steps) == 0 {
		t.Fatal("expected at least one step")
	}
	if d.Steps[0].Tool != "run_command" {
		t.Fatalf("expected first step run_command, got %q", d.Steps[0].Tool)
	}

	got, err := st.GetIncident(context.Background(), inc.ID)
	if err != nil || got == nil {
		t.Fatalf("get incident: %v", err)
	}
	if got.Status != model.IncidentDiagnosed {
		t.Fatalf("expected incident diagnosed, got %q", got.Status)
	}

	runs, err := st.ListAllCommandRuns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].CommandID != "uptime" {
		t.Fatalf("expected 1 uptime run, got %+v", runs)
	}
	if runs[0].IncidentID == nil || *runs[0].IncidentID != inc.ID {
		t.Fatal("expected command run linked to incident")
	}
}

func TestLoadRulesFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/AGENTS.md"
	content := "# agent rules\n- use run_command\n- paths: repos/docs\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	exec := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})
	ag := New(NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, Options{InstructionsFile: path})

	inc := &model.Incident{ID: 1, Host: "web-01", Title: "x", Labels: map[string]string{}}
	sys := ag.systemPrompt(inc)
	if !strings.Contains(sys.Content, strings.TrimSpace(content)) {
		t.Fatalf("expected instructions file injected into system prompt, got:\n%s", sys.Content)
	}
}

func TestSystemPromptMentionsMRTool(t *testing.T) {
	st, _ := store.Open(t.TempDir() + "/p.db")
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	exec := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})
	ag := New(NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, Options{InstructionsFile: ""})
	inc := &model.Incident{ID: 1, Host: "web-01", Title: "x", Labels: map[string]string{}}
	sys := ag.systemPrompt(inc)
	if !strings.Contains(sys.Content, "create_gitlab_mr") {
		t.Fatalf("expected MR tool mentioned in system prompt, got:\n%s", sys.Content)
	}
}

// alwaysToolCallLLM returns a run_command tool call on every request.
func alwaysToolCallLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"run_command","arguments":"{\"host\":\"web-01\",\"command_id\":\"uptime\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiagnoseMaxStepsTerminates(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/max.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	exec := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})

	llm := NewClient(alwaysToolCallLLM(t).URL+"/v1", "", "m")
	ag := New(llm, mcpSrv, st, Options{MaxSteps: 3, Timeout: 0})
	inc, _ := st.CreateIncident(context.Background(), &model.Incident{
		Host: "web-01", Title: "t", Status: model.IncidentOpen,
	})
	done := make(chan *model.Diagnosis, 1)
	go func() {
		d, _ := ag.Diagnose(context.Background(), inc)
		done <- d
	}()
	select {
	case d := <-done:
		if d.Status != "done" {
			t.Fatalf("expected done status after max steps, got %q", d.Status)
		}
		if len(d.Steps) != 3 {
			t.Fatalf("expected 3 recorded steps, got %d", len(d.Steps))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("diagnosis did not terminate at max steps")
	}
}

// reflectLLM returns a tool call, then the final report, then a store_memory
// tool call (the reflection), then "none".
func reflectLLM(t *testing.T) *httptest.Server {
	t.Helper()
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		w.Header().Set("Content-Type", "application/json")
		switch turn {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"run_command","arguments":"{\"host\":\"web-01\",\"command_id\":\"uptime\"}"}}]},"finish_reason":"tool_calls"}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"REPORT: done."},"finish_reason":"stop"}]}`))
		case 3:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"store_memory","arguments":"{\"topic\":\"nginx\",\"content\":\"check error.log first\"}"}}]},"finish_reason":"tool_calls"}]}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"none"},"finish_reason":"stop"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiagnoseReflectionStoresMemory(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/ref.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	exec := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})

	llm := NewClient(reflectLLM(t).URL+"/v1", "", "m")
	ag := New(llm, mcpSrv, st, Options{MaxSteps: 5, Timeout: 0})
	inc, _ := st.CreateIncident(context.Background(), &model.Incident{
		Host: "web-01", Title: "nginx down", Status: model.IncidentOpen,
	})
	if _, err := ag.Diagnose(context.Background(), inc); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	mems, _ := st.ListMemories(context.Background(), 10)
	if len(mems) != 1 || mems[0].Topic != "nginx" {
		t.Fatalf("expected reflection to store a memory, got %+v", mems)
	}
}

func TestDiagnoseLLMError(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/err.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	exec := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})

	// Endpoint returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	llm := NewClient(srv.URL+"/v1", "k", "m")
	ag := New(llm, mcpSrv, st, Options{MaxSteps: 2, Timeout: 0})
	inc, _ := st.CreateIncident(context.Background(), &model.Incident{
		Host: "web-01", Title: "t", Status: model.IncidentOpen,
	})
	_, err = ag.Diagnose(context.Background(), inc)
	if err == nil {
		t.Fatal("expected error from failing LLM")
	}
	d, _ := st.GetDiagnosis(context.Background(), inc.ID)
	if d == nil || d.Status != "error" {
		t.Fatalf("expected error diagnosis status, got %+v", d)
	}
	gotInc, _ := st.GetIncident(context.Background(), inc.ID)
	if gotInc == nil || gotInc.Status != model.IncidentError {
		t.Fatalf("expected incident status error, got %+v", gotInc)
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "hello"
	if got := truncateOutput(short, 100); got != short {
		t.Fatalf("expected short passthrough, got %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz"
	got := truncateOutput(long, 10)
	if len(got) > 10+len("…[truncated]") {
		t.Fatalf("truncated output too long: %q", got)
	}
	if got != "abcdefghij…[truncated]" {
		t.Fatalf("unexpected truncated output: %q", got)
	}
	if got := truncateOutput(long, 0); got != long {
		t.Fatalf("max<=0 should pass through, got %q", got)
	}
	// Multi-byte output must not be split mid-rune.
	emoji := "🎉🎉🎉🎉🎉"
	if got := truncateOutput(emoji, 3); len([]rune(got)) > 3+len("…[truncated]") {
		t.Fatalf("rune split: %q", got)
	}
}
