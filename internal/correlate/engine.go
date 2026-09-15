package correlate

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rusik69/opsagent/internal/model"
	"github.com/rusik69/opsagent/internal/store"
)

// Config controls how incidents are correlated.
type Config struct {
	Window    time.Duration
	Methods   []string // host, alertname, label, rootcause
	LabelKeys []string // label keys to correlate on
}

func DefaultConfig() Config {
	return Config{
		Window:    2 * time.Hour,
		Methods:   []string{model.GroupHost, model.GroupAlertname, model.GroupLabel, model.GroupRootCause},
		LabelKeys: []string{"cluster", "service", "region"},
	}
}

func (c Config) enabled(method string) bool {
	for _, m := range c.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// Engine groups related incidents using the configured methods.
type Engine struct {
	store *store.Store
	cfg   Config
}

func NewEngine(st *store.Store, cfg Config) *Engine { return &Engine{store: st, cfg: cfg} }

// Run correlates all incidents created within the window.
func (e *Engine) Run(ctx context.Context) error {
	since := time.Now().UTC().Add(-e.cfg.Window)
	incs, err := e.store.RecentIncidentsForCorrelation(ctx, since, 1000)
	if err != nil {
		return err
	}
	return e.correlate(ctx, incs)
}

// CorrelateIncident re-runs correlation including a specific incident,
// so a newly diagnosed incident joins any recent matching groups.
func (e *Engine) CorrelateIncident(ctx context.Context, incidentID int64) error {
	inc, err := e.store.GetIncident(ctx, incidentID)
	if err != nil || inc == nil {
		return err
	}
	since := time.Now().UTC().Add(-e.cfg.Window)
	incs, err := e.store.RecentIncidentsForCorrelation(ctx, since, 1000)
	if err != nil {
		return err
	}
	found := false
	for _, i := range incs {
		if i.ID == inc.ID {
			found = true
			break
		}
	}
	if !found {
		incs = append(incs, inc)
	}
	return e.correlate(ctx, incs)
}

// LinkByAgent explicitly correlates an incident with others (agent-driven).
func (e *Engine) LinkByAgent(ctx context.Context, primaryID int64, relatedIDs []int64, reason string) (*model.IncidentGroup, error) {
	ids := append([]int64{primaryID}, relatedIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	key := fmt.Sprintf("%s:%d", model.GroupAgent, primaryID)
	label := reason
	if label == "" {
		label = "agent-correlated with #" + fmt.Sprint(primaryID)
	}
	g, err := e.store.GetOrCreateGroup(ctx, model.GroupAgent, key, label)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		added, err := e.store.AddIncidentToGroup(ctx, g.ID, id)
		if err != nil {
			return nil, err
		}
		if added {
			_, _ = e.store.AddEvent(ctx, id, model.EventCorrelated, label)
		}
	}
	return g, nil
}

// bucket produces the grouping key: kind:value:hour-bucket.
func bucket(kind, value string, t time.Time) string {
	return fmt.Sprintf("%s:%s:%s", kind, sanitize(value), t.UTC().Format("2006010215"))
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		case r == ' ' || r == '/' || r == ':':
			sb.WriteRune('-')
		}
	}
	if sb.Len() == 0 {
		return "unknown"
	}
	if sb.Len() > 40 {
		return sb.String()[:40]
	}
	return sb.String()
}

func (e *Engine) correlate(ctx context.Context, incs []*model.Incident) error {
	if len(incs) < 2 {
		return nil
	}
	if e.cfg.enabled(model.GroupHost) {
		if err := e.groupBy(ctx, model.GroupHost, incs, func(i *model.Incident) []string {
			if i.Host == "" {
				return nil
			}
			return []string{i.Host}
		}); err != nil {
			return err
		}
	}
	if e.cfg.enabled(model.GroupAlertname) {
		if err := e.groupBy(ctx, model.GroupAlertname, incs, func(i *model.Incident) []string {
			if a := i.Labels["alertname"]; a != "" {
				return []string{a}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if e.cfg.enabled(model.GroupLabel) {
		if err := e.groupBy(ctx, model.GroupLabel, incs, func(i *model.Incident) []string {
			var vals []string
			for _, k := range e.cfg.LabelKeys {
				if v := i.Labels[k]; v != "" {
					vals = append(vals, k+"="+v)
				}
			}
			return vals
		}); err != nil {
			return err
		}
	}
	if e.cfg.enabled(model.GroupRootCause) {
		if err := e.groupBy(ctx, model.GroupRootCause, incs, func(i *model.Incident) []string {
			if i.RootCause != "" {
				return []string{i.RootCause}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) groupBy(ctx context.Context, kind string, incs []*model.Incident, extract func(*model.Incident) []string) error {
	clusters := map[string][]int64{}
	labels := map[string]string{}
	for _, inc := range incs {
		for _, value := range extract(inc) {
			key := bucket(kind, value, inc.CreatedAt)
			clusters[key] = append(clusters[key], inc.ID)
			labels[key] = labelFor(kind, value)
		}
	}
	for key, ids := range clusters {
		if len(ids) < 2 {
			continue
		}
		g, err := e.store.GetOrCreateGroup(ctx, kind, key, labels[key])
		if err != nil {
			return err
		}
		for _, id := range ids {
			added, err := e.store.AddIncidentToGroup(ctx, g.ID, id)
			if err != nil {
				return err
			}
			if added {
				_, _ = e.store.AddEvent(ctx, id, model.EventCorrelated, labels[key])
			}
		}
	}
	return nil
}

func labelFor(kind, value string) string {
	switch kind {
	case model.GroupHost:
		return "host burst: " + value
	case model.GroupAlertname:
		return "alert: " + value
	case model.GroupLabel:
		return "label: " + value
	case model.GroupRootCause:
		return "root cause: " + value
	}
	return value
}
