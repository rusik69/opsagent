package incidents

import (
	"context"
	"fmt"
	"strings"

	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/store"
)

// Service creates incidents from external event sources.
type Service struct {
	store *store.Store
}

func NewService(st *store.Store) *Service { return &Service{store: st} }

type GenericIncident struct {
	Host       string            `json:"host"`
	Severity   string            `json:"severity"`
	Title      string            `json:"title"`
	Message    string            `json:"message"`
	ExternalID string            `json:"external_id"`
	Source     string            `json:"source"`
	Labels     map[string]string `json:"labels"`
}

// CreateGeneric normalizes a generic webhook payload into an Incident.
func (s *Service) CreateGeneric(ctx context.Context, g GenericIncident) (*model.Incident, error) {
	host := strings.TrimSpace(g.Host)
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	title := strings.TrimSpace(g.Title)
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}
	sev := model.Severity(g.Severity)
	switch sev {
	case model.SeverityInfo, model.SeverityWarning, model.SeverityCritical, "":
	default:
		return nil, fmt.Errorf("invalid severity %q", g.Severity)
	}
	if sev == "" {
		sev = model.SeverityWarning
	}
	if g.Labels == nil {
		g.Labels = map[string]string{}
	}
	src := g.Source
	if src == "" {
		src = "generic"
	}
	inc := &model.Incident{
		ExternalID: g.ExternalID,
		Source:     src,
		Host:       host,
		Severity:   sev,
		Title:      title,
		Message:    g.Message,
		Labels:     g.Labels,
		Status:     model.IncidentOpen,
	}
	created, err := s.store.CreateIncident(ctx, inc)
	if err != nil {
		return nil, err
	}
	_, _ = s.store.AddEvent(ctx, created.ID, model.EventCreated, fmt.Sprintf("%s: %s", created.Host, created.Title))
	s.detectRecurrence(ctx, created)
	return created, nil
}

// detectRecurrence marks a new incident as a recurrence when the same alert
// was previously resolved — a signal that the earlier fix may have failed.
func (s *Service) detectRecurrence(ctx context.Context, inc *model.Incident) {
	if inc.Source == "" || inc.ExternalID == "" || inc.Host == "" {
		return
	}
	prior, err := s.store.FindResolvedIncidentByExternal(ctx, inc.Source, inc.ExternalID, inc.Host)
	if err != nil || prior == nil {
		return
	}
	resolved := "unknown"
	if prior.ResolvedAt != nil {
		resolved = prior.ResolvedAt.Format("2006-01-02 15:04")
	}
	_, _ = s.store.AddEvent(ctx, inc.ID, model.EventRecurrence,
		fmt.Sprintf("recurrence of incident #%d (resolved %s via %s)", prior.ID, resolved, prior.ResolvedVia))
}

// AlertmanagerPayload is a Prometheus Alertmanager webhook payload.
type AlertmanagerPayload struct {
	Status string         `json:"status"`
	Alerts []Alertmanager `json:"alerts"`
}

type Alertmanager struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

// CreateAlertmanager consumes an Alertmanager webhook payload and creates one
// incident per firing alert. Host is derived from labels (host/hostname/instance).
func (s *Service) CreateAlertmanager(ctx context.Context, payload AlertmanagerPayload) ([]*model.Incident, error) {
	created := []*model.Incident{}
	for _, a := range payload.Alerts {
		if a.Status == "resolved" || payload.Status == "resolved" {
			continue
		}
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		host := firstNonEmpty(labels["host"], labels["hostname"], instanceHost(labels["instance"]))
		if host == "" {
			continue
		}
		title := labels["alertname"]
		if title == "" {
			title = "alert"
		}
		sev := normalizeSeverity(labels["severity"])
		msg := a.Annotations["description"]
		if msg == "" {
			msg = a.Annotations["summary"]
		}
		inc := &model.Incident{
			ExternalID: a.Labels["alertname"] + ":" + host + ":" + a.StartsAt,
			Source:     "alertmanager",
			Host:       host,
			Severity:   sev,
			Title:      title,
			Message:    msg,
			Labels:     labels,
			Status:     model.IncidentOpen,
		}
		createdInc, err := s.store.CreateIncident(ctx, inc)
		if err != nil {
			return created, fmt.Errorf("create incident from alertmanager: %w", err)
		}
		created = append(created, createdInc)
	}
	return created, nil
}

func instanceHost(instance string) string {
	// Support "[::1]:9100", "host:9100", "10.0.0.5", "host".
	if strings.HasPrefix(instance, "[") {
		if i := strings.Index(instance, "]"); i >= 0 {
			return instance[1:i]
		}
	}
	if i := strings.LastIndex(instance, ":"); i >= 0 {
		return instance[:i]
	}
	return instance
}

func normalizeSeverity(s string) model.Severity {
	switch s {
	case "critical", "page":
		return model.SeverityCritical
	case "warning", "warn":
		return model.SeverityWarning
	case "info", "none":
		return model.SeverityInfo
	}
	return model.SeverityWarning
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
