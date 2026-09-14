package store

import (
	"context"
	"fmt"

	"github.com/rusik69/opsagent/internal/model"
)

// AddEvent appends a timeline event for an incident.
func (s *Store) AddEvent(ctx context.Context, incidentID int64, kind, detail string) (*model.IncidentEvent, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incident_events (incident_id, kind, detail, created_at) VALUES (?, ?, ?, ?)`,
		incidentID, kind, detail, now())
	if err != nil {
		return nil, fmt.Errorf("add event: %w", err)
	}
	id, _ := res.LastInsertId()
	return &model.IncidentEvent{ID: id, IncidentID: incidentID, Kind: kind, Detail: detail, CreatedAt: timeParse(now())}, nil
}

// HasEvent reports whether an incident has at least one event of a kind.
func (s *Store) HasEvent(ctx context.Context, incidentID int64, kind string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incident_events WHERE incident_id = ? AND kind = ?`, incidentID, kind).Scan(&n)
	return n > 0, err
}

// ListEvents returns the timeline for an incident, oldest first.
func (s *Store) ListEvents(ctx context.Context, incidentID int64) ([]*model.IncidentEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, incident_id, kind, detail, created_at FROM incident_events WHERE incident_id = ? ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.IncidentEvent{}
	for rows.Next() {
		var (
			e         model.IncidentEvent
			createdAt string
		)
		if err := rows.Scan(&e.ID, &e.IncidentID, &e.Kind, &e.Detail, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.CreatedAt = timeParse(createdAt)
		out = append(out, &e)
	}
	return out, rows.Err()
}
