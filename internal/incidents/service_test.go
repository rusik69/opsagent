package incidents

import (
	"context"
	"testing"

	"github.com/rusik69/opsagent/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateGeneric(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st)
	inc, err := svc.CreateGeneric(context.Background(), GenericIncident{
		Host: "web-01", Title: "high load", Severity: "critical", Message: "load 40",
		Labels: map[string]string{"cluster": "prod"},
	})
	if err != nil {
		t.Fatalf("CreateGeneric: %v", err)
	}
	if inc.ID == 0 || inc.Host != "web-01" || inc.Status != "open" {
		t.Fatalf("unexpected incident: %+v", inc)
	}
}

func TestCreateGenericMissingHost(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st)
	if _, err := svc.CreateGeneric(context.Background(), GenericIncident{Title: "x"}); err == nil {
		t.Fatal("expected error for missing host")
	}
}

func TestRecurrenceDetection(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st)
	ctx := context.Background()

	// First occurrence.
	first, err := svc.CreateGeneric(ctx, GenericIncident{
		Host: "web-01", Title: "high cpu", Source: "alertmanager", ExternalID: "HighCPU:web-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkResolved(ctx, first.ID, "mr_merged"); err != nil {
		t.Fatal(err)
	}

	// Same alert re-fires -> recurrence event.
	second, err := svc.CreateGeneric(ctx, GenericIncident{
		Host: "web-01", Title: "high cpu", Source: "alertmanager", ExternalID: "HighCPU:web-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	has, _ := st.HasEvent(ctx, second.ID, "recurrence")
	if !has {
		t.Fatal("expected recurrence event on re-fired incident")
	}
	// A different external_id should NOT be a recurrence.
	third, err := svc.CreateGeneric(ctx, GenericIncident{
		Host: "web-01", Title: "other", Source: "alertmanager", ExternalID: "OtherAlert",
	})
	if err != nil {
		t.Fatal(err)
	}
	has, _ = st.HasEvent(ctx, third.ID, "recurrence")
	if has {
		t.Fatal("expected no recurrence for unrelated alert")
	}
}

func TestCreateGenericInvalidSeverity(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st)
	if _, err := svc.CreateGeneric(context.Background(), GenericIncident{Host: "h", Title: "t", Severity: "nuclear"}); err == nil {
		t.Fatal("expected error for invalid severity")
	}
}

func TestAlertmanagerPayload(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st)
	created, err := svc.CreateAlertmanager(context.Background(), AlertmanagerPayload{
		Status: "firing",
		Alerts: []Alertmanager{
			{Status: "firing", Labels: map[string]string{"alertname": "HighCPU", "host": "web-01", "severity": "critical"}, Annotations: map[string]string{"summary": "CPU high"}},
			{Status: "firing", Labels: map[string]string{"alertname": "DiskFull", "instance": "db-02:9100"}},
			{Status: "resolved", Labels: map[string]string{"alertname": "Old", "host": "web-01"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateAlertmanager: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("expected 2 incidents, got %d", len(created))
	}
	if created[0].Host != "web-01" || created[0].Severity != "critical" {
		t.Fatalf("unexpected first incident: %+v", created[0])
	}
	if created[1].Host != "db-02" {
		t.Fatalf("expected host parsed from instance, got %q", created[1].Host)
	}
}

func TestInstanceHost(t *testing.T) {
	cases := map[string]string{
		"db-02:9100":    "db-02",
		"10.0.0.5:9090": "10.0.0.5",
		"no-port":       "no-port",
		"[::1]:9100":    "::1",
		"[2001:db8::1]": "2001:db8::1",
	}
	for in, want := range cases {
		if got := instanceHost(in); got != want {
			t.Errorf("instanceHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAlertmanagerEmptyAndResolved(t *testing.T) {
	st := openTestStore(t)
	svc := NewService(st)
	ctx := context.Background()

	// Empty payload -> no incidents.
	created, err := svc.CreateAlertmanager(ctx, AlertmanagerPayload{Status: "firing", Alerts: []Alertmanager{}})
	if err != nil {
		t.Fatalf("empty payload: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("expected 0 from empty payload, got %d", len(created))
	}

	// Fully resolved payload -> no incidents.
	created, err = svc.CreateAlertmanager(ctx, AlertmanagerPayload{
		Status: "resolved",
		Alerts: []Alertmanager{{Status: "firing", Labels: map[string]string{"alertname": "X", "host": "h1"}}},
	})
	if err != nil {
		t.Fatalf("resolved payload: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("expected 0 from resolved payload, got %d", len(created))
	}

	// Alert without a usable host -> skipped.
	created, err = svc.CreateAlertmanager(ctx, AlertmanagerPayload{
		Status: "firing",
		Alerts: []Alertmanager{{Status: "firing", Labels: map[string]string{"alertname": "X"}}},
	})
	if err != nil {
		t.Fatalf("no-host payload: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("expected 0 from no-host alert, got %d", len(created))
	}
}

func TestSeverityNormalization(t *testing.T) {
	cases := map[string]string{
		"critical": "critical",
		"page":     "critical",
		"warning":  "warning",
		"warn":     "warning",
		"info":     "info",
		"none":     "info",
		"weird":    "warning",
	}
	for in, want := range cases {
		if got := normalizeSeverity(in); string(got) != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}
