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
	// MaxTokens optionally caps the completion length (0 = provider default).
	MaxTokens int
	// MaxRetries is the number of retries on transient (429/5xx/network) errors.
	MaxRetries int
	// HTTP has no timeout by default: callers supply a context deadline.
	HTTP *http.Client
	// retryBase is the base backoff between retries (overridable in tests).
	retryBase time.Duration
}

func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL:    strings.TrimSuffix(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		MaxRetries: 3,
		HTTP:       &http.Client{},
		retryBase:  500 * time.Millisecond,
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

// Usage reports token consumption for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []oaiTool `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

type oaiTool struct {
	Type     string         `json:"type"`
	Function map[string]any `json:"function"`
}

type chatResponse struct {
	ID      string   `json:"id"`
	Choices []choice `json:"choices"`
	Usage   Usage    `json:"usage"`
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

// Completion performs one chat round-trip, retrying transient failures. The
// response message may contain tool calls for the caller to execute and echo
// back. Usage is discarded; use CompletionWithUsage to observe it.
func (c *Client) Completion(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	msg, _, err := c.CompletionWithUsage(ctx, messages, tools)
	return msg, err
}

// CompletionWithUsage performs one chat round-trip and reports token usage.
func (c *Client) CompletionWithUsage(ctx context.Context, messages []Message, tools []Tool) (Message, Usage, error) {
	var oaiTools []oaiTool
	for _, t := range tools {
		oaiTools = append(oaiTools, oaiTool{Type: "function", Function: map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		}})
	}
	body, err := json.Marshal(chatRequest{
		Model:       c.Model,
		Messages:    messages,
		Tools:       oaiTools,
		ToolChoice:  "auto",
		Temperature: 0.1,
		MaxTokens:   c.MaxTokens,
		Stream:      false,
	})
	if err != nil {
		return Message{}, Usage{}, err
	}

	retries := c.MaxRetries
	if retries < 0 {
		retries = 0
	}
	for attempt := 0; ; attempt++ {
		msg, usage, retryable, err := c.doCompletion(ctx, body)
		if err == nil {
			return msg, usage, nil
		}
		if !retryable || attempt >= retries {
			return Message{}, Usage{}, err
		}
		select {
		case <-ctx.Done():
			return Message{}, Usage{}, ctx.Err()
		case <-time.After(c.backoff(attempt)):
		}
	}
}

// doCompletion performs a single HTTP attempt. It reports whether the failure
// is worth retrying (network error, 429, or 5xx).
func (c *Client) doCompletion(ctx context.Context, body []byte) (Message, Usage, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Message{}, Usage{}, true, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Message{}, Usage{}, true, err
	}
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return Message{}, Usage{}, retryable, fmt.Errorf("llm http %d: %s", resp.StatusCode, truncate(string(data), 400))
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return Message{}, Usage{}, false, fmt.Errorf("llm response parse: %w", err)
	}
	if cr.Error != nil {
		return Message{}, Usage{}, false, fmt.Errorf("llm error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return Message{}, Usage{}, false, fmt.Errorf("llm returned no choices")
	}
	return cr.Choices[0].Message, cr.Usage, false, nil
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.retryBase
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	d := base << uint(attempt)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
