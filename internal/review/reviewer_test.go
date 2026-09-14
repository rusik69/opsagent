package review

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rusik69/opsagent/internal/agent"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
)

// reviewLLM first returns two tool calls (store_memory + store_instruction),
// then a final summary.
func reviewLLM(t *testing.T) *httptest.Server {
	t.Helper()
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		w.Header().Set("Content-Type", "application/json")
		switch turn {
		case 1:
			body := `{"choices":[{"message":{"role":"assistant","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"store_memory","arguments":"{\"topic\":\"nginx\",\"content\":\"check error.log first\"}"}},` +
				`{"id":"c2","type":"function","function":{"name":"store_instruction","arguments":"{\"content\":\"always verify config\",\"priority\":2}"}}` +
				`]},"finish_reason":"tool_calls"}]}`
			_, _ = w.Write([]byte(body))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"SUMMARY: recurring nginx issues; verify config first."},"finish_reason":"stop"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupReviewer(t *testing.T) (*Reviewer, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/review.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	exec := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("test", "0.0.0", mcp.Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver})
	llm := agent.NewClient(reviewLLM(t).URL+"/v1", "", "m")
	rv := New(st, llm, mcpSrv, Config{Limit: 10, MinIncidents: 2, MaxSummaryLen: 2000})
	return rv, st
}

func seedCompleted(t *testing.T, s *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		inc, err := s.CreateIncident(ctx, &model.Incident{
			Host: "web-01", Title: "nginx down", Status: model.IncidentDiagnosed, RootCause: "config error",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = s.CreateDiagnosis(ctx, &model.Diagnosis{IncidentID: inc.ID, Status: "done", Summary: "worker_processes misconfigured"})
		_, _ = s.AddEvent(ctx, inc.ID, model.EventDiagnosed, "worker_processes misconfigured")
	}
}

func TestReviewRunGeneratesLearning(t *testing.T) {
	rv, st := setupReviewer(t)
	seedCompleted(t, st, 3)

	retro, err := rv.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if retro == nil {
		t.Fatal("expected a retrospective")
	}
	if retro.IncidentsReviewd != 3 {
		t.Fatalf("expected 3 incidents reviewed, got %d", retro.IncidentsReviewd)
	}
	if retro.MemoriesCreated != 1 || retro.InstructionsCreated != 1 {
		t.Fatalf("expected 1 memory + 1 instruction, got %d/%d", retro.MemoriesCreated, retro.InstructionsCreated)
	}
	if !strings.Contains(retro.Summary, "SUMMARY") {
		t.Fatalf("expected summary, got %q", retro.Summary)
	}

	// The memory and instruction should now be in the store.
	mems, _ := st.ListMemories(context.Background(), 10)
	if len(mems) != 1 || mems[0].Topic != "nginx" {
		t.Fatalf("expected stored memory, got %+v", mems)
	}
	instr, _ := st.ListInstructions(context.Background(), 10)
	if len(instr) != 1 || instr[0].Content != "always verify config" {
		t.Fatalf("expected stored instruction, got %+v", instr)
	}
}

func TestReviewSkipsWithoutEnoughIncidents(t *testing.T) {
	rv, st := setupReviewer(t)
	seedCompleted(t, st, 1) // below MinIncidents=2
	retro, err := rv.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if retro != nil {
		t.Fatal("expected no retrospective below min incidents")
	}
}

func TestReviewDedups(t *testing.T) {
	rv, st := setupReviewer(t)
	seedCompleted(t, st, 3)
	ctx := context.Background()
	if _, err := rv.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Second run: the memory/instruction already exist -> nothing new stored.
	retro, err := rv.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retro == nil {
		t.Fatal("expected retrospective on second run")
	}
	if retro.MemoriesCreated != 0 || retro.InstructionsCreated != 0 {
		t.Fatalf("expected dedup on second run, got %d/%d", retro.MemoriesCreated, retro.InstructionsCreated)
	}
}
