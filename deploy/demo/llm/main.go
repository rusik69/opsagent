// Command mockllm is a scripted OpenAI-compatible chat completions server used
// by the k8s demo. It behaves like a small agent: it runs the same read-only
// commands a real model would, then writes a diagnosis report grounded in the
// actual tool outputs it received. It is deterministic and offline.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

var (
	mu   sync.Mutex
	step int
)

func main() {
	addr := flag.String("listen", ":8081", "HTTP listen address")
	flag.Parse()
	http.HandleFunc("/v1/chat/completions", handle)
	log.Printf("mockllm listening on %s", *addr)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("mockllm: %v", err)
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A fresh diagnosis is signalled by the very last message being the
	// incident prompt ("...Diagnose this incident now."). On later steps the
	// last message is a tool result, and after the report it is the reflection
	// prompt, so the script only restarts at the start of a diagnosis.
	if n := len(req.Messages); n > 0 {
		last := req.Messages[n-1]
		if last.Role == "user" && strings.Contains(last.Content, "Diagnose this incident now.") {
			mu.Lock()
			step = 0
			mu.Unlock()
		}
	}

	mu.Lock()
	cur := step
	step++
	mu.Unlock()

	log.Printf("chat request: step=%d last_role=%s", cur, req.Messages[len(req.Messages)-1].Role)
	resp := respond(req.Messages, cur)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func lastUser(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

var (
	incidentIDRe = regexp.MustCompile(`#(\d+)`)
	hostRe       = regexp.MustCompile(`- host: (\S+)`)
)

func planHost(prompt string) string {
	if m := hostRe.FindStringSubmatch(prompt); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return "web-01"
}

func incidentID(prompt string) int {
	if m := incidentIDRe.FindStringSubmatch(prompt); len(m) == 2 {
		var id int
		_, _ = fmtSscan(m[1], &id)
		return id
	}
	return 0
}

func respond(msgs []chatMessage, step int) map[string]any {
	prompt := lastUser(msgs)
	host := planHost(prompt)
	isDB := host == "db-01"

	msg := map[string]any{"role": "assistant"}
	finish := "stop"

	switch step {
	// --- Evidence gathering: run the same commands a real agent would. ---
	case 0:
		msg["tool_calls"] = []any{toolCall("c1", "run_command", jsonArgs(map[string]string{"host": host, "command_id": "uptime"}))}
		finish = "tool_calls"
	case 1:
		if isDB {
			msg["tool_calls"] = []any{toolCall("c2", "run_command", jsonArgs(map[string]any{"host": host, "command_id": "systemctl_status", "params": map[string]any{"service": "postgresql"}}))}
		} else {
			msg["tool_calls"] = []any{toolCall("c2", "run_command", jsonArgs(map[string]string{"host": host, "command_id": "nginx_conf_test"}))}
		}
		finish = "tool_calls"
	case 2:
		if isDB {
			msg["tool_calls"] = []any{toolCall("c3", "run_command", jsonArgs(map[string]any{"host": host, "command_id": "journalctl_service", "params": map[string]any{"service": "postgresql", "lines": "50"}}))}
		} else {
			msg["tool_calls"] = []any{toolCall("c3", "run_command", jsonArgs(map[string]any{"host": host, "command_id": "systemctl_status", "params": map[string]any{"service": "nginx"}}))}
		}
		finish = "tool_calls"
	case 3:
		if isDB {
			msg["tool_calls"] = []any{toolCall("c4", "run_command", jsonArgs(map[string]string{"host": host, "command_id": "ss_listening"}))}
		} else {
			msg["tool_calls"] = []any{toolCall("c4", "run_command", jsonArgs(map[string]any{"host": host, "command_id": "journalctl_service", "params": map[string]any{"service": "nginx", "lines": "50"}}))}
		}
		finish = "tool_calls"
	// --- Expected state from the config repos + docs. ---
	case 4:
		msg["tool_calls"] = []any{toolCall("c5", "get_host_config", jsonArgs(map[string]string{"host": host}))}
		finish = "tool_calls"
	case 5:
		if isDB {
			msg["tool_calls"] = []any{toolCall("c6", "search_docs", jsonArgs(map[string]string{"query": "postgres tuning"}))}
		} else {
			msg["tool_calls"] = []any{toolCall("c6", "search_docs", jsonArgs(map[string]string{"query": "nginx troubleshooting"}))}
		}
		finish = "tool_calls"
	// --- Record the outcome and propose a fix MR. ---
	case 6:
		id := incidentID(prompt)
		if isDB {
			msg["tool_calls"] = []any{
				toolCall("c7", "set_incident_outcome", jsonArgs(map[string]any{"incident_id": id, "root_cause": "postgres failed to start: could not create shared memory segment", "confidence": "high"})),
			}
		} else {
			msg["tool_calls"] = []any{
				toolCall("c7", "set_incident_outcome", jsonArgs(map[string]any{"incident_id": id, "root_cause": "nginx config test failed: unknown directive in /etc/nginx/conf.d/broken.conf", "confidence": "high"})),
			}
		}
		finish = "tool_calls"
	case 7:
		msg["tool_calls"] = []any{toolCall("c8", "create_gitlab_mr", gitLabArgs(host, isDB))}
		finish = "tool_calls"
	// --- Grounded final report. ---
	default:
		msg["content"] = report(msgs, host, isDB)
	}

	return map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 200, "completion_tokens": 80, "total_tokens": 280},
	}
}

