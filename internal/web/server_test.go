package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rusik69/opsagent/internal/agent"
	"github.com/rusik69/opsagent/internal/config"
	"github.com/rusik69/opsagent/internal/correlate"
	"github.com/rusik69/opsagent/internal/gitlab"
	"github.com/rusik69/opsagent/internal/incidents"
	"github.com/rusik69/opsagent/internal/mcp"
	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/repos"
	"github.com/rusik69/opsagent/internal/sshx"
	"github.com/rusik69/opsagent/internal/store"
)

// fakeLLM returns a plain final report with no tool calls.
func fakeLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"REPORT: root cause found. Confidence: high."},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type testServer struct {
	ts       *httptest.Server
	store    *store.Store
	cfg      *config.Config
	gitlab   *gitlab.Client
	reposM   *repos.Manager
	executor *sshx.Executor
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	cfg := config.Default()
	cfg.Server.Listen = ":0"
	cfg.LLM.Enabled = true
	cfg.LLM.BaseURL = fakeLLM(t).URL + "/v1"
	cfg.LLM.Model = "test"
	cfg.LLM.MaxSteps = 2

	st, err := store.Open(t.TempDir() + "/web.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	al, err := sshx.NewAllowlist(sshx.DefaultAllowlist())
	if err != nil {
		t.Fatal(err)
	}
	runner := &sshx.FakeRunner{Outputs: map[string]string{"uptime": "load average: 1.0, 1.0, 1.0"}}
	executor := sshx.NewExecutor(al, runner, st)
	resolver := sshx.HostsFromTargets([]sshx.HostTarget{{Name: "web-01", Address: "10.0.0.1", User: "ops", Port: 22}})

	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "host_vars"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "host_vars", "web-01.yml"), []byte("nginx_port: 8080\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "hosts.ini"), []byte("[web]\nweb-01\n"), 0o644)
	docs := t.TempDir()
	_ = os.WriteFile(filepath.Join(docs, "nginx.md"), []byte("# nginx troubleshooting\n"), 0o644)
	rm, err := repos.NewManager([]config.RepoConfig{
		{Name: "ansible", Type: "ansible", Path: dir},
		{Name: "docs", Type: "docs", Path: docs},
	}, "./data/repos")
	if err != nil {
		t.Fatal(err)
	}

	gl := gitlab.New("http://127.0.0.1:1", "tok")
	corr := correlate.NewEngine(st, correlate.DefaultConfig())
	mcpSrv, err := mcp.NewServer("test", "0.0.0", mcp.Deps{
		Executor: executor, Repos: rm, Store: st, Resolver: resolver, GitLab: gl, Correlate: corr,
	})
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(agent.NewClient(cfg.LLM.BaseURL, "", cfg.LLM.Model), mcpSrv, st, agent.Options{
		MaxSteps: cfg.LLM.MaxSteps, HostConfig: rm.HostConfig,
	})
	inc := incidents.NewService(st)

	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, inc, corr)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	t.Cleanup(ts.Close)
	return &testServer{ts: ts, store: st, cfg: cfg, gitlab: gl, reposM: rm, executor: executor}
}

