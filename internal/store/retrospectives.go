package store

import (
	"context"
	"fmt"

	"github.com/rusik69/opsagent/internal/model"
)

func (s *Store) CreateRetrospective(ctx context.Context, r *model.Retrospective) (*model.Retrospective, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO retrospectives (window_start, window_end, incidents_reviewed, summary, memories_created, instructions_created, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.WindowStart.UTC().Format(timeFmt), r.WindowEnd.UTC().Format(timeFmt),
		r.IncidentsReviewd, r.Summary, r.MemoriesCreated, r.InstructionsCreated, now())
	if err != nil {
		return nil, fmt.Errorf("create retrospective: %w", err)
	}
	r.ID, _ = res.LastInsertId()
	r.CreatedAt = timeParse(now())
	return r, nil
}

func (s *Store) ListRetrospectives(ctx context.Context, limit int) ([]*model.Retrospective, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, window_start, window_end, incidents_reviewed, summary, memories_created, instructions_created, created_at
		 FROM retrospectives ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Retrospective{}
	for rows.Next() {
		var (
			r          model.Retrospective
			ws, we, ca string
		)
		if err := rows.Scan(&r.ID, &ws, &we, &r.IncidentsReviewd, &r.Summary,
			&r.MemoriesCreated, &r.InstructionsCreated, &ca); err != nil {
			return nil, fmt.Errorf("scan retrospective: %w", err)
		}
		r.WindowStart = timeParse(ws)
		r.WindowEnd = timeParse(we)
		r.CreatedAt = timeParse(ca)
		out = append(out, &r)
	}
	return out, rows.Err()
}
