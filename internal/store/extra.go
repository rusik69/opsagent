package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/rusik69/opsagent/internal/model"
)

// FindOpenIncidentByExternal returns an open incident matching the same
// (source, external_id, host), used to deduplicate intake and to resolve
// incidents when an alert clears.
func (s *Store) FindOpenIncidentByExternal(ctx context.Context, source, externalID, host string) (*model.Incident, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+incidentCols+` FROM incidents
		 WHERE source = ? AND external_id = ? AND host = ? AND status IN (?, ?)
		 ORDER BY id DESC LIMIT 1`,
		source, externalID, host, model.IncidentOpen, model.IncidentDiagnosing)
	inc, err := scanIncident(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return inc, err
}

// UpdateIncidentDetails refreshes the mutable descriptive fields of an
// incident, used when a duplicate alert is received for an existing incident.
func (s *Store) UpdateIncidentDetails(ctx context.Context, id int64, severity model.Severity, title, message string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET severity = ?, title = ?, message = ?, updated_at = ? WHERE id = ?`,
		severity, title, message, now(), id)
	return err
}

// DeleteIncident soft-deletes an incident.
func (s *Store) DeleteIncident(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = ?, updated_at = ? WHERE id = ?`,
		model.IncidentDeleted, now(), id)
	return err
}

// BulkUpdateStatus transitions multiple incidents to a status in one query.
func (s *Store) BulkUpdateStatus(ctx context.Context, ids []int64, status model.IncidentStatus) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+2)
	args = append(args, status, now())
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE incidents SET status = ?, updated_at = ? WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IncidentStats aggregates incident counters for the dashboard.
type IncidentStats struct {
	Total          int64       `json:"total"`
	Open           int64       `json:"open"`
	Diagnosing     int64       `json:"diagnosing"`
	Diagnosed      int64       `json:"diagnosed"`
	Resolved       int64       `json:"resolved"`
	Cancelled      int64       `json:"cancelled"`
	Error          int64       `json:"error"`
	Deleted        int64       `json:"deleted"`
	Critical       int64       `json:"critical"`
	Recurrences    int64       `json:"recurrences"`
	AvgDiagnosisMS int64       `json:"avg_diagnosis_ms"`
	PerHost        []HostCount `json:"per_host"`
	PerDay         []DayCount  `json:"per_day"`
}

// HostCount is a host -> incident count pair.
type HostCount struct {
	Host  string `json:"host"`
	Count int64  `json:"count"`
}

// DayCount is a day -> incident count pair (day is YYYY-MM-DD).
type DayCount struct {
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

// IncidentStats computes dashboard aggregates.
func (s *Store) IncidentStats(ctx context.Context) (*IncidentStats, error) {
	st := &IncidentStats{}

	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM incidents GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		switch model.IncidentStatus(status) {
		case model.IncidentOpen:
			st.Open = n
		case model.IncidentDiagnosing:
			st.Diagnosing = n
		case model.IncidentDiagnosed:
			st.Diagnosed = n
		case model.IncidentResolved:
			st.Resolved = n
		case model.IncidentCancelled:
			st.Cancelled = n
		case model.IncidentError:
			st.Error = n
		case model.IncidentDeleted:
			st.Deleted = n
		}
		st.Total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incidents WHERE severity = ?`, model.SeverityCritical).Scan(&st.Critical)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incident_events WHERE kind = ?`, model.EventRecurrence).Scan(&st.Recurrences)
	_ = s.db.QueryRowContext(ctx, `SELECT AVG(duration_ms) FROM command_runs WHERE status = 'success'`).Scan(&st.AvgDiagnosisMS)

	if rows, err := s.db.QueryContext(ctx,
		`SELECT host, COUNT(*) FROM incidents GROUP BY host ORDER BY COUNT(*) DESC LIMIT 10`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var h HostCount
			if rows.Scan(&h.Host, &h.Count) == nil {
				st.PerHost = append(st.PerHost, h)
			}
		}
	}
	if rows, err := s.db.QueryContext(ctx,
		`SELECT substr(created_at, 1, 10) AS day, COUNT(*) FROM incidents GROUP BY day ORDER BY day DESC LIMIT 14`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var d DayCount
			if rows.Scan(&d.Day, &d.Count) == nil {
				st.PerDay = append(st.PerDay, d)
			}
		}
	}
	return st, nil
}

// ListStaleIncidents returns open/diagnosing incidents created before cutoff,
// for the auto-close job.
func (s *Store) ListStaleIncidents(ctx context.Context, cutoff time.Time, limit int) ([]*model.Incident, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+incidentCols+` FROM incidents
		 WHERE status IN (?, ?) AND created_at < ?
		 ORDER BY id LIMIT ?`,
		model.IncidentOpen, model.IncidentDiagnosing, cutoff.UTC().Format(timeFmt), limit)
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
