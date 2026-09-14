package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Server.Listen != ":8080" {
		t.Errorf("expected default listen :8080, got %q", cfg.Server.Listen)
	}
	if !cfg.LLM.Enabled {
		t.Error("expected LLM enabled by default")
	}
	if cfg.Agent.InstructionsFile != "agent-rules.md" {
		t.Errorf("expected default instructions file agent-rules.md, got %q", cfg.Agent.InstructionsFile)
	}
	if cfg.MCP.ExposeHTTP != true {
		t.Error("expected MCP exposed over HTTP by default")
	}
	if cfg.Correlate.Enabled != true || cfg.Correlate.WindowMinutes != 120 {
		t.Errorf("unexpected correlate defaults: %+v", cfg.Correlate)
	}
	if cfg.Review.Enabled != true || cfg.Review.IntervalHours != 6 || cfg.Review.MinIncidents != 3 {
		t.Errorf("unexpected review defaults: %+v", cfg.Review)
	}
	if cfg.SSH.TimeoutSeconds == 0 {
		t.Error("expected a default SSH timeout")
	}
}

func TestLoadParsesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  listen: ":9000"
  api_key: "sekret"
storage:
  path: "/tmp/x.db"
ssh:
  timeout_seconds: 5
  use_agent: false
  private_key: "/tmp/key"
  hosts:
    - {name: web-01, address: 10.0.0.1, user: ops, port: 22}
allowlist:
  commands:
    - id: custom
      description: custom cmd
      template: "mycmd"
      params: {x: "[a-z]+"}
repos:
  - {name: r1, type: ansible, path: /tmp/r1}
gitlab:
  base_url: "https://gitlab.example.com"
  token: "tok"
  default_project_id: "acme/infra"
  target_branch: "master"
agent:
  instructions_file: "/tmp/rules.md"
mcp:
  expose_http: false
llm:
  enabled: false
  model: "gpt-x"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":9000" || cfg.Server.APIKey != "sekret" {
		t.Errorf("server config not parsed: %+v", cfg.Server)
	}
	if cfg.Storage.Path != "/tmp/x.db" {
		t.Errorf("storage path: %q", cfg.Storage.Path)
	}
	if len(cfg.SSH.Hosts) != 1 || cfg.SSH.Hosts[0].Name != "web-01" {
		t.Errorf("hosts not parsed: %+v", cfg.SSH.Hosts)
	}
	if len(cfg.Allowlist.Commands) != 1 || cfg.Allowlist.Commands[0].ID != "custom" {
		t.Errorf("allowlist not parsed: %+v", cfg.Allowlist.Commands)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Type != "ansible" {
		t.Errorf("repos not parsed: %+v", cfg.Repos)
	}
	if cfg.GitLab.BaseURL != "https://gitlab.example.com" || cfg.GitLab.TargetBranch != "master" {
		t.Errorf("gitlab not parsed: %+v", cfg.GitLab)
	}
	if cfg.Agent.InstructionsFile != "/tmp/rules.md" {
		t.Errorf("agent config not parsed: %+v", cfg.Agent)
	}
	if cfg.MCP.ExposeHTTP {
		t.Error("expected expose_http false")
	}
	if cfg.LLM.Enabled {
		t.Error("expected llm disabled")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadEmptyPath(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected default config for empty path")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("server: [unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error for invalid yaml")
	}
}