func (ts *testServer) createIncident(t *testing.T, host, title string) int64 {
	t.Helper()
	resp, err := http.Post(ts.ts.URL+"/api/v1/incidents",
		"application/json",
		strings.NewReader(fmt.Sprintf(`{"host":%q,"title":%q,"severity":"warning"}`, host, title)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create incident: %d", resp.StatusCode)
	}
	var inc struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inc); err != nil {
		t.Fatal(err)
	}
	return inc.ID
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestPagesRender(t *testing.T) {
	ts := newTestServer(t)
	ts.createIncident(t, "web-01", "high cpu")
	for _, path := range []string{"/", "/incidents", "/incidents/1", "/hosts", "/repos", "/memory"} {
		resp, err := http.Get(ts.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := new(strings.Builder)
		_, _ = io.Copy(body, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
		if strings.Contains(body.String(), "template") && strings.Contains(body.String(), "panic") {
			t.Errorf("%s: template error in body", path)
		}
		if body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

func TestCreateIncidentJSON(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.ts.URL+"/api/v1/incidents", "application/json",
		strings.NewReader(`{"host":"web-01","title":"t","severity":"critical","labels":{"a":"b"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var inc struct {
		ID     int64             `json:"id"`
		Status string            `json:"status"`
		Labels map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inc); err != nil {
		t.Fatal(err)
	}
	if inc.Status != "open" || inc.Labels["a"] != "b" {
		t.Fatalf("unexpected incident: %+v", inc)
	}
}

func TestCreateIncidentForm(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Post(ts.ts.URL+"/api/v1/incidents", "application/x-www-form-urlencoded",
		strings.NewReader("host=web-01&title=form+title&severity=warning"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 for form, got %d", resp.StatusCode)
	}
	incs, _ := ts.store.ListIncidents(context.Background(), "", 10)
	if len(incs) != 1 || incs[0].Title != "form title" {
		t.Fatalf("unexpected incidents: %+v", incs)
	}
}

func TestCreateIncidentValidation(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.ts.URL+"/api/v1/incidents", "application/json",
		strings.NewReader(`{"title":"no host"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing host, got %d", resp.StatusCode)
	}
}

func TestAlertmanagerIntake(t *testing.T) {
	ts := newTestServer(t)
	payload := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"HighCPU","host":"web-01","severity":"critical"}}]}`
	resp, err := http.Post(ts.ts.URL+"/api/v1/incidents/alertmanager", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Created int `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Created != 1 {
		t.Fatalf("expected 1 created, got %d", out.Created)
	}
}

func TestIncidentLifecycle(t *testing.T) {
	ts := newTestServer(t)
	id := ts.createIncident(t, "web-01", "high cpu")

	// Diagnose.
	resp, err := http.Post(ts.ts.URL+fmt.Sprintf("/api/v1/incidents/%d/diagnose", id), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("diagnose: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Wait for the diagnosis to complete (fake LLM returns immediately).
	deadline := time.Now().Add(5 * time.Second)
	for {
		dresp, err := http.Get(ts.ts.URL + fmt.Sprintf("/api/v1/incidents/%d/diagnosis", id))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Diagnosis *struct {
				Status string `json:"status"`
			} `json:"diagnosis"`
			Running bool `json:"running"`
		}
		_ = json.NewDecoder(dresp.Body).Decode(&out)
		dresp.Body.Close()
		if out.Diagnosis != nil && out.Diagnosis.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("diagnosis did not complete: %+v", out)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Status should now be diagnosed.
	inc, _ := ts.store.GetIncident(context.Background(), id)
	if inc.Status != "diagnosed" {
		t.Fatalf("expected diagnosed, got %q", inc.Status)
	}

	// Resolve via API.
	resp, err = http.Post(ts.ts.URL+fmt.Sprintf("/api/v1/incidents/%d/status", id), "application/json",
		strings.NewReader(`{"status":"resolved"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d", resp.StatusCode)
	}
	inc, _ = ts.store.GetIncident(context.Background(), id)
	if inc.Status != "resolved" {
		t.Fatalf("expected resolved, got %q", inc.Status)
	}
}

func TestIncidentNotFound(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.ts.URL + "/api/v1/incidents/99999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCommandRunAndRejection(t *testing.T) {
	ts := newTestServer(t)
	// Allowed command.
	resp, err := http.Post(ts.ts.URL+"/api/v1/hosts/web-01/commands/run", "application/json",
		strings.NewReader(`{"command_id":"uptime"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("run: %d", resp.StatusCode)
	}
	// Injection attempt.
	resp2, err := http.Post(ts.ts.URL+"/api/v1/hosts/web-01/commands/run", "application/json",
		strings.NewReader(`{"command_id":"systemctl_status","params":{"service":"nginx; rm -rf /"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for injection, got %d", resp2.StatusCode)
	}
	// Unknown host.
	resp3, err := http.Post(ts.ts.URL+"/api/v1/hosts/nope/commands/run", "application/json",
		strings.NewReader(`{"command_id":"uptime"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown host, got %d", resp3.StatusCode)
	}
}

func TestHostsCommandsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.ts.URL + "/api/v1/hosts/web-01/commands")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commands: %d", resp.StatusCode)
	}
	var cmds []sshx.Command
	if err := json.NewDecoder(resp.Body).Decode(&cmds); err != nil {
		t.Fatal(err)
	}
	if len(cmds) == 0 {
		t.Fatal("expected commands")
	}
}

func TestReposSearchEndpoints(t *testing.T) {
	ts := newTestServer(t)
	// Config search.
	resp, err := http.Get(ts.ts.URL + "/api/v1/repos/search?q=nginx_port")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d", resp.StatusCode)
	}
	// Docs scope.
	resp2, err := http.Get(ts.ts.URL + "/api/v1/repos/search?q=troubleshooting&scope=docs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var matches []repos.Match
	if err := json.NewDecoder(resp2.Body).Decode(&matches); err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("expected docs matches")
	}
	for _, m := range matches {
		if m.Repo != "docs" {
			t.Fatalf("expected docs repo only, got %q", m.Repo)
		}
	}
	// Missing q.
	resp3, err := http.Get(ts.ts.URL + "/api/v1/repos/search")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without q, got %d", resp3.StatusCode)
	}
}

func TestReposSyncEndpoint(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.ts.URL+"/api/v1/repos/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync: %d", resp.StatusCode)
	}
}

func TestMemoryAndInstructionsAPI(t *testing.T) {
	ts := newTestServer(t)
	// Memory.
	resp, err := http.Post(ts.ts.URL+"/api/v1/memory", "application/json",
		strings.NewReader(`{"topic":"nginx","content":"check error.log","tags":["web"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("memory create: %d", resp.StatusCode)
	}
	resp2, err := http.Get(ts.ts.URL + "/api/v1/memory")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var mems []struct {
		Topic string `json:"topic"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&mems); err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 || mems[0].Topic != "nginx" {
		t.Fatalf("unexpected memories: %+v", mems)
	}
	// Missing content -> 400.
	resp3, err := http.Post(ts.ts.URL+"/api/v1/memory", "application/json", strings.NewReader(`{"topic":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing memory content, got %d", resp3.StatusCode)
	}
	// Instruction.
	resp4, err := http.Post(ts.ts.URL+"/api/v1/instructions", "application/json",
		strings.NewReader(`{"content":"always check config","priority":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusCreated {
		t.Fatalf("instruction create: %d", resp4.StatusCode)
	}
}

func TestStaticAndMCPEndpoints(t *testing.T) {
	ts := newTestServer(t)
	// Static asset.
	resp, err := http.Get(ts.ts.URL + "/static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("static: %d", resp.StatusCode)
	}
	// MCP initialize over HTTP.
	mcpReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	mresp, err := http.Post(ts.ts.URL+"/mcp", "application/json", strings.NewReader(mcpReq))
	if err != nil {
		t.Fatal(err)
	}
	mresp.Body.Close()
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("mcp initialize: %d", mresp.StatusCode)
	}
}

func TestMCPDisabledByConfig(t *testing.T) {
	// Build a server with expose_http=false and verify /mcp is 404.
	base := newTestServer(t)
	cfg := base.cfg
	cfg.MCP.ExposeHTTP = false
	// Rebuild the web server with the modified config.
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	st, _ := store.Open(t.TempDir() + "/x.db")
	defer st.Close()
	executor := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("t", "0", mcp.Deps{Executor: executor, Repos: rm, Store: st, Resolver: resolver})
	ag := agent.New(agent.NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, agent.Options{})
	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, incidents.NewService(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()
	// With expose_http=false the MCP endpoint is not mounted: an MCP-style
	// POST must not be handled as JSON-RPC (405 Method Not Allowed).
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for disabled /mcp, got %d", resp.StatusCode)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Listen = ":0"
	cfg.Server.APIKey = "secret-key"
	cfg.LLM.Enabled = false

	st, _ := store.Open(t.TempDir() + "/auth.db")
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	executor := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("t", "0", mcp.Deps{Executor: executor, Repos: rm, Store: st, Resolver: resolver})
	ag := agent.New(agent.NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, agent.Options{})
	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, incidents.NewService(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	// No key -> 401.
	resp, err := http.Get(ts.URL + "/api/v1/incidents")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	// Wrong key -> 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/incidents", nil)
	req.Header.Set("X-API-Key", "wrong")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong key, got %d", resp2.StatusCode)
	}
	// Correct key -> 200.
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/incidents", nil)
	req3.Header.Set("X-API-Key", "secret-key")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with key, got %d", resp3.StatusCode)
	}
}

func TestIncidentListFiltersAPI(t *testing.T) {
	ts := newTestServer(t)
	ts.createIncident(t, "web-01", "high cpu")
	ts.createIncident(t, "db-01", "disk full")
	// Filter by severity.
	resp, err := http.Get(ts.ts.URL + "/api/v1/incidents?severity=critical")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var incs []struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&incs); err != nil {
		t.Fatal(err)
	}
	if len(incs) != 0 {
		t.Fatalf("expected 0 critical incidents, got %d", len(incs))
	}
	// Search query.
	resp2, err := http.Get(ts.ts.URL + "/api/v1/incidents?q=disk")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	_ = json.NewDecoder(resp2.Body).Decode(&incs)
	if len(incs) != 1 || incs[0].Host != "db-01" {
		t.Fatalf("expected 1 disk incident, got %+v", incs)
	}
}

func TestHTMXIncidentActions(t *testing.T) {
	ts := newTestServer(t)
	id := ts.createIncident(t, "web-01", "t")
	// Diagnose with HX-Request should return the incident partial HTML.
	req, _ := http.NewRequest(http.MethodPost, ts.ts.URL+fmt.Sprintf("/api/v1/incidents/%d/diagnose", id), nil)
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("htmx diagnose: %d", resp.StatusCode)
	}
	body := new(strings.Builder)
	_, _ = io.Copy(body, resp.Body)
	if !strings.Contains(body.String(), "incident-root") {
		t.Fatalf("expected incident partial, got: %s", body.String())
	}
	// Resolve via HTMX form.
	req2, _ := http.NewRequest(http.MethodPost, ts.ts.URL+fmt.Sprintf("/api/v1/incidents/%d/status", id),
		strings.NewReader("status=resolved"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("HX-Request", "true")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("htmx resolve: %d", resp2.StatusCode)
	}
	inc, _ := ts.store.GetIncident(context.Background(), id)
	if inc.Status != "resolved" {
		t.Fatalf("expected resolved, got %q", inc.Status)
	}
}

func TestCorrelationAndHistoryEndpoints(t *testing.T) {
	ts := newTestServer(t)
	a := ts.createIncident(t, "web-01", "cpu")
	b := ts.createIncident(t, "web-01", "mem")

	// Related (no groups yet).
	resp, err := http.Get(ts.ts.URL + fmt.Sprintf("/api/v1/incidents/%d/related", a))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("related: %d", resp.StatusCode)
	}

	// Events.
	resp, err = http.Get(ts.ts.URL + fmt.Sprintf("/api/v1/incidents/%d/events", a))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Kind != "created" {
		t.Fatalf("expected created event, got %+v", events)
	}

	// Correlate run + groups list.
	resp, err = http.Post(ts.ts.URL+"/api/v1/correlate/run", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correlate run: %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.ts.URL + "/api/v1/groups")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var groups []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("expected at least one correlation group")
	}

	// Related should now include b.
	resp, err = http.Get(ts.ts.URL + fmt.Sprintf("/api/v1/incidents/%d/related", a))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rel struct {
		Incidents []model.Incident `json:"incidents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		t.Fatal(err)
	}
	if len(rel.Incidents) != 1 || rel.Incidents[0].ID != b {
		t.Fatalf("expected a related to b, got %+v", rel.Incidents)
	}
}

func TestReviewEndpoints(t *testing.T) {
	ts := newTestServer(t)
	// Without a configured reviewer, review/run returns 400.
	resp, err := http.Post(ts.ts.URL+"/api/v1/review/run", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without reviewer, got %d", resp.StatusCode)
	}
	// Retrospectives list works and is empty.
	resp, err = http.Get(ts.ts.URL + "/api/v1/retrospectives")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrospectives: %d", resp.StatusCode)
	}
}

func TestHistoryPageRenders(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history page: %d", resp.StatusCode)
	}
}

func TestGroupsPageRenders(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.ts.URL + "/groups")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("groups page: %d", resp.StatusCode)
	}
}

func TestHealthzExemptFromAPIKey(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Listen = ":0"
	cfg.Server.APIKey = "secret-key"
	cfg.LLM.Enabled = false

	st, _ := store.Open(t.TempDir() + "/auth.db")
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	executor := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("t", "0", mcp.Deps{Executor: executor, Repos: rm, Store: st, Resolver: resolver})
	ag := agent.New(agent.NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, agent.Options{})
	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, incidents.NewService(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	// Liveness probe must work without credentials even when an API key is set.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for healthz without key, got %d", resp.StatusCode)
	}
	// The protected API still requires the key.
	resp2, err := http.Get(ts.URL + "/api/v1/incidents")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for API without key, got %d", resp2.StatusCode)
	}
}

func TestDiagnosisConcurrencyLimit(t *testing.T) {
	// A blocking LLM holds the single diagnosis slot open until released.
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseNow()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer llm.Close()

	cfg := config.Default()
	cfg.Server.Listen = ":0"
	cfg.LLM.Enabled = true
	cfg.LLM.BaseURL = llm.URL + "/v1"
	cfg.LLM.Model = "test"
	cfg.LLM.MaxSteps = 1
	cfg.Agent.MaxConcurrent = 1

	st, _ := store.Open(t.TempDir() + "/diag.db")
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	executor := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("t", "0", mcp.Deps{Executor: executor, Repos: rm, Store: st, Resolver: resolver})
	ag := agent.New(agent.NewClient(cfg.LLM.BaseURL, "", cfg.LLM.Model), mcpSrv, st, agent.Options{MaxSteps: 1})
	inc := incidents.NewService(st)
	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, inc, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	create := func(host string) int64 {
		resp, err := http.Post(ts.URL+"/api/v1/incidents", "application/json",
			strings.NewReader(fmt.Sprintf(`{"host":%q,"title":"cpu","severity":"warning"}`, host)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			ID int64 `json:"id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.ID
	}

	id1 := create("web-01")
	id2 := create("web-02")

	// First diagnosis occupies the single slot.
	r1, err := http.Post(ts.URL+fmt.Sprintf("/api/v1/incidents/%d/diagnose", id1), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusAccepted {
		t.Fatalf("first diagnose: %d", r1.StatusCode)
	}
	// Wait until the slot is actually taken.
	time.Sleep(50 * time.Millisecond)

	// Second diagnosis must be rejected with 429.
	r2, err := http.Post(ts.URL+fmt.Sprintf("/api/v1/incidents/%d/diagnose", id2), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for second diagnose, got %d", r2.StatusCode)
	}

	// Release the slot; a retry now succeeds.
	releaseNow()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r3, err := http.Post(ts.URL+fmt.Sprintf("/api/v1/incidents/%d/diagnose", id2), "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		r3.Body.Close()
		if r3.StatusCode == http.StatusAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("diagnosis did not become available again: %d", r3.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAPIKeyScopes(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Listen = ":0"
	cfg.Server.APIKeyReadOnly = "ro-key"
	cfg.Server.APIKeyWebhook = "hook-key"
	cfg.LLM.Enabled = false

	st, _ := store.Open(t.TempDir() + "/scopes.db")
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	executor := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("t", "0", mcp.Deps{Executor: executor, Repos: rm, Store: st, Resolver: resolver})
	ag := agent.New(agent.NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, agent.Options{})
	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, incidents.NewService(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	get := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/incidents", nil)
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	post := func(key string, path string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(`{"host":"web-01","title":"t"}`))
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Read-only key: GET allowed, POST rejected.
	if got := get("ro-key"); got != http.StatusOK {
		t.Fatalf("read-only key GET: %d", got)
	}
	if got := post("ro-key", "/api/v1/incidents"); got != http.StatusUnauthorized {
		t.Fatalf("read-only key POST: %d", got)
	}
	// Webhook key: intake POST allowed, diagnose POST rejected.
	if got := post("hook-key", "/api/v1/incidents"); got != http.StatusCreated {
		t.Fatalf("webhook key intake: %d", got)
	}
	if got := post("hook-key", "/api/v1/incidents/1/diagnose"); got != http.StatusUnauthorized {
		t.Fatalf("webhook key diagnose: %d", got)
	}
	// Webhook key cannot read either.
	if got := get("hook-key"); got != http.StatusUnauthorized {
		t.Fatalf("webhook key GET: %d", got)
	}
}

func TestApplyInstructionAPI(t *testing.T) {
	ts := newTestServer(t)
	// Create an instruction via the API.
	body := strings.NewReader(`{"content":"always check dmesg","priority":2}`)
	resp, err := http.Post(ts.ts.URL+"/api/v1/instructions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create instruction: %d", resp.StatusCode)
	}
	var ins struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ins)

	// Mark it applied.
	resp2, err := http.Post(ts.ts.URL+fmt.Sprintf("/api/v1/instructions/%d/apply", ins.ID), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("apply instruction: %d", resp2.StatusCode)
	}
	items, err := ts.store.ListInstructions(context.Background(), 10)
	if err != nil || len(items) != 1 || !items[0].Applied {
		t.Fatalf("expected instruction marked applied, got %+v (%v)", items, err)
	}
}

func TestNoteDeleteAndBulk(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	a := ts.createIncident(t, "web-01", "cpu")
	b := ts.createIncident(t, "web-02", "mem")

	// Note.
	resp, err := http.Post(ts.ts.URL+fmt.Sprintf("/api/v1/incidents/%d/note", a), "application/json",
		strings.NewReader(`{"note":"oncall investigating"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("note: %d", resp.StatusCode)
	}
	events, _ := ts.store.ListEvents(ctx, a)
	found := false
	for _, e := range events {
		if e.Kind == model.EventNote && e.Detail == "oncall investigating" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected note event, got %+v", events)
	}

	// Bulk status.
	req, _ := http.NewRequest(http.MethodPost, ts.ts.URL+"/api/v1/incidents/bulk/status",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d],"status":"resolved"}`, a, b)))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("bulk status: %d", resp2.StatusCode)
	}
	for _, id := range []int64{a, b} {
		inc, _ := ts.store.GetIncident(ctx, id)
		if inc.Status != model.IncidentResolved {
			t.Fatalf("expected %d resolved, got %q", id, inc.Status)
		}
	}

	// Delete (soft) + bulk delete.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.ts.URL+fmt.Sprintf("/api/v1/incidents/%d", a), nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", delResp.StatusCode)
	}
	inc, _ := ts.store.GetIncident(ctx, a)
	if inc.Status != model.IncidentDeleted {
		t.Fatalf("expected soft-deleted, got %q", inc.Status)
	}
}

func TestStatsEndpoint(t *testing.T) {
	ts := newTestServer(t)
	ts.createIncident(t, "web-01", "cpu")
	ts.createIncident(t, "web-02", "mem")
	resp, err := http.Get(ts.ts.URL + "/api/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats: %d", resp.StatusCode)
	}
	var st struct {
		Total int64 `json:"total"`
		Open  int64 `json:"open"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Open != 2 {
		t.Fatalf("unexpected stats: %+v", st)
	}
}

func TestWebhookHMAC(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Listen = ":0"
	cfg.Server.WebhookHMACSecret = "s3cret"
	cfg.LLM.Enabled = false

	st, _ := store.Open(t.TempDir() + "/hmac.db")
	defer st.Close()
	al, _ := sshx.NewAllowlist(sshx.DefaultAllowlist())
	executor := sshx.NewExecutor(al, &sshx.FakeRunner{}, st)
	resolver := sshx.HostsFromTargets(nil)
	rm, _ := repos.NewManager(nil, t.TempDir())
	mcpSrv, _ := mcp.NewServer("t", "0", mcp.Deps{Executor: executor, Repos: rm, Store: st, Resolver: resolver})
	ag := agent.New(agent.NewClient("http://127.0.0.1:1/v1", "", "m"), mcpSrv, st, agent.Options{})
	ws, err := New(cfg, st, executor, resolver, rm, mcpSrv, ag, incidents.NewService(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(ws.Handler())
	defer ts.Close()

	body := `{"host":"web-01","title":"t","severity":"warning"}`
	sign := func(key string, payload []byte) string {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(payload)
		return hex.EncodeToString(mac.Sum(nil))
	}

	// Missing signature -> 401.
	resp, err := http.Post(ts.URL+"/api/v1/incidents", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without signature, got %d", resp.StatusCode)
	}

	// Correct signature -> 201.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/incidents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", sign("s3cret", []byte(body)))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 with signature, got %d", resp2.StatusCode)
	}
}

func TestCSRFOriginCheck(t *testing.T) {
	ts := newTestServer(t)
	// Same-origin JSON POST must be accepted.
	req, _ := http.NewRequest(http.MethodPost, ts.ts.URL+"/api/v1/incidents",
		strings.NewReader(`{"host":"web-01","title":"cpu","severity":"warning"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin POST: %d", resp.StatusCode)
	}
	// Cross-origin POST must be rejected.
	req2, _ := http.NewRequest(http.MethodPost, ts.ts.URL+"/api/v1/incidents",
		strings.NewReader(`host=web-01&title=cpu&severity=warning`))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Origin", "http://evil.example.com")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", resp2.StatusCode)
	}
	// No Origin (curl/API) is unaffected.
	resp3, err := http.Post(ts.ts.URL+"/api/v1/incidents", "application/json",
		strings.NewReader(`{"host":"web-01","title":"cpu","severity":"warning"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusCreated {
		t.Fatalf("no-origin POST: %d", resp3.StatusCode)
	}
}

func TestMetricsAndReadyz(t *testing.T) {
	ts := newTestServer(t)
	ts.createIncident(t, "web-01", "cpu")

	// Readiness.
	resp, err := http.Get(ts.ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz: %d", resp.StatusCode)
	}

	// Metrics (no API key configured in the test server, so it is open).
	resp2, err := http.Get(ts.ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body), "opsagent_incidents{status=\"open\"} 1") {
		t.Fatalf("metrics: %d %s", resp2.StatusCode, body)
	}
}

func TestIncidentPagination(t *testing.T) {
	ts := newTestServer(t)
	for i := 0; i < 5; i++ {
		ts.createIncident(t, "web-01", fmt.Sprintf("incident %d", i))
	}
	resp, err := http.Get(ts.ts.URL + "/api/v1/incidents?limit=2&offset=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Total-Count"); got != "5" {
		t.Fatalf("expected X-Total-Count 5, got %q", got)
	}
	var incs []struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&incs)
	if len(incs) != 2 {
		t.Fatalf("expected 2 incidents on page 1, got %d", len(incs))
	}
	// Second page must not overlap.
	resp2, err := http.Get(ts.ts.URL + "/api/v1/incidents?limit=2&offset=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var incs2 []struct {
		ID int64 `json:"id"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&incs2)
	if len(incs2) != 2 || incs2[0].ID == incs[0].ID {
		t.Fatalf("expected disjoint page 2, got %+v", incs2)
	}
}
