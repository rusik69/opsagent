package store

import (
	"context"
	"testing"
	"time"

	"github.com/rusik69/opsagent/internal/model"
)

func TestIncidentOutcomeColumns(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc := seedIncident(t, s, "web-01", "x", "warning")

	if err := s.UpdateIncidentOutcome(ctx, inc.ID, "nginx worker crash", "medium", ""); err != nil {
		t.Fatalf("update outcome: %v", err)
	}
	got, _ := s.GetIncident(ctx, inc.ID)
	if got.RootCause != "nginx worker crash" || got.Confidence != "medium" {
		t.Fatalf("outcome not persisted: %+v", got)
	}

	if err := s.MarkResolved(ctx, inc.ID, "mr_merged"); err != nil {
		t.Fatalf("mark resolved: %v", err)
	}
	got, _ = s.GetIncident(ctx, inc.ID)
	if got.Status != model.IncidentResolved || got.ResolvedVia != "mr_merged" || got.ResolvedAt == nil {
		t.Fatalf("resolve not persisted: %+v", got)
	}
}

func TestEvents(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc := seedIncident(t, s, "web-01", "x", "warning")

	for _, k := range []string{model.EventCreated, model.EventDiagnosisStart, model.EventDiagnosed, model.EventMRCreated} {
		if _, err := s.AddEvent(ctx, inc.ID, k, "detail-"+k); err != nil {
			t.Fatalf("add event %s: %v", k, err)
		}
	}
	events, err := s.ListEvents(ctx, inc.ID)
	if err != nil || len(events) != 4 {
		t.Fatalf("list events: %v (n=%d)", err, len(events))
	}
	if events[0].Kind != model.EventCreated || events[3].Kind != model.EventMRCreated {
		t.Fatalf("unexpected event order: %+v", events)
	}
	if events[2].Detail != "detail-diagnosed" {
		t.Fatalf("event detail not persisted: %+v", events[2])
	}
}

func TestGroups(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	a := seedIncident(t, s, "web-01", "cpu", "critical")
	b := seedIncident(t, s, "web-01", "mem", "warning")
	c := seedIncident(t, s, "db-01", "cpu", "critical")

	g, err := s.GetOrCreateGroup(ctx, model.GroupHost, "host:web-01:2026010112", "web-01 burst")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := s.AddIncidentToGroup(ctx, g.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddIncidentToGroup(ctx, g.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if added, _ := s.AddIncidentToGroup(ctx, g.ID, a.ID); added {
		t.Fatal("expected duplicate membership to be ignored")
	}
	// Same (kind,key) returns the existing group.
	g2, err := s.GetOrCreateGroup(ctx, model.GroupHost, "host:web-01:2026010112", "web-01 burst")
	if err != nil || g2.ID != g.ID {
		t.Fatalf("expected existing group, got %+v (%v)", g2, err)
	}

	related, err := s.RelatedIncidentIDs(ctx, a.ID)
	if err != nil || len(related) != 1 || related[0] != b.ID {
		t.Fatalf("related incidents: %v %v", err, related)
	}
	groups, err := s.GroupByIncident(ctx, a.ID)
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups by incident: %v %v", err, groups)
	}
	members, err := s.GroupMemberIDs(ctx, g.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("member ids: %v %v", err, members)
	}
	_ = c

	all, err := s.ListGroups(ctx, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("list groups: %v %v", err, all)
	}
}

func TestRetrospectives(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r, err := s.CreateRetrospective(ctx, &model.Retrospective{
		WindowStart: now.Add(-24 * time.Hour), WindowEnd: now,
		IncidentsReviewd: 5, Summary: "recurring nginx", MemoriesCreated: 2, InstructionsCreated: 1,
	})
	if err != nil {
		t.Fatalf("create retrospective: %v", err)
	}
	list, err := s.ListRetrospectives(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list retrospectives: %v %v", err, list)
	}
	if list[0].Summary != "recurring nginx" || list[0].IncidentsReviewd != 5 || r.ID == 0 {
		t.Fatalf("retrospective roundtrip: %+v", list[0])
	}
}

func TestFindResolvedIncidentByExternal(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	inc, _ := s.CreateIncident(ctx, &model.Incident{
		Source: "alertmanager", ExternalID: "HighCPU:web-01", Host: "web-01", Title: "cpu", Status: model.IncidentResolved,
	})
	got, err := s.FindResolvedIncidentByExternal(ctx, "alertmanager", "HighCPU:web-01", "web-01")
	if err != nil || got == nil || got.ID != inc.ID {
		t.Fatalf("find resolved: %v %+v", err, got)
	}
	missing, err := s.FindResolvedIncidentByExternal(ctx, "alertmanager", "Nope", "web-01")
	if err != nil || missing != nil {
		t.Fatalf("expected nil for missing: %v %+v", err, missing)
	}
}
