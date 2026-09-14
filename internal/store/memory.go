package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rusik69/opsagent/internal/model"
)

func (s *Store) CreateMemory(ctx context.Context, m *model.Memory) (*model.Memory, error) {
	if m.Tags == nil {
		m.Tags = []string{}
	}
	tags, _ := json.Marshal(m.Tags)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = timeParse(now())
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (topic, content, tags_json, created_at) VALUES (?, ?, ?, ?)`,
		m.Topic, m.Content, string(tags), m.CreatedAt.Format(timeFmt))
	if err != nil {
		return nil, fmt.Errorf("create memory: %w", err)
	}
	m.ID, _ = res.LastInsertId()
	return m, nil
}

func (s *Store) ListMemories(ctx context.Context, limit int) ([]*model.Memory, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, topic, content, tags_json, created_at FROM memories ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

func (s *Store) SearchMemories(ctx context.Context, query string, limit int) ([]*model.Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	like := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, topic, content, tags_json, created_at FROM memories
		 WHERE lower(topic) LIKE ? OR lower(content) LIKE ? OR lower(tags_json) LIKE ?
		 ORDER BY id DESC LIMIT ?`, like, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// MemoryExists reports whether a memory with the topic already exists.
func (s *Store) MemoryExists(ctx context.Context, topic string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE lower(topic) = ?`, strings.ToLower(strings.TrimSpace(topic))).Scan(&n)
	return n > 0, err
}

func scanMemories(rows *sql.Rows) ([]*model.Memory, error) {
	out := []*model.Memory{}
	for rows.Next() {
		var (
			m         model.Memory
			tags      string
			createdAt string
		)
		if err := rows.Scan(&m.ID, &m.Topic, &m.Content, &tags, &createdAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		m.CreatedAt = timeParse(createdAt)
		_ = json.Unmarshal([]byte(tags), &m.Tags)
		if m.Tags == nil {
			m.Tags = []string{}
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (s *Store) CreateInstruction(ctx context.Context, i *model.Instruction) (*model.Instruction, error) {
	if i.CreatedAt.IsZero() {
		i.CreatedAt = timeParse(now())
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO instructions (content, priority, source, applied, created_at) VALUES (?, ?, ?, ?, ?)`,
		i.Content, i.Priority, i.Source, boolInt(i.Applied), i.CreatedAt.Format(timeFmt))
	if err != nil {
		return nil, fmt.Errorf("create instruction: %w", err)
	}
	i.ID, _ = res.LastInsertId()
	return i, nil
}

func (s *Store) ListInstructions(ctx context.Context, limit int) ([]*model.Instruction, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, priority, source, applied, created_at FROM instructions ORDER BY priority ASC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Instruction{}
	for rows.Next() {
		var (
			i         model.Instruction
			applied   int
			createdAt string
		)
		if err := rows.Scan(&i.ID, &i.Content, &i.Priority, &i.Source, &applied, &createdAt); err != nil {
			return nil, fmt.Errorf("scan instruction: %w", err)
		}
		i.Applied = applied == 1
		i.CreatedAt = timeParse(createdAt)
		out = append(out, &i)
	}
	return out, rows.Err()
}

// InstructionExists reports whether an instruction with the same content exists.
func (s *Store) InstructionExists(ctx context.Context, content string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM instructions WHERE lower(content) = ?`, strings.ToLower(strings.TrimSpace(content))).Scan(&n)
	return n > 0, err
}

func (s *Store) MarkInstructionApplied(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE instructions SET applied = 1 WHERE id = ?`, id)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
