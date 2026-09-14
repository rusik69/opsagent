# opsagent

An incident-diagnosis agent written in Go. It accepts incoming incidents over
an HTTP API, diagnoses them by driving an internal **MCP server** that runs a
**limited, read-only SSH command set** on the affected hosts, cross-references
**Puppet/Ansible configuration repos** to understand how the host is supposed
to be configured, and keeps a **memory + self-improvement instruction** store.

Everything is **read-only** for now: the only commands that can ever run on a
host are allowlisted templates, and no command may modify the host.

## How it works

```
                      ┌─────────────────────────── opsagent ───────────────────────────┐
 Prometheus/API ───►  │ POST /api/v1/incidents        store ──► LLM agent (diagnose)   │
 Alertmanager ────►  │ POST /api/v1/incidents/alertmanager         │                  │
                      │                                             ▼                  │
                      │                                    internal MCP server         │
                      │   run_command (allowlist, RO) ◄─────┼───── tools/list + call   │
                      │   get_host_config / search_repos    │      (also at /mcp HTTP) │
                      │   recall/store memory, instructions │                          │
                      │        │                             │                          │
                      │        ▼                             ▼                          │
                      │   SSH (agent/key) ─────────►  hosts   Puppet/Ansible repos      │
                      └─────────────────────────────────────────────────────────────────┘
```

## Components

- **Incident intake** (`internal/incidents`) — generic JSON webhook and a
  Prometheus Alertmanager-compatible endpoint.
- **SSH allowlist** (`internal/sshx`) — commands are templates with validated
  params. Shell metacharacters are rejected everywhere; commands run without a
  PTY and without a shell. Every run is persisted to an audit table.
- **MCP server** (`internal/mcp`, built on `mark3labs/mcp-go`) — exposes the
  tools the agent uses. Served in-process for the agent and over streamable
  HTTP at `/mcp` for external MCP clients (e.g. opencode).
- **Agent** (`internal/agent`) — calls an OpenAI-compatible LLM which decides
  which allowlisted commands to run and which config to read, then writes a
  structured diagnosis report. At the end it reflects and may store a memory
  or a self-improvement instruction.
- **Repos** (`internal/repos`) — clones/pulls Puppet and Ansible repos (or uses
  local checkouts) and provides host-config lookups and file:line search.
- **Web UI** (`internal/web`) — Go templates + HTMX, embedded in the binary.
- **Correlation** (`internal/correlate`) — groups related incidents by host,
  alertname, label and root cause within a time window, plus agent-driven links.
- **History & self-improvement** (`internal/review`) — every incident keeps an
  append-only event timeline; a periodic reviewer mines completed incidents and
  turns lessons into memories + instructions that steer future diagnoses.
  GitLab MR-merge polling feeds back whether fixes actually worked.

## MCP tools

| Tool | Purpose |
|---|---|
| `list_hosts` | Hosts that can be diagnosed |
| `list_commands` | The limited read-only command allowlist |
| `run_command` | Run an allowlisted read-only command (host, command_id, params) |
| `list_repos` / `sync_repos` | Config repos |
| `get_host_config` | Config for a host from Puppet/Ansible repos |
| `search_repos` / `read_repo_file` | Search and read config |
| `list_docs` / `search_docs` | Documentation repos |
| `create_gitlab_mr` | Open a GitLab MR fixing the problem (branch + commit + MR) |
| `get_incident` / `list_incidents` | Incident context |
| `get_related_incidents` / `correlate_incidents` | Incident correlation |
| `set_incident_outcome` | Record root cause / confidence / resolved-via |
| `get_incident_history` / `list_retrospectives` | Incident timeline + past reviews |
| `recall_memory` / `store_memory` | Persistent agent memory |
| `store_instruction` / `list_instructions` | Self-improvement instructions |

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/incidents` | Create an incident (JSON or form) |
| POST | `/api/v1/incidents/alertmanager` | Alertmanager webhook |
| GET | `/api/v1/incidents?status=&severity=&q=` | List incidents |
| GET | `/api/v1/incidents/{id}` | Incident detail |
| POST | `/api/v1/incidents/{id}/diagnose` | Start agent diagnosis |
| POST | `/api/v1/incidents/{id}/cancel` | Cancel running diagnosis |
| POST | `/api/v1/incidents/{id}/status` | Transition status (e.g. resolved) |
| GET | `/api/v1/incidents/{id}/diagnosis` | Diagnosis report + command runs |
| GET | `/api/v1/incidents/{id}/related` | Correlated incidents + groups |
| GET | `/api/v1/incidents/{id}/events` | Incident timeline |
| GET | `/api/v1/groups` / `/api/v1/groups/{id}` | Correlation groups |
| POST | `/api/v1/correlate/run` | Run correlation now |
| GET | `/api/v1/retrospectives` | Past self-improvement reviews |
| POST | `/api/v1/review/run` | Run a review now |
| GET | `/api/v1/hosts` | Configured hosts |
| GET | `/api/v1/hosts/{host}/commands` | Allowlisted commands |
| POST | `/api/v1/hosts/{host}/commands/run` | Manually run a read-only command |
| GET/POST | `/api/v1/repos`, `/api/v1/repos/sync`, `/api/v1/repos/search?q=` | Repos |
| GET/POST | `/api/v1/memory`, `/api/v1/instructions` | Memory + instructions |
| GET/POST | `/mcp` | MCP server (streamable HTTP) |
| GET | `/healthz` | Liveness |

## Configuration

Copy `config.yaml.example` to `config.yaml` and adjust: SSH hosts, Puppet/Ansible
repos, docs repos (`type: docs`), the OpenAI-compatible LLM endpoint, and
GitLab (`base_url`, `token`, `default_project_id`) for MR creation. The
allowlist section adds extra read-only commands on top of the built-in set.

### Agent rules file (`agent-rules.md`)

`agent.instructions_file` (default `agent-rules.md`) points to a rules file that is
read on every diagnosis and injected verbatim into the LLM system prompt
alongside the repos/docs layout. It defines the agent's constraints, the paths
to the config and documentation repos, and the expected report format. Editing
it changes behaviour without a restart. See the bundled `agent-rules.md`.
(`AGENTS.md` at the repo root is the developer guide for AI agents working on
this codebase.)

## Build & run

```sh
make build          # builds bin/opsagent
./bin/opsagent -config config.yaml
```

Then:

```sh
curl -X POST localhost:8080/api/v1/incidents \
  -H 'content-type: application/json' \
  -d '{"host":"web-01","title":"high CPU","severity":"critical","message":"load 40 on 4 cores"}'
curl -X POST localhost:8080/api/v1/incidents/1/diagnose
```

Open http://localhost:8080 for the dashboard. The incidents list shows the
investigation status of each alert, a Details button, and the MR link /
solution summary once one exists.

## Security notes

- Host commands are read-only only: the allowlist is the sole execution path;
  params are regex validated and shell metacharacters are rejected.
- The only mutating operation is `create_gitlab_mr` (branch + commit + MR),
  gated by GitLab config. The agent is instructed to never commit secrets and
  to only propose fixes tied to the incident. MR links and the proposed
  solution are recorded on the incident for review.
- SSH uses ssh-agent or a private key; no passwords. Known-hosts verification
  is strict unless `known_hosts: insecure` is set (dev only).
- Optional `X-API-Key` on the server config protects all endpoints.