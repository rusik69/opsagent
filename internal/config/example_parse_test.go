package config

import "testing"

func TestExampleFileParses(t *testing.T) {
	cfg, err := Load("../../config.yaml.example")
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.Maintenance.RepoSyncMinutes != 60 {
		t.Errorf("expected repos_sync_minutes 60, got %d", cfg.Maintenance.RepoSyncMinutes)
	}
	if cfg.Storage.RetentionDays != 90 {
		t.Errorf("expected retention_days 90, got %d", cfg.Storage.RetentionDays)
	}
	if cfg.LLM.MaxRetries != 3 || cfg.LLM.MaxTokens != 0 {
		t.Errorf("unexpected llm settings: %+v", cfg.LLM)
	}
}
