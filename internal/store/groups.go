package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rusik69/opsagent/internal/model"
)

// GetOrCreateGroup returns the group for (kind, key), creating it if missing.
func (s *Store) GetOrCreateGroup(ctx context.Context, kind, key, label string) (*model.IncidentGroup, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, kind, key, label, created_at FROM incident_groups WHERE kind = ? AND key = ?`, kind, key)
	var g model.IncidentGroup
	var createdAt string
	err := row.Scan(&g.ID, &g.Kind, &g.Key, &g.Label, &createdAt)
	if err == nil {
		g.CreatedAt = timeParse(createdAt)
		return &g, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incident_groups (kind, key, label, created_at) VALUES (?, ?, ?, ?)`,
		kind, key, label, now())
	if err != nil {
		return nil, fmt.Errorf("create group: %w", err)
	}
	id, _ := res.LastInsertId()
	return &model.IncidentGroup{ID: id, Kind: kind, Key: key, Label: label, CreatedAt: timeParse(now())}, nil
}

// AddIncidentToGroup links an incident to a group (idempotent).
func (s *Store) AddIncidentToGroup(ctx context.Context, groupID, incidentID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO incident_group_members (group_id, incident_id, added_at) VALUES (?, ?, ?)`,
		groupID, incidentID, now())
	if err != nil {
		return false, fmt.Errorf("add group member: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GroupByIncident returns the groups an incident belongs to.
func (s *Store) GroupByIncident(ctx context.Context, incidentID int64) ([]*model.IncidentGroup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.id, g.kind, g.key, g.label, g.created_at,
		        (SELECT COUNT(*) FROM incident_group_members m2 WHERE m2.group_id = g.id)
		 FROM incident_groups g
		 JOIN incident_group_members m ON m.group_id = g.id
		 WHERE m.incident_id = ? ORDER BY g.id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGroups(rows)
}

// ListGroups returns groups that contain at least one incident, newest first.
func (s *Store) ListGroups(ctx context.Context, limit int) ([]*model.IncidentGroup, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.id, g.kind, g.key, g.label, g.created_at,
		        (SELECT COUNT(*) FROM incident_group_members m WHERE m.group_id = g.id)
		 FROM incident_groups g
		 WHERE EXISTS (SELECT 1 FROM incident_group_members m WHERE m.group_id = g.id)
		 ORDER BY g.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGroups(rows)
}

// GroupMemberIDs returns incident ids for a group, oldest first.
func (s *Store) GroupMemberIDs(ctx context.Context, groupID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT incident_id FROM incident_group_members WHERE group_id = ? ORDER BY incident_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RelatedIncidentIDs returns incident ids in the same groups as incidentID.
func (s *Store) RelatedIncidentIDs(ctx context.Context, incidentID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT m2.incident_id
		 FROM incident_group_members m
		 JOIN incident_group_members m2 ON m2.group_id = m.group_id
		 WHERE m.incident_id = ? AND m2.incident_id != ? ORDER BY m2.incident_id`, incidentID, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanGroups(rows *sql.Rows) ([]*model.IncidentGroup, error) {
	out := []*model.IncidentGroup{}
	for rows.Next() {
		var (
			g         model.IncidentGroup
			createdAt string
			count     int
		)
		if err := rows.Scan(&g.ID, &g.Kind, &g.Key, &g.Label, &createdAt, &count); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		g.CreatedAt = timeParse(createdAt)
		_ = count
		out = append(out, &g)
	}
	return out, rows.Err()
}
