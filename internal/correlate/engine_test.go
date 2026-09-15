package correlate

import (
	"context"
	"testing"
	"time"

	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/corr.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newIncident(t *testing.T, s *store.Store, host, title, alertname string, labels map[string]string, rootCause string) *model.Incident {
	t.Helper()
	if labels == nil {
		labels = map[string]string{}
	}
	if alertname != "" {
		labels["alertname"] = alertname
	}
	inc, err := s.CreateIncident(context.Background(), &model.Incident{
		Host: host, Title: title, Status: model.IncidentOpen,
		Labels: labels, RootCause: rootCause,
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestEngineCorrelatesByHostAndAlert(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	inc1 := newIncident(t, s, "web-01", "cpu", "HighCPU", map[string]string{"cluster": "prod"}, "")
	inc2 := newIncident(t, s, "web-01", "mem", "HighMem", map[string]string{"cluster": "prod"}, "")
	inc3 := newIncident(t, s, "db-01", "cpu", "HighCPU", map[string]string{"cluster": "prod"}, "")

	e := NewEngine(s, DefaultConfig())
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// inc1 and inc2 share host web-01 -> host group of 2.
	hostGroup, err := s.GroupByIncident(ctx, inc1.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundHost := false
	for _, g := range hostGroup {
		if g.Kind == model.GroupHost {
			foundHost = true
			members, _ := s.GroupMemberIDs(ctx, g.ID)
			if len(members) != 2 {
				t.Fatalf("host group expected 2 members, got %v", members)
			}
		}
	}
	if !foundHost {
		t.Fatal("expected host correlation group")
	}

	// inc1 and inc3 share alertname HighCPU -> alert group across hosts.
	related, err := s.RelatedIncidentIDs(ctx, inc1.ID)
	if err != nil {
		t.Fatal(err)
	}
	have := func(id int64) bool {
		for _, r := range related {
			if r == id {
				return true
			}
		}
		return false
	}
	if !have(inc2.ID) || !have(inc3.ID) {
		t.Fatalf("expected inc1 related to inc2 and inc3, got %v", related)
	}
}

func TestEngineWindowExcludesOldIncidents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	e := NewEngine(s, Config{Window: time.Hour, Methods: []string{model.GroupHost}})

	// An incident created outside the window.
	oldTime := time.Now().UTC().Add(-48 * time.Hour)
	old, err := s.CreateIncident(ctx, &model.Incident{
		Host: "web-01", Title: "old", Status: model.IncidentOpen, CreatedAt: oldTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = old
	cur := newIncident(t, s, "web-01", "new", "", map[string]string{}, "")

	if err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
	related, _ := s.RelatedIncidentIDs(ctx, cur.ID)
	if len(related) != 0 {
		t.Fatalf("expected no correlation with out-of-window incident, got %v", related)
	}
}

func TestLinkByAgent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := newIncident(t, s, "web-01", "a", "", map[string]string{}, "host down")
	b := newIncident(t, s, "web-01", "b", "", map[string]string{}, "")
	c := newIncident(t, s, "db-01", "c", "", map[string]string{}, "")

	e := NewEngine(s, DefaultConfig())
	g, err := e.LinkByAgent(ctx, a.ID, []int64{b.ID, c.ID}, "host down explains everything")
	if err != nil {
		t.Fatalf("LinkByAgent: %v", err)
	}
	if g.Kind != model.GroupAgent {
		t.Fatalf("expected agent group kind, got %s", g.Kind)
	}
	members, _ := s.GroupMemberIDs(ctx, g.ID)
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %v", members)
	}
	// Events should have been emitted for the members.
	has, _ := s.HasEvent(ctx, b.ID, model.EventCorrelated)
	if !has {
		t.Fatal("expected correlated event on b")
	}
}

func TestCorrelateIncidentSingle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := newIncident(t, s, "web-01", "a", "HighCPU", map[string]string{}, "")
	b := newIncident(t, s, "web-01", "b", "", map[string]string{}, "")

	e := NewEngine(s, DefaultConfig())
	if err := e.CorrelateIncident(ctx, a.ID); err != nil {
		t.Fatalf("CorrelateIncident: %v", err)
	}
	related, _ := s.RelatedIncidentIDs(ctx, a.ID)
	if len(related) != 1 || related[0] != b.ID {
		t.Fatalf("expected a correlated with b, got %v", related)
	}
}

func TestAutoCorrelationEmitsEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := newIncident(t, s, "web-01", "a", "", map[string]string{}, "")
	b := newIncident(t, s, "web-01", "b", "", map[string]string{}, "")

	e := NewEngine(s, Config{Window: 2 * time.Hour, Methods: []string{model.GroupHost}})
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	hasA, _ := s.HasEvent(ctx, a.ID, model.EventCorrelated)
	hasB, _ := s.HasEvent(ctx, b.ID, model.EventCorrelated)
	if !hasA || !hasB {
		t.Fatalf("expected correlated events on both incidents (a=%v b=%v)", hasA, hasB)
	}
}
