package store

import (
	"context"
	"fmt"
	"time"
)

// PruneOlderThan deletes command runs and incident events older than cutoff,
// returning the number of rows removed from each table. Diagnoses, incidents,
// groups and retrospectives are retained as the durable record.
func (s *Store) PruneOlderThan(ctx context.Context, cutoff time.Time) (commandRuns, events int64, err error) {
	ts := cutoff.UTC().Format(timeFmt)
	res, err := s.db.ExecContext(ctx, `DELETE FROM command_runs WHERE created_at < ?`, ts)
	if err != nil {
		return 0, 0, fmt.Errorf("prune command_runs: %w", err)
	}
	commandRuns, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `DELETE FROM incident_events WHERE created_at < ?`, ts)
	if err != nil {
		return commandRuns, 0, fmt.Errorf("prune incident_events: %w", err)
	}
	events, _ = res.RowsAffected()
	return commandRuns, events, nil
}
