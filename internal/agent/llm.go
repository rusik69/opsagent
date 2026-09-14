package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal OpenAI-compatible chat completions client.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// Tool is an OpenAI function tool definition.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID   string       `json:"id"`
	Type string       `json:"type"`
	Func FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"arguments"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []oaiTool `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

type oaiTool struct {
	Type     string         `json:"type"`
	Function map[string]any `json:"function"`
}

type chatResponse struct {
	ID      string   `json:"id"`
	Choices []choice `json:"choices"`
	Error   *oaiErr  `json:"error"`
}

type choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type oaiErr struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Completion performs one chat round-trip. The response message may contain
// tool calls for the caller to execute and echo back.
func (c *Client) Completion(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	var oaiTools []oaiTool
	for _, t := range tools {
		fn := map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		}
		oaiTools = append(oaiTools, oaiTool{Type: "function", Function: fn})
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.Model,
		Messages:    messages,
		Tools:       oaiTools,
		ToolChoice:  "auto",
		Temperature: 0.1,
		Stream:      false,
	})
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("llm http %d: %s", resp.StatusCode, truncate(string(data), 400))
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return Message{}, fmt.Errorf("llm response parse: %w", err)
	}
	if cr.Error != nil {
		return Message{}, fmt.Errorf("llm error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return Message{}, fmt.Errorf("llm returned no choices")
	}
	return cr.Choices[0].Message, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
