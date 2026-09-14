package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/rusik69/opsagent/internal/model"
)

func (s *Store) CreateCommandRun(ctx context.Context, r *model.CommandRun) (*model.CommandRun, error) {
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	params, _ := json.Marshal(r.Params)
	if r.Status == "" {
		r.Status = "pending"
	}
	ts := now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = timeParse(ts)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO command_runs (incident_id, host, command_id, params_json, command, status, stdout, stderr, duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.IncidentID, r.Host, r.CommandID, string(params), r.Command, r.Status,
		r.Stdout, r.Stderr, r.DurationMS, r.CreatedAt.Format(timeFmt))
	if err != nil {
		return nil, fmt.Errorf("create command run: %w", err)
	}
	r.ID, _ = res.LastInsertId()
	return r, nil
}

func (s *Store) UpdateCommandRun(ctx context.Context, r *model.CommandRun) error {
	params, _ := json.Marshal(r.Params)
	_, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, stdout = ?, stderr = ?, duration_ms = ?, params_json = ? WHERE id = ?`,
		r.Status, r.Stdout, r.Stderr, r.DurationMS, string(params), r.ID)
	return err
}

func (s *Store) ListCommandRuns(ctx context.Context, incidentID int64) ([]*model.CommandRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, incident_id, host, command_id, params_json, command, status, stdout, stderr, duration_ms, created_at
		 FROM command_runs WHERE incident_id = ? ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommandRuns(rows)
}

func (s *Store) ListAllCommandRuns(ctx context.Context, limit int) ([]*model.CommandRun, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, incident_id, host, command_id, params_json, command, status, stdout, stderr, duration_ms, created_at
		 FROM command_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCommandRuns(rows)
}

func scanCommandRuns(rows *sql.Rows) ([]*model.CommandRun, error) {
	out := []*model.CommandRun{}
	for rows.Next() {
		var (
			r         model.CommandRun
			incID     sql.NullInt64
			params    string
			createdAt string
		)
		if err := rows.Scan(&r.ID, &incID, &r.Host, &r.CommandID, &params, &r.Command,
			&r.Status, &r.Stdout, &r.Stderr, &r.DurationMS, &createdAt); err != nil {
			return nil, fmt.Errorf("scan command run: %w", err)
		}
		if incID.Valid {
			r.IncidentID = &incID.Int64
		}
		r.CreatedAt = timeParse(createdAt)
		_ = json.Unmarshal([]byte(params), &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
