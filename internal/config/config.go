package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Storage   StorageConfig   `yaml:"storage"`
	SSH       SSHConfig       `yaml:"ssh"`
	Allowlist AllowlistConfig `yaml:"allowlist"`
	Repos     []RepoConfig    `yaml:"repos"`
	LLM       LLMConfig       `yaml:"llm"`
	GitLab    GitLabConfig    `yaml:"gitlab"`
	Agent     AgentConfig     `yaml:"agent"`
	Correlate CorrelateConfig `yaml:"correlate"`
	Review    ReviewConfig    `yaml:"review"`
	MCP       MCPConfig       `yaml:"mcp"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
	APIKey string `yaml:"api_key"`
}

type StorageConfig struct {
	Path string `yaml:"path"`
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
	Enabled     bool   `yaml:"enabled"`
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
		Storage: StorageConfig{Path: "./data/opsagent.db"},
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
			Enabled:     true,
		},
		Agent: AgentConfig{InstructionsFile: "agent-rules.md", MaxConcurrent: 4},
		Correlate: CorrelateConfig{
			Enabled: true, WindowMinutes: 120, IntervalMinutes: 15,
			Methods: []string{"host", "alertname", "label", "rootcause"},
		},
		Review: ReviewConfig{Enabled: true, IntervalHours: 6, Limit: 50, MinIncidents: 3, MaxSummaryLen: 4000},
		MCP:    MCPConfig{ExposeHTTP: true},
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
	return cfg, nil
}
