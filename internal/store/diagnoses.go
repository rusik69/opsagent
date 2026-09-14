package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/rusik69/opsagent/internal/model"
)

func (s *Store) CreateDiagnosis(ctx context.Context, d *model.Diagnosis) (*model.Diagnosis, error) {
	if d.Steps == nil {
		d.Steps = []model.DiagnosisStep{}
	}
	steps, _ := json.Marshal(d.Steps)
	ts := now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = timeParse(ts)
	}
	d.UpdatedAt = d.CreatedAt
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO diagnoses (incident_id, status, report, summary, steps_json, logs, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.IncidentID, d.Status, d.Report, d.Summary, string(steps), d.Logs,
		d.CreatedAt.Format(timeFmt), d.UpdatedAt.Format(timeFmt))
	if err != nil {
		return nil, fmt.Errorf("create diagnosis: %w", err)
	}
	d.ID, _ = res.LastInsertId()
	return d, nil
}

func (s *Store) GetDiagnosis(ctx context.Context, incidentID int64) (*model.Diagnosis, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, incident_id, status, report, summary, steps_json, logs, created_at, updated_at FROM diagnoses WHERE incident_id = ?`,
		incidentID)
	d, err := scanDiagnosis(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func scanDiagnosis(row interface{ Scan(...any) error }) (*model.Diagnosis, error) {
	var (
		d         model.Diagnosis
		stepsJSON string
		createdAt string
		updatedAt string
	)
	if err := row.Scan(&d.ID, &d.IncidentID, &d.Status, &d.Report, &d.Summary,
		&stepsJSON, &d.Logs, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	d.CreatedAt = timeParse(createdAt)
	d.UpdatedAt = timeParse(updatedAt)
	_ = json.Unmarshal([]byte(stepsJSON), &d.Steps)
	if d.Steps == nil {
		d.Steps = []model.DiagnosisStep{}
	}
	return &d, nil
}

func (s *Store) UpdateDiagnosis(ctx context.Context, d *model.Diagnosis) error {
	steps, _ := json.Marshal(d.Steps)
	_, err := s.db.ExecContext(ctx,
		`UPDATE diagnoses SET status = ?, report = ?, summary = ?, steps_json = ?, logs = ?, updated_at = ? WHERE id = ?`,
		d.Status, d.Report, d.Summary, string(steps), d.Logs, now(), d.ID)
	return err
}

// ListCompletedIncidents returns incidents that have a finished ('done')
// diagnosis, newest first — the input for the self-improvement review.
func (s *Store) ListCompletedIncidents(ctx context.Context, limit int) ([]*model.Incident, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixedIncidentCols("i")+` FROM incidents i
		 JOIN diagnoses d ON d.incident_id = i.id
		 WHERE d.status = 'done'
		 ORDER BY i.id DESC LIMIT ?`, limit)
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
