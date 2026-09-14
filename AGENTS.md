# opsagent — developer guide for AI agents

You are working on **opsagent**, a Go incident-diagnosis agent. It ingests
incidents over HTTP, diagnoses them with an LLM agent that drives an internal
MCP server (allowlisted read-only SSH commands, Puppet/Ansible/doc repos, GitLab
MRs), and self-improves by reviewing its own history.

## Build, test, verify

- Go 1.26.5, module `github.com/rusik69/opsagent`. No cgo (pure-Go SQLite).
- `make build` → `bin/opsagent`; `make test` → `go test ./...`;
  `make vet` → `go vet ./...`; `make fmt`.
- Always run before finishing: `gofmt -l .` (0 files), `go vet ./...`,
  `go test -race ./...`, `go build ./...`.
- Run locally: `./bin/opsagent -config config.yaml` (see `config.yaml.example`).
  With no SSH hosts the server uses a `FakeRunner`; with an unreachable LLM
  diagnoses record an `error` status cleanly.

## Package map

| Package | Responsibility |
|---|---|
| `internal/config` | YAML config + defaults |
| `internal/model` | Domain types + status/event/group constants |
| `internal/store` | SQLite (modernc.org/sqlite), migrations, all CRUD |
| `internal/sshx` | SSH client, read-only command allowlist + executor |
| `internal/repos` | Puppet/Ansible/docs repo clone/pull, search, host config |
| `internal/mcp` | MCP server (mark3labs/mcp-go): tools the agent calls |
| `internal/agent` | LLM agent loop, prompts, diagnosis pool |
| `internal/incidents` | Intake (generic + Alertmanager) + recurrence detection |
| `internal/gitlab` | GitLab REST client (branches, files, MRs, MR state) |
| `internal/correlate` | Incident correlation engine (host/alertname/label/rootcause/agent) |
| `internal/review` | Retrospective reviewer → memories + instructions |
| `internal/web` | HTTP server: web UI (Go templates + HTMX), JSON API |
| `cmd/opsagent` | Wiring + background jobs (sync, correlate, MR poll, review) |

## Data flow

Intake (`POST /api/v1/incidents`, `/alertmanager`) → `incidents.Service` stores
an incident (+ `created` event, recurrence check) → operator/auto triggers
`POST /incidents/{id}/diagnose` → `agent.Agent.Diagnose` runs the LLM loop:
system prompt (rules file + memory + instructions + host config) → LLM calls
MCP tools (`run_command` via allowlist, `get_host_config`, `search_repos`,
`search_docs`, `create_gitlab_mr`, `set_incident_outcome`, `correlate_incidents`,
`store_memory`, `store_instruction`) → diagnosis persisted with steps →
`correlate.Engine` groups it → `review.Reviewer` later mines completed
diagnoses for memories/instructions. GitLab merge polling marks incidents
`resolved_via=mr_merged`.

## Security invariants (never regress)

- **Commands**: the allowlist (`sshx.Allowlist.Render`) is the ONLY execution
  path. Params are regex-validated; shell metacharacters rejected everywhere;
  no PTY/shell. `sshx.Executor.Run` audits every run to `command_runs`.
- **File reads**: `repos.safeJoin` confines `read_repo_file` to the repo root
  (rejects `..`, absolute paths, symlink escapes).
- **MCP args**: tools must never panic on bad input — use `strArgs`,
  `intArgs`, `strSliceArgs`; return `resultErr` instead.
- **OpenAI args**: `tool_calls[].function.arguments` arrives as a JSON **string**
  (or object); use `agent.parseToolArgs`.
- **Mutation**: only `create_gitlab_mr` mutates anything outside memory/store.
  SSH stays read-only.
- **State races**: `agent.finish` uses `UpdateIncidentStatusIfActive` so a
  concurrent operator resolve/cancel isn't overwritten.

## Conventions & recipes

- **Add an MCP tool**: define it in `internal/mcp/tools_*.go` inside a
  `toolsX() []toolReg` slice, register it in `NewServer`, add a Deps field if it
  needs a new service, and add a test in `server_test.go` (including a
  missing-args no-panic case).
- **Add an allowlisted command**: add a `Command{ID, Description, Template,
  Params}` to `sshx.DefaultAllowlist()` (or the config `allowlist.commands`).
  Templates must not contain shell metacharacters.
- **Add a store table/column**: extend the `schema` string in
  `store.migrate()` (additive `ALTER TABLE` for columns) and write CRUD in a
  `store/*.go` file with tests.
- **Config**: add a struct in `config.go`, a default in `Default()`, and a
  `config.yaml.example` entry.
- **Diagnosis-visible data** (memory/instructions/repos/docs) flows through
  `agent.prompt.go`; the injected rules file is `agent-rules.md`
  (`agent.instructions_file`).
- **Templates**: pages are self-contained `{{define "page"}}...{{end}}` using
  shared `head`/`nav`/`foot` from `layout.html`; HTMX partials use
  `incident_root`/`incident_body` blocks.

## Testing

Every package has tests; run `go test -race ./...`. Fakes to reuse:
`sshx.FakeRunner` (host outputs), httptest fake LLM (see `agent_test.go`,
`review_test.go`), httptest fake GitLab (see `gitlab/client_test.go`,
`mcp/server_test.go`). The web suite builds a full `Server` and exercises the
API + pages. Add a test for every new bug fix.