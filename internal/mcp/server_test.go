package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rusik69/opsagent/internal/config"
	"github.com/rusik69/opsagent/internal/correlate"
	"github.com/rusik69/opsagent/internal/gitlab"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
)

func newTestDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	al, err := sshx.NewAllowlist(sshx.DefaultAllowlist())
	if err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	runner := &sshx.FakeRunner{Outputs: map[string]string{"uptime": " 14:00:00 up 30 days,  0 users,  load average: 0.5, 0.6, 0.7"}}
	exec := sshx.NewExecutor(al, runner, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})

	// A tiny local repo fixture.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "host_vars"), 0o755); err != nil {
		t.Fatal(err)
	}
	hostVars := `---
memory_limit: 4g
role: web
`
	hostsFile := `[webservers]
web-01 ansible_host=10.0.0.1
`
	if err := os.WriteFile(filepath.Join(dir, "host_vars", "web-01.yml"), []byte(hostVars), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hosts.ini"), []byte(hostsFile), 0o644); err != nil {
		t.Fatal(err)
	}

	// A tiny docs repo fixture.
	docsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(docsDir, "nginx.md"), []byte("# nginx\nSee error.log first when troubleshooting.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rm, err := repos.NewManager([]config.RepoConfig{
		{Name: "ansible", Type: "ansible", Path: dir},
		{Name: "infra-docs", Type: "docs", Path: docsDir},
	}, "./data/repos")
	if err != nil {
		t.Fatal(err)
	}

	return Deps{Executor: exec, Repos: rm, Store: st, Resolver: resolver}, st
}

func TestCallListHosts(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	out, err := srv.CallText(context.Background(), "list_hosts", map[string]any{})
	if err != nil {
		t.Fatalf("list_hosts: %v", err)
	}
	if out == "" || !contains(out, "web-01") {
		t.Fatalf("unexpected list_hosts output: %q", out)
	}
}

func TestCallRunCommand(t *testing.T) {
	deps, st := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	out, err := srv.CallText(context.Background(), "run_command", map[string]any{
		"host": "web-01", "command_id": "uptime",
	})
	if err != nil {
		t.Fatalf("run_command: %v", err)
	}
	if !contains(out, "load average") {
		t.Fatalf("unexpected run_command output: %q", out)
	}
	runs, err := st.ListAllCommandRuns(context.Background(), 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected 1 audit row, got %d (err %v)", len(runs), err)
	}
}

func TestCallRunCommandRejectsDisallowed(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	res, err := srv.Call(context.Background(), "run_command", map[string]any{
		"host": "web-01", "command_id": "rm_rf",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error for disallowed command")
	}
}

func TestCallRunCommandInjection(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	res, err := srv.Call(context.Background(), "run_command", map[string]any{
		"host": "web-01", "command_id": "systemctl_status",
		"params": map[string]any{"service": "nginx; rm -rf /"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error for injection attempt")
	}
}

func TestCallRunCommandMissingArgsNoPanic(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cases := []map[string]any{
		{},                               // nothing at all
		{"host": "web-01"},               // missing command_id
		{"command_id": "uptime"},         // missing host
		{"host": 42, "command_id": true}, // wrong types
	}
	for i, args := range cases {
		res, err := srv.Call(context.Background(), "run_command", args)
		if err != nil {
			t.Fatalf("case %d: Call: %v", i, err)
		}
		if !res.IsError {
			t.Fatalf("case %d: expected tool error (no panic), got %q", i, textOf(res))
		}
	}
	// Non-object params must not panic; the command runs with no params.
	res, err := srv.Call(context.Background(), "run_command", map[string]any{
		"host": "web-01", "command_id": "uptime", "params": "not-an-object",
	})
	if err != nil {
		t.Fatalf("params coercion: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected params to be ignored, got error %q", textOf(res))
	}
}

func TestCallCreateMRMissingArgsNoPanic(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cases := []map[string]any{
		{},
		{"title": "t"},
		{"title": "t", "description": "d"},
		{"title": "t", "description": "d", "files": "not-an-array"},
		{"title": "t", "description": "d", "files": []any{"x"}},
	}
	for i, args := range cases {
		res, err := srv.Call(context.Background(), "create_gitlab_mr", args)
		if err != nil {
			t.Fatalf("case %d: Call: %v", i, err)
		}
		if !res.IsError {
			t.Fatalf("case %d: expected tool error (no panic), got %q", i, textOf(res))
		}
	}
}

func TestCallReadRepoFileRejectsTraversal(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	res, err := srv.Call(context.Background(), "read_repo_file", map[string]any{
		"repo": "ansible", "path": "../../etc/passwd",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected traversal attempt to be rejected")
	}
}

func TestCallGetHostConfig(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	out, err := srv.CallText(context.Background(), "get_host_config", map[string]any{"host": "web-01"})
	if err != nil {
		t.Fatalf("get_host_config: %v", err)
	}
	if !contains(out, "web-01") {
		t.Fatalf("expected host config mention, got %q", out)
	}
}

func TestStoreMemoryAndRecall(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	_, err = srv.CallText(context.Background(), "store_memory", map[string]any{
		"topic": "nginx", "content": "check error.log first", "tags": "web",
	})
	if err != nil {
		t.Fatalf("store_memory: %v", err)
	}
	out, err := srv.CallText(context.Background(), "recall_memory", map[string]any{"query": "nginx"})
	if err != nil {
		t.Fatalf("recall_memory: %v", err)
	}
	if !contains(out, "error.log") {
		t.Fatalf("expected recalled memory, got %q", out)
	}
}

func TestSearchDocs(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	out, err := srv.CallText(context.Background(), "search_docs", map[string]any{"query": "error.log"})
	if err != nil {
		t.Fatalf("search_docs: %v", err)
	}
	if !contains(out, "nginx.md") || !contains(out, "error.log") {
		t.Fatalf("expected docs match, got %q", out)
	}
}

func TestListDocs(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	out, err := srv.CallText(context.Background(), "list_docs", map[string]any{})
	if err != nil {
		t.Fatalf("list_docs: %v", err)
	}
	if !contains(out, "infra-docs") {
		t.Fatalf("expected docs repo listed, got %q", out)
	}
}

func TestCreateGitLabMR(t *testing.T) {
	// A fake GitLab API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/acme/infra/repository/branches":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"opsagent-fix-fix-nginx"}`))
		case "/projects/acme/infra/repository/files/host_vars/web-01.yml":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/projects/acme/infra/merge_requests":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"iid":7,"web_url":"https://gitlab.example.com/acme/infra/-/merge_requests/7"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	deps, st := newTestDeps(t)
	gl := gitlab.New(srv.URL, "tok")
	gl.DefaultProjectID = "acme/infra"
	gl.TargetBranch = "main"
	gl.SourceBranchPrefix = "opsagent-fix"
	deps.GitLab = gl

	srvMCP, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	inc, err := st.CreateIncident(context.Background(), &model.Incident{Host: "web-01", Title: "t", Status: model.IncidentOpen})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithIncident(context.Background(), inc.ID)
	out, err := srvMCP.CallText(ctx, "create_gitlab_mr", map[string]any{
		"title":       "fix nginx",
		"description": "bump nginx version",
		"files": []any{
			map[string]any{"path": "host_vars/web-01.yml", "content": "nginx_version: 1.25"},
		},
	})
	if err != nil {
		t.Fatalf("create_gitlab_mr: %v", err)
	}
	if !contains(out, "merge_requests/7") {
		t.Fatalf("expected MR URL, got %q", out)
	}
	// The MR link and solution should be recorded on the incident.
	got, err := st.GetIncident(context.Background(), inc.ID)
	if err != nil || got == nil {
		t.Fatalf("get incident: %v", err)
	}
	if got.MRURL == "" || !contains(got.MRURL, "merge_requests/7") {
		t.Fatalf("expected MR URL recorded on incident, got %q", got.MRURL)
	}
	if got.Solution != "bump nginx version" {
		t.Fatalf("expected solution recorded, got %q", got.Solution)
	}
}

func TestCreateGitLabMRNotConfigured(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	res, err := srv.Call(context.Background(), "create_gitlab_mr", map[string]any{
		"title": "x", "description": "y",
		"files": []any{map[string]any{"path": "a", "content": "b"}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error when gitlab not configured")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSetOutcomeAndHistory(t *testing.T) {
	deps, st := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	inc, _ := st.CreateIncident(context.Background(), &model.Incident{Host: "web-01", Title: "x", Status: model.IncidentOpen})

	_, err = srv.CallText(context.Background(), "set_incident_outcome", map[string]any{
		"incident_id": inc.ID, "root_cause": "nginx worker crash", "confidence": "medium",
	})
	if err != nil {
		t.Fatalf("set_incident_outcome: %v", err)
	}
	got, _ := st.GetIncident(context.Background(), inc.ID)
	if got.RootCause != "nginx worker crash" || got.Confidence != "medium" {
		t.Fatalf("outcome not stored: %+v", got)
	}

	out, err := srv.CallText(context.Background(), "get_incident_history", map[string]any{"id": inc.ID})
	if err != nil {
		t.Fatalf("get_incident_history: %v", err)
	}
	if !contains(out, "outcome_set") {
		t.Fatalf("expected outcome_set event, got %q", out)
	}
}

func TestCorrelateAndRelated(t *testing.T) {
	deps, st := newTestDeps(t)
	deps.Correlate = correlate.NewEngine(st, correlate.DefaultConfig())
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	a, _ := st.CreateIncident(context.Background(), &model.Incident{Host: "web-01", Title: "a", Status: model.IncidentOpen})
	b, _ := st.CreateIncident(context.Background(), &model.Incident{Host: "web-01", Title: "b", Status: model.IncidentOpen})

	out, err := srv.CallText(context.Background(), "correlate_incidents", map[string]any{
		"incident_id": a.ID, "related_incident_ids": []any{float64(b.ID)}, "reason": "host down",
	})
	if err != nil {
		t.Fatalf("correlate_incidents: %v", err)
	}
	if !contains(out, "correlated") {
		t.Fatalf("unexpected output: %q", out)
	}

	rel, err := srv.CallText(context.Background(), "get_related_incidents", map[string]any{"id": a.ID})
	if err != nil {
		t.Fatalf("get_related_incidents: %v", err)
	}
	if !contains(rel, fmt.Sprintf("#%d", b.ID)) {
		t.Fatalf("expected related incident listed, got %q", rel)
	}
}

func TestCorrelateMissingArgsNoPanic(t *testing.T) {
	deps, _ := newTestDeps(t)
	srv, err := NewServer("test", "0.0.0", deps)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	for _, args := range []map[string]any{
		{},
		{"incident_id": 1},
		{"incident_id": 1, "related_incident_ids": "nope"},
	} {
		res, err := srv.Call(context.Background(), "correlate_incidents", args)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected tool error for %v", args)
		}
	}
}
