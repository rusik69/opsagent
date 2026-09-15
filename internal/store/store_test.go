package store

import (
	"context"
	"testing"

	"github.com/rusik69/opsagent/internal/model"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/store.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedIncident(t *testing.T, s *Store, host, title, sev string) *model.Incident {
	t.Helper()
	inc, err := s.CreateIncident(context.Background(), &model.Incident{
		Host: host, Title: title, Severity: model.Severity(sev), Status: model.IncidentOpen,
		Labels: map[string]string{"cluster": "prod"},
	})
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	return inc
}

func TestIncidentCRUD(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc := seedIncident(t, s, "web-01", "high cpu", "critical")

	got, err := s.GetIncident(ctx, inc.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Host != "web-01" || got.Severity != model.SeverityCritical || got.Status != model.IncidentOpen {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Labels["cluster"] != "prod" {
		t.Fatalf("labels not preserved: %+v", got.Labels)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("expected created_at set")
	}
}

func TestIncidentSolutionUpdate(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc := seedIncident(t, s, "db-01", "disk full", "warning")
	if err := s.UpdateIncidentSolution(ctx, inc.ID, "clean old logs", "https://gitlab/x/-/mr/1"); err != nil {
		t.Fatalf("update solution: %v", err)
	}
	got, _ := s.GetIncident(ctx, inc.ID)
	if got.Solution != "clean old logs" || got.MRURL != "https://gitlab/x/-/mr/1" {
		t.Fatalf("solution/mr not updated: %+v", got)
	}
}

func TestListIncidentsFiltered(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	seedIncident(t, s, "web-01", "high cpu", "critical")
	seedIncident(t, s, "db-01", "disk full", "warning")
	if err := s.UpdateIncidentStatus(ctx, 1, model.IncidentResolved); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		status   string
		severity string
		query    string
		want     int
	}{
		{"all", "", "", "", 2},
		{"by status", "resolved", "", "", 1},
		{"by severity", "", "warning", "", 1},
		{"by host query", "", "", "web-01", 1},
		{"by title query", "", "", "disk", 1},
		{"by id query", "", "", "99999", 0},
		{"no match", "", "", "zzz", 0},
	}
	for _, c := range cases {
		got, err := s.ListIncidentsFiltered(ctx, c.status, c.severity, c.query, 10)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Errorf("%s: expected %d, got %d", c.name, c.want, len(got))
		}
	}
}

func TestDiagnosisCRUD(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc := seedIncident(t, s, "web-01", "x", "warning")

	d, err := s.CreateDiagnosis(ctx, &model.Diagnosis{IncidentID: inc.ID, Status: "running"})
	if err != nil {
		t.Fatalf("create diagnosis: %v", err)
	}
	d.Steps = append(d.Steps, model.DiagnosisStep{Step: 1, Tool: "uptime", Input: "{}", Output: "load 0.5"})
	d.Status = "done"
	d.Report = "report text"
	if err := s.UpdateDiagnosis(ctx, d); err != nil {
		t.Fatalf("update diagnosis: %v", err)
	}
	got, err := s.GetDiagnosis(ctx, inc.ID)
	if err != nil || got == nil {
		t.Fatalf("get diagnosis: %v", err)
	}
	if got.Status != "done" || got.Report != "report text" {
		t.Fatalf("diagnosis mismatch: %+v", got)
	}
	if len(got.Steps) != 1 || got.Steps[0].Tool != "uptime" {
		t.Fatalf("steps not persisted: %+v", got.Steps)
	}
	missing, _ := s.GetDiagnosis(ctx, 99999)
	if missing != nil {
		t.Fatal("expected nil diagnosis for unknown incident")
	}
}

func TestCommandRunCRUD(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc := seedIncident(t, s, "web-01", "x", "warning")

	run, err := s.CreateCommandRun(ctx, &model.CommandRun{
		IncidentID: &inc.ID, Host: "web-01", CommandID: "uptime", Command: "uptime", Status: "running",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run.Status = "success"
	run.Stdout = "load avg 0.5"
	run.DurationMS = 42
	if err := s.UpdateCommandRun(ctx, run); err != nil {
		t.Fatalf("update run: %v", err)
	}

	byIncident, err := s.ListCommandRuns(ctx, inc.ID)
	if err != nil || len(byIncident) != 1 {
		t.Fatalf("list by incident: %v (n=%d)", err, len(byIncident))
	}
	if byIncident[0].Status != "success" || byIncident[0].DurationMS != 42 || byIncident[0].IncidentID == nil || *byIncident[0].IncidentID != inc.ID {
		t.Fatalf("run mismatch: %+v", byIncident[0])
	}
	all, err := s.ListAllCommandRuns(ctx, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("list all: %v (n=%d)", err, len(all))
	}
}

func TestMemoryAndInstructions(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	m, err := s.CreateMemory(ctx, &model.Memory{Topic: "nginx", Content: "check error.log", Tags: []string{"web"}})
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	if m.ID == 0 {
		t.Fatal("expected memory id")
	}
	bySearch, err := s.SearchMemories(ctx, "error.log", 5)
	if err != nil || len(bySearch) != 1 {
		t.Fatalf("search memories: %v (n=%d)", err, len(bySearch))
	}
	all, err := s.ListMemories(ctx, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("list memories: %v (n=%d)", err, len(all))
	}

	i, err := s.CreateInstruction(ctx, &model.Instruction{Content: "always check config", Priority: 1})
	if err != nil {
		t.Fatalf("create instruction: %v", err)
	}
	if err := s.MarkInstructionApplied(ctx, i.ID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	list, err := s.ListInstructions(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list instructions: %v", err)
	}
	if !list[0].Applied {
		t.Fatal("expected instruction marked applied")
	}
}

func TestMigrateExistingDBAddsColumns(t *testing.T) {
	// Simulate an old DB without the solution/mr_url columns.
	dir := t.TempDir()
	path := dir + "/old.db"
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Create schema without new columns by using the current schema, then
	// drop the columns to simulate an older version.
	_, err = old.db.Exec(`ALTER TABLE incidents DROP COLUMN solution; ALTER TABLE incidents DROP COLUMN mr_url;`)
	if err != nil {
		t.Fatalf("setup old schema: %v", err)
	}
	old.Close()

	// Reopen: migration should add the columns back.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	inc := seedIncident(t, s, "web-01", "x", "warning")
	if err := s.UpdateIncidentSolution(context.Background(), inc.ID, "sol", "url"); err != nil {
		t.Fatalf("update solution after migration: %v", err)
	}
	got, _ := s.GetIncident(context.Background(), inc.ID)
	if got.Solution != "sol" || got.MRURL != "url" {
		t.Fatalf("columns not usable after migration: %+v", got)
	}
}

func TestMemoryFTSAndLIKE(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.CreateMemory(ctx, &model.Memory{Topic: "nginx", Content: "check error.log first when nginx returns 502", Tags: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMemory(ctx, &model.Memory{Topic: "postgres", Content: "connection pool exhaustion causes latency spikes", Tags: []string{"db"}}); err != nil {
		t.Fatal(err)
	}

	items, err := s.SearchMemories(ctx, "nginx 502", 10)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(items) != 1 || items[0].Topic != "nginx" {
		t.Fatalf("expected nginx memory, got %+v", items)
	}

	items, err = s.SearchMemories(ctx, "pool", 10)
	if err != nil {
		t.Fatalf("SearchMemories pool: %v", err)
	}
	if len(items) != 1 || items[0].Topic != "postgres" {
		t.Fatalf("expected postgres memory, got %+v", items)
	}

	// A query with no alphanumeric tokens falls back to the LIKE path.
	items, err = s.SearchMemories(ctx, "!!!", 10)
	if err != nil {
		t.Fatalf("SearchMemories fallback: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no matches, got %+v", items)
	}

	// Dedup helper.
	dup, err := s.MemoryContentExists(ctx, "check error.log first when nginx returns 502")
	if err != nil || !dup {
		t.Fatalf("expected duplicate detection (dup=%v err=%v)", dup, err)
	}
}
