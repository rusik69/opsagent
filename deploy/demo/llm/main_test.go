package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func decode(t *testing.T, m map[string]any) oaiResponse {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var r oaiResponse
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMockLLMPlansAndGroundedReport(t *testing.T) {
	prompt := "Incident #7 to diagnose:\n- host: web-01\n- severity: critical\n- title: high CPU\n- message: load 40\n- labels: map[]\n\nDiagnose this incident now."
	msgs := []chatMessage{{Role: "user", Content: prompt}}

	for step := 0; step < 8; step++ {
		r := decode(t, respond(msgs, step))
		if step < 7 {
			if r.Choices[0].FinishReason != "tool_calls" || len(r.Choices[0].Message.ToolCalls) != 1 {
				t.Fatalf("step %d: expected one tool call, got %+v", step, r.Choices[0])
			}
			fn := r.Choices[0].Message.ToolCalls[0].Function
			if fn.Name == "" {
				t.Fatalf("step %d: empty tool call", step)
			}
			// Host-carrying tools must reference the diagnosed host.
			switch fn.Name {
			case "run_command", "get_host_config":
				if !strings.Contains(fn.Arguments, "web-01") {
					t.Fatalf("step %d: tool call %s missing host: %s", step, fn.Name, fn.Arguments)
				}
			}
		}
		// Simulate the tool result the agent would append.
		msgs = append(msgs, chatMessage{Role: "tool", Content: "$ uptime\n load average: 40.0, 39.0, 38.0\n[exit: success, 12ms]"})
	}

	r := decode(t, respond(msgs, 8))
	if r.Choices[0].FinishReason != "stop" || r.Choices[0].Message.Content == "" {
		t.Fatalf("expected a final report, got %+v", r.Choices[0])
	}
	if !strings.Contains(r.Choices[0].Message.Content, "load average: 40.0") {
		t.Fatalf("expected report grounded in tool evidence:\n%s", r.Choices[0].Message.Content)
	}
	if !strings.Contains(r.Choices[0].Message.Content, "MR proposing the config fix") {
		t.Fatalf("expected report to mention the MR:\n%s", r.Choices[0].Message.Content)
	}
}

func TestMockLLMDoesNotRestartOnReflection(t *testing.T) {
	// The reflection call after a diagnosis re-sends history plus a reflection
	// prompt; the last user message no longer contains the incident prompt, so
	// the script must not restart.
	history := []chatMessage{
		{Role: "user", Content: "Incident #1 to diagnose:\n- host: web-01\n\nDiagnose this incident now."},
		{Role: "tool", Content: "$ uptime\n load average: 1.0"},
	}
	msgs := append(history, chatMessage{Role: "user", Content: "Reflect on this diagnosis. If there is a clear lesson call store_memory. Otherwise reply: none."})
	r := decode(t, respond(msgs, 8))
	if r.Choices[0].Message.Content == "" {
		t.Fatalf("expected a short non-restart answer, got %+v", r.Choices[0])
	}
	if r.Choices[0].Message.Content != "" && r.Choices[0].FinishReason != "stop" {
		t.Fatalf("expected stop finish, got %+v", r.Choices[0])
	}
}
