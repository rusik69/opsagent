package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Storage     StorageConfig     `yaml:"storage"`
	SSH         SSHConfig         `yaml:"ssh"`
	Allowlist   AllowlistConfig   `yaml:"allowlist"`
	Repos       []RepoConfig      `yaml:"repos"`
	LLM         LLMConfig         `yaml:"llm"`
	GitLab      GitLabConfig      `yaml:"gitlab"`
	Agent       AgentConfig       `yaml:"agent"`
	Correlate   CorrelateConfig   `yaml:"correlate"`
	Review      ReviewConfig      `yaml:"review"`
	MCP         MCPConfig         `yaml:"mcp"`
	Maintenance MaintenanceConfig `yaml:"maintenance"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
	// APIKey gates every endpoint. APIKeyReadOnly additionally allows GET
	// requests (read-only), and APIKeyWebhook additionally allows incident
	// intake webhooks (POST /api/v1/incidents and /alertmanager).
	APIKey         string `yaml:"api_key"`
	APIKeyReadOnly string `yaml:"api_key_readonly"`
	APIKeyWebhook  string `yaml:"api_key_webhook"`
}

type StorageConfig struct {
	Path string `yaml:"path"`
	// RetentionDays prunes old command runs and events (0 disables pruning).
	RetentionDays int `yaml:"retention_days"`
}

type MaintenanceConfig struct {
	// RepoSyncMinutes periodically pulls the configured git repos
	// (0 disables the periodic sync; the initial boot sync still runs).
	RepoSyncMinutes int `yaml:"repos_sync_minutes"`
}

type SSHConfig struct {
	TimeoutSeconds int          `yaml:"timeout_seconds"`
	UseAgent       bool         `yaml:"use_agent"`
	PrivateKey     string       `yaml:"private_key"`
	KnownHosts     string       `yaml:"known_hosts"` // path or "insecure"
	Hosts          []HostConfig `yaml:"hosts"`
}

type HostConfig struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
	User    string `yaml:"user"`
	Port    int    `yaml:"port"`
}

type AllowlistConfig struct {
	Commands []CommandDef `yaml:"commands"`
}

type CommandDef struct {
	ID          string            `yaml:"id"`
	Description string            `yaml:"description"`
	Template    string            `yaml:"template"`
	Params      map[string]string `yaml:"params"` // param name -> regex pattern
}

type RepoConfig struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"` // "puppet", "ansible", "generic"
	URL    string `yaml:"url"`  // git url (optional if Path set)
	Path   string `yaml:"path"` // local path (optional if URL set)
	Branch string `yaml:"branch"`
	Ref    string `yaml:"ref"`
}

type LLMConfig struct {
	BaseURL     string `yaml:"base_url"`
	APIKey      string `yaml:"api_key"`
	Model       string `yaml:"model"`
	MaxSteps    int    `yaml:"max_steps"`
	TimeoutSecs int    `yaml:"timeout_secs"`
	// MaxRetries is the number of retries on transient LLM errors (429/5xx).
	MaxRetries int `yaml:"max_retries"`
	// MaxTokens optionally caps the completion length (0 = provider default).
	MaxTokens int  `yaml:"max_tokens"`
	Enabled   bool `yaml:"enabled"`
}

type GitLabConfig struct {
	BaseURL            string `yaml:"base_url"`
	Token              string `yaml:"token"`
	DefaultProjectID   string `yaml:"default_project_id"`
	SourceBranchPrefix string `yaml:"source_branch_prefix"`
	TargetBranch       string `yaml:"target_branch"`
	// PollMRSeconds controls how often agent-created MRs are checked for merge
	// (0 disables merge polling).
	PollMRSeconds int `yaml:"poll_mr_seconds"`
}

// AgentConfig controls the diagnosis agent behaviour.
type AgentConfig struct {
	// InstructionsFile is a file (e.g. AGENTS.md) with rules and paths that is
	// injected into every LLM request together with the repos/docs layout.
	InstructionsFile string `yaml:"instructions_file"`
	// MaxConcurrent bounds how many diagnoses may run at once across the whole
	// server. Excess requests return 429 instead of starting another LLM loop.
	MaxConcurrent int `yaml:"max_concurrent"`
}

// CorrelateConfig controls incident correlation.
type CorrelateConfig struct {
	Enabled         bool     `yaml:"enabled"`
	WindowMinutes   int      `yaml:"window_minutes"`
	IntervalMinutes int      `yaml:"interval_minutes"`
	Methods         []string `yaml:"methods"`
}

// ReviewConfig controls the self-improvement review loop.
type ReviewConfig struct {
	Enabled       bool `yaml:"enabled"`
	IntervalHours int  `yaml:"interval_hours"`
	Limit         int  `yaml:"limit"`
	MinIncidents  int  `yaml:"min_incidents"`
	MaxSummaryLen int  `yaml:"max_summary_len"`
}

type MCPConfig struct {
	ExposeHTTP bool `yaml:"expose_http"`
}

func Default() *Config {
	return &Config{
		Server:  ServerConfig{Listen: ":8080"},
		Storage: StorageConfig{Path: "./data/opsagent.db", RetentionDays: 90},
		SSH: SSHConfig{
			TimeoutSeconds: 15,
			UseAgent:       true,
			KnownHosts:     "insecure",
		},
		LLM: LLMConfig{
			BaseURL:     "http://localhost:11434/v1",
			Model:       "llama3.1",
			MaxSteps:    12,
			TimeoutSecs: 180,
			MaxRetries:  3,
			Enabled:     true,
		},
		Agent: AgentConfig{InstructionsFile: "agent-rules.md", MaxConcurrent: 4},
		Correlate: CorrelateConfig{
			Enabled: true, WindowMinutes: 120, IntervalMinutes: 15,
			Methods: []string{"host", "alertname", "label", "rootcause"},
		},
		Review:      ReviewConfig{Enabled: true, IntervalHours: 6, Limit: 50, MinIncidents: 3, MaxSummaryLen: 4000},
		MCP:         MCPConfig{ExposeHTTP: true},
		Maintenance: MaintenanceConfig{RepoSyncMinutes: 60},
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.expandSecrets()
	return cfg, nil
}

// expandSecrets resolves environment-variable references and file-backed
// secrets in sensitive fields. A value of the form  is replaced with the
// environment variable VAR, and a value prefixed with "file:" is read from the
// referenced file (trailing whitespace trimmed). This keeps tokens out of the
// config file itself.
func (c *Config) expandSecrets() {
	c.Server.APIKey = resolveSecret(c.Server.APIKey)
	c.Server.APIKeyReadOnly = resolveSecret(c.Server.APIKeyReadOnly)
	c.Server.APIKeyWebhook = resolveSecret(c.Server.APIKeyWebhook)
	c.LLM.APIKey = resolveSecret(c.LLM.APIKey)
	c.GitLab.Token = resolveSecret(c.GitLab.Token)
	c.SSH.PrivateKey = resolveSecret(c.SSH.PrivateKey)
}

func resolveSecret(s string) string {
	if s == "" {
		return s
	}
	if path, ok := strings.CutPrefix(s, "file:"); ok {
		data, err := os.ReadFile(strings.TrimSpace(path))
		if err != nil {
			return s
		}
		return strings.TrimSpace(string(data))
	}
	return os.ExpandEnv(s)
}