// toolCall builds an OpenAI function tool_call.
func toolCall(id, name string, args string) map[string]any {
	return map[string]any{
		"id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": args},
	}
}

func jsonArgs(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// gitLabArgs builds the file change for the fix MR.
func gitLabArgs(host string, isDB bool) string {
	if isDB {
		return jsonArgs(map[string]any{
			"title":       "fix(postgres): raise shared memory and max_connections",
			"description": "Fix postgres startup failure on " + host + " by allowing a larger shared memory segment and sensible connection limits.",
			"files": []any{
				map[string]any{"path": "roles/postgres/templates/postgresql.conf", "content": "max_connections = 200\nshared_buffers = 256MB\nshared_memory_type = mmap\n"},
			},
		})
	}
	return jsonArgs(map[string]any{
		"title":       "fix(nginx): correct worker_processes and broken.conf on web-01",
		"description": "Remove the invalid directive in conf.d/broken.conf and align worker_processes with the expected config so nginx -t passes.",
		"files": []any{
			map[string]any{"path": "roles/nginx/templates/nginx.conf.j2", "content": "worker_processes 4;\n\nevents {\n    worker_connections 1024;\n}\n\nhttp {\n    include /etc/nginx/mime.types;\n    default_type application/octet-stream;\n    server {\n        listen 8080;\n        server_name web-01.example.com;\n        root /var/www/html;\n    }\n}\n"},
		},
	})
}

// report builds the final diagnosis from the tool outputs actually seen.
func report(msgs []chatMessage, host string, isDB bool) string {
	var sb strings.Builder
	if isDB {
		sb.WriteString("## Diagnosis\n\nRoot cause hypothesis: postgres failed to start on " + host + " - the shared memory segment could not be created, so the database service is down.\n\nEvidence gathered by read-only commands:\n")
	} else {
		sb.WriteString("## Diagnosis\n\nRoot cause hypothesis: nginx on " + host + " cannot start because `nginx -t` fails on an invalid directive in /etc/nginx/conf.d/broken.conf; the host is not serving the expected port 8080.\n\nEvidence gathered by read-only commands:\n")
	}
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		trimmed := strings.TrimSpace(m.Content)
		if trimmed == "" {
			continue
		}
		lines := strings.Split(trimmed, "\n")
		if len(lines) > 12 {
			lines = lines[:12]
		}
		sb.WriteString("- ")
		sb.WriteString(strings.Join(lines, "\n  "))
		sb.WriteString("\n")
	}
	if isDB {
		sb.WriteString("\nConfidence: high.\n\nRecommended next steps (read-only suggestions):\n- Raise shared_buffers / switch shared_memory_type to mmap on db-01.\n- Verify free -h and /dev/shm sizing before restarting postgres.\n")
	} else {
		sb.WriteString("\nConfidence: high.\n\nRecommended next steps (read-only suggestions):\n- Remove the invalid directive from /etc/nginx/conf.d/broken.conf.\n- Re-run `nginx -t`; when it passes, reload nginx.\n")
	}
	sb.WriteString("An MR proposing the config fix was created (see the incident record).")
	return sb.String()
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	*out = n
	return 1, nil
}
