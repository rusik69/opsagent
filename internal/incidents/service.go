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

// maxAlertsPerPayload bounds the number of alerts ingested from a single
// Alertmanager webhook to protect the store from oversized bursts.
const maxAlertsPerPayload = 500

func NewService(st *store.Store) *Service { return &Service{store: st} }

type GenericIncident struct {
	Host       string            `json:"host"`
	Severity   string            `json:"severity"`
	Title      string            `json:"title"`
	Message    string            `json:"message"`
	ExternalID string            `json:"external_id"`
	Source     string            `json:"source"`
	Labels     map[string]string `json:"labels"`
	Tags       []string          `json:"tags"`
	Owner      string            `json:"owner"`
	Team       string            `json:"team"`
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
	// Deduplicate: an already-open incident for the same external alert is
	// refreshed instead of creating a second one.
	if g.ExternalID != "" {
		if existing, err := s.store.FindOpenIncidentByExternal(ctx, src, g.ExternalID, host); err == nil && existing != nil {
			_ = s.store.UpdateIncidentDetails(ctx, existing.ID, sev, title, g.Message)
			existing.Severity, existing.Title, existing.Message = sev, title, g.Message
			return existing, nil
		}
	}
	inc := &model.Incident{
		ExternalID: g.ExternalID,
		Source:     src,
		Host:       host,
		Severity:   sev,
		Title:      title,
		Message:    g.Message,
		Labels:     g.Labels,
		Tags:       g.Tags,
		Owner:      g.Owner,
		Team:       g.Team,
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
	// Cap the number of alerts ingested per payload to bound work; excess
	// alerts are dropped (they will be re-delivered by Alertmanager).
	if len(payload.Alerts) > maxAlertsPerPayload {
		payload.Alerts = payload.Alerts[:maxAlertsPerPayload]
	}
	for _, a := range payload.Alerts {
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		host := firstNonEmpty(labels["host"], labels["hostname"], instanceHost(labels["instance"]))
		if host == "" {
			continue
		}
		externalID := labels["alertname"] + ":" + host + ":" + a.StartsAt

		// A resolved alert clears any matching open incident (auto-resolution).
		if a.Status == "resolved" || payload.Status == "resolved" {
			if existing, err := s.store.FindOpenIncidentByExternal(ctx, "alertmanager", externalID, host); err == nil && existing != nil {
				if err := s.store.MarkResolved(ctx, existing.ID, "auto"); err == nil {
					_, _ = s.store.AddEvent(ctx, existing.ID, model.EventResolved, "alertmanager: alert resolved")
					created = append(created, existing)
				}
			}
			continue
		}

		// Deduplicate: Alertmanager re-delivers the same firing alert with the
		// same startsAt; refresh the existing incident instead of duplicating.
		if existing, err := s.store.FindOpenIncidentByExternal(ctx, "alertmanager", externalID, host); err == nil && existing != nil {
			created = append(created, existing)
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
			ExternalID: externalID,
			Source:     "alertmanager",
			Host:       host,
			Severity:   sev,
			Title:      title,
			Message:    msg,
			Labels:     labels,
			Owner:      labels["owner"],
			Team:       labels["team"],
			Status:     model.IncidentOpen,
		}
		createdInc, err := s.store.CreateIncident(ctx, inc)
		if err != nil {
			return created, fmt.Errorf("create incident from alertmanager: %w", err)
		}
		_, _ = s.store.AddEvent(ctx, createdInc.ID, model.EventCreated, fmt.Sprintf("%s: %s", createdInc.Host, createdInc.Title))
		s.detectRecurrence(ctx, createdInc)
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
