// Command mockllm is a scripted OpenAI-compatible chat completions server used
// by the k8s demo. It simulates an agent that: runs uptime over SSH, reads the
// host config from the repos, searches the docs, and then writes a diagnosis
// report. It is intentionally not a real model.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
)

type chatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

var (
	mu           sync.Mutex
	step         int
	hadDiagnosis bool
)

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
	// A fresh "Diagnose this incident" user message restarts the script.
	newDiagnosis := false
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "Diagnose this incident") {
			newDiagnosis = true
		}
	}

	mu.Lock()
	if newDiagnosis {
		step = 0
		hadDiagnosis = true
	}
	cur := step
	step++
	mu.Unlock()

	resp := scripted(cur, newDiagnosis && !hadDiagnosis)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func scripted(step int, start bool) map[string]any {
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	switch {
	case step == 0:
		msg["tool_calls"] = []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "run_command", "arguments": `{"host":"web-01","command_id":"uptime"}`}},
		}
		finish = "tool_calls"
	case step == 1:
		msg["tool_calls"] = []any{
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "get_host_config", "arguments": `{"host":"web-01"}`}},
		}
		finish = "tool_calls"
	case step == 2:
		msg["tool_calls"] = []any{
			map[string]any{"id": "c3", "type": "function", "function": map[string]any{"name": "search_docs", "arguments": `{"query":"nginx troubleshooting"}`}},
		}
		finish = "tool_calls"
	case step == 3:
		msg["content"] = `## Diagnosis

Root cause hypothesis: web-01 is under sustained load; the uptime output shows a high load average.

Evidence:
- uptime on web-01 reported an elevated load average.
- Host config from the ansible repo shows nginx serving port 8080.
- The docs repo recommends checking /var/log/nginx/error.log first.

Confidence: medium.

Recommended next steps (read-only suggestions for a human operator):
- Inspect /var/log/nginx/error.log on web-01.
- Check which processes contribute to load with top -bn1.

No merge request was created in this demo.`
	default:
		msg["content"] = "none"
	}
	return map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
	}
}

func main() {
	addr := ":8081"
	http.HandleFunc("/v1/chat/completions", handle)
	log.Printf("mockllm listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("mockllm: %v", err)
	}
}
