package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
	// ftsMemories reports whether the FTS5 memory index is available; when it
	// is not (e.g. the driver lacks FTS5), memory search falls back to LIKE.
	ftsMemories bool
}

func Open(path string) (*Store, error) {
	if path == "" {
		path = "./data/opsagent.db"
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create storage dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS incidents (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	external_id TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL DEFAULT '',
	severity TEXT NOT NULL DEFAULT 'info',
	title TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	labels_json TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL DEFAULT 'open',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_incidents_status ON incidents(status);
CREATE INDEX IF NOT EXISTS idx_incidents_host ON incidents(host);

CREATE TABLE IF NOT EXISTS diagnoses (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'running',
	report TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	steps_json TEXT NOT NULL DEFAULT '[]',
	logs TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_diagnoses_incident ON diagnoses(incident_id);

CREATE TABLE IF NOT EXISTS command_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER,
	host TEXT NOT NULL DEFAULT '',
	command_id TEXT NOT NULL DEFAULT '',
	params_json TEXT NOT NULL DEFAULT '{}',
	command TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	stdout TEXT NOT NULL DEFAULT '',
	stderr TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_command_runs_incident ON command_runs(incident_id);
CREATE INDEX IF NOT EXISTS idx_command_runs_host ON command_runs(host);

CREATE TABLE IF NOT EXISTS memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	topic TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL DEFAULT '',
	tags_json TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_topic ON memories(topic);

CREATE TABLE IF NOT EXISTS instructions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	content TEXT NOT NULL DEFAULT '',
	priority INTEGER NOT NULL DEFAULT 5,
	source TEXT NOT NULL DEFAULT '',
	applied INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS incident_groups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL DEFAULT '',
	key TEXT NOT NULL DEFAULT '',
	label TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_key ON incident_groups(kind, key);

CREATE TABLE IF NOT EXISTS incident_group_members (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	group_id INTEGER NOT NULL,
	incident_id INTEGER NOT NULL,
	added_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_group_members_uniq ON incident_group_members(group_id, incident_id);
CREATE INDEX IF NOT EXISTS idx_group_members_incident ON incident_group_members(incident_id);

CREATE TABLE IF NOT EXISTS incident_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_incident ON incident_events(incident_id);
CREATE INDEX IF NOT EXISTS idx_events_incident_kind ON incident_events(incident_id, kind);

CREATE TABLE IF NOT EXISTS retrospectives (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	window_start TEXT NOT NULL,
	window_end TEXT NOT NULL,
	incidents_reviewed INTEGER NOT NULL DEFAULT 0,
	summary TEXT NOT NULL DEFAULT '',
	memories_created INTEGER NOT NULL DEFAULT 0,
	instructions_created INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive migrations: safe to ignore "duplicate column" errors.
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN solution TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN mr_url TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN root_cause TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN confidence TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN resolved_via TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN resolved_at TEXT`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN tags_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN owner TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE incidents ADD COLUMN team TEXT NOT NULL DEFAULT ''`)

	// Optional FTS5 index over memories for ranked full-text recall. If the
	// driver does not support FTS5 the table creation fails and the store
	// silently falls back to LIKE-based search.
	if err := s.enableMemoryFTS(); err != nil {
		s.ftsMemories = false
	} else {
		s.ftsMemories = true
	}
	return nil
}

// enableMemoryFTS builds an external-content FTS5 table over memories with
// triggers to keep it in sync, and rebuilds it from the base table.
func (s *Store) enableMemoryFTS() error {
	statements := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(topic, content, tags, content='memories', content_rowid='id')`,
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, topic, content, tags) VALUES (new.id, new.topic, new.content, new.tags_json);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, topic, content, tags) VALUES ('delete', old.id, old.topic, old.content, old.tags_json);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, topic, content, tags) VALUES ('delete', old.id, old.topic, old.content, old.tags_json);
			INSERT INTO memories_fts(rowid, topic, content, tags) VALUES (new.id, new.topic, new.content, new.tags_json);
		END`,
		`INSERT INTO memories_fts(memories_fts) VALUES ('rebuild')`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("enable fts: %w", err)
		}
	}
	return nil
}

const timeFmt = time.RFC3339Nano

func now() string { return time.Now().UTC().Format(timeFmt) }

func timeParse(s string) time.Time {
	t, err := time.Parse(timeFmt, s)
	if err != nil && s != "" {
		log.Printf("store: malformed timestamp %q: %v", s, err)
	}
	return t
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
