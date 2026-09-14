package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rusik69/opsagent/internal/model"
)

const incidentCols = `id, external_id, source, host, severity, title, message, labels_json, status, solution, mr_url, root_cause, confidence, resolved_via, resolved_at, created_at, updated_at`

// prefixedIncidentCols returns the incident columns qualified with an alias.
func prefixedIncidentCols(alias string) string {
	parts := strings.Split(incidentCols, ", ")
	qualified := make([]string, 0, len(parts))
	for _, p := range parts {
		qualified = append(qualified, alias+"."+p)
	}
	return strings.Join(qualified, ", ")
}

func scanIncident(row interface{ Scan(...any) error }) (*model.Incident, error) {
	var (
		inc        model.Incident
		labels     string
		status     string
		createdAt  string
		updatedAt  string
		resolvedAt sql.NullString
	)
	if err := row.Scan(&inc.ID, &inc.ExternalID, &inc.Source, &inc.Host, &inc.Severity,
		&inc.Title, &inc.Message, &labels, &status, &inc.Solution, &inc.MRURL,
		&inc.RootCause, &inc.Confidence, &inc.ResolvedVia, &resolvedAt,
		&createdAt, &updatedAt); err != nil {
		return nil, err
	}
	inc.Status = model.IncidentStatus(status)
	inc.CreatedAt = timeParse(createdAt)
	inc.UpdatedAt = timeParse(updatedAt)
	if resolvedAt.Valid {
		t := timeParse(resolvedAt.String)
		inc.ResolvedAt = &t
	}
	_ = json.Unmarshal([]byte(labels), &inc.Labels)
	if inc.Labels == nil {
		inc.Labels = map[string]string{}
	}
	return &inc, nil
}

func (s *Store) CreateIncident(ctx context.Context, inc *model.Incident) (*model.Incident, error) {
	if inc.Labels == nil {
		inc.Labels = map[string]string{}
	}
	labels, _ := json.Marshal(inc.Labels)
	ts := now()
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = timeParse(ts)
	}
	inc.UpdatedAt = inc.CreatedAt
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents (external_id, source, host, severity, title, message, labels_json, status, solution, mr_url, root_cause, confidence, resolved_via, resolved_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inc.ExternalID, inc.Source, inc.Host, inc.Severity, inc.Title, inc.Message,
		string(labels), inc.Status, inc.Solution, inc.MRURL, inc.RootCause, inc.Confidence, inc.ResolvedVia,
		formatResolvedAt(inc.ResolvedAt), inc.CreatedAt.Format(timeFmt), inc.UpdatedAt.Format(timeFmt))
	if err != nil {
		return nil, fmt.Errorf("create incident: %w", err)
	}
	id, _ := res.LastInsertId()
	inc.ID = id
	if inc.Status == "" {
		inc.Status = model.IncidentOpen
	}
	return inc, nil
}

func formatResolvedAt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeFmt)
}

func (s *Store) GetIncident(ctx context.Context, id int64) (*model.Incident, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+incidentCols+` FROM incidents WHERE id = ?`, id)
	inc, err := scanIncident(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inc, nil
}

func (s *Store) ListIncidents(ctx context.Context, status string, limit int) ([]*model.Incident, error) {
	return s.ListIncidentsFiltered(ctx, status, "", "", limit)
}

// ListIncidentsFiltered lists incidents optionally filtered by status,
// severity and a free-text query against host/title/message/source.
func (s *Store) ListIncidentsFiltered(ctx context.Context, status, severity, query string, limit int) ([]*model.Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + incidentCols + ` FROM incidents`
	conds := []string{}
	args := []any{}
	if status != "" {
		conds = append(conds, `status = ?`)
		args = append(args, status)
	}
	if severity != "" {
		conds = append(conds, `severity = ?`)
		args = append(args, severity)
	}
	if query != "" {
		conds = append(conds, `(lower(host) LIKE ? OR lower(title) LIKE ? OR lower(message) LIKE ? OR lower(source) LIKE ? OR CAST(id AS TEXT) = ?)`)
		like := "%" + strings.ToLower(query) + "%"
		args = append(args, like, like, like, like, query)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Incident{}
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *Store) UpdateIncidentStatus(ctx context.Context, id int64, status model.IncidentStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	return err
}

// UpdateIncidentStatusIfActive transitions an incident only while it is still
// open or diagnosing, so a concurrent operator action (resolved/cancelled) is
// not overwritten by a finishing diagnosis.
func (s *Store) UpdateIncidentStatusIfActive(ctx context.Context, id int64, status model.IncidentStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = ?, updated_at = ? WHERE id = ? AND status IN (?, ?)`,
		status, now(), id, model.IncidentOpen, model.IncidentDiagnosing)
	return err
}

func (s *Store) UpdateIncidentSolution(ctx context.Context, id int64, solution, mrURL string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET solution = ?, mr_url = ?, updated_at = ? WHERE id = ?`,
		solution, mrURL, now(), id)
	return err
}

// UpdateIncidentOutcome records root cause, confidence and resolution info.
func (s *Store) UpdateIncidentOutcome(ctx context.Context, id int64, rootCause, confidence, resolvedVia string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET root_cause = ?, confidence = ?, resolved_via = ?, updated_at = ? WHERE id = ?`,
		rootCause, confidence, resolvedVia, now(), id)
	return err
}

// MarkResolved transitions an incident to resolved and records the mechanism.
func (s *Store) MarkResolved(ctx context.Context, id int64, via string) error {
	t := now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = ?, resolved_via = ?, resolved_at = ?, updated_at = ? WHERE id = ?`,
		model.IncidentResolved, via, t, t, id)
	return err
}

// RecentIncidentsForCorrelation returns incidents created within the given
// window, newest first, for correlation grouping.
func (s *Store) RecentIncidentsForCorrelation(ctx context.Context, since time.Time, limit int) ([]*model.Incident, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+incidentCols+` FROM incidents WHERE created_at >= ? ORDER BY id DESC LIMIT ?`,
		since.UTC().Format(timeFmt), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Incident{}
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// FindResolvedIncidentByExternal returns a resolved incident matching the same
// (source, external_id, host), used to detect recurrence after a fix.
func (s *Store) FindResolvedIncidentByExternal(ctx context.Context, source, externalID, host string) (*model.Incident, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+incidentCols+` FROM incidents
		 WHERE source = ? AND external_id = ? AND host = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		source, externalID, host, model.IncidentResolved)
	inc, err := scanIncident(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inc, nil
}

// ListIncidentsWithMR returns incidents that have an MR URL and are not yet
// resolved, used by the merge poller.
func (s *Store) ListIncidentsWithMR(ctx context.Context, limit int) ([]*model.Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+incidentCols+` FROM incidents WHERE mr_url != '' AND status != ? ORDER BY id LIMIT ?`,
		model.IncidentResolved, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Incident{}
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}
