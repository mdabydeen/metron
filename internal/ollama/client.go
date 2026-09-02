package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type ToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Tool struct {
	Type     string `json:"type"`
	Function any    `json:"function"`
}

type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Tools    []Tool         `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}

type ChatResponse struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
}

// Options carries the per-request generation settings and the HTTP timeout.
type Options struct {
	Temperature float64
	TopP        float64
	NumCtx      int
	Timeout     time.Duration
}

// DefaultOptions matches metron's built-in configuration.
func DefaultOptions() Options {
	return Options{Temperature: 0.1, TopP: 0.95, NumCtx: 16384, Timeout: 180 * time.Second}
}

type Client struct {
	endpoint string
	model    string
	opts     Options
	http     *http.Client
}

func NewClient(endpoint, model string, opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultOptions().Timeout
	}
	return &Client{
		endpoint: endpoint,
		model:    model,
		opts:     opts,
		http:     &http.Client{Timeout: opts.Timeout},
	}
}

func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (*Message, error) {
	reqBody := ChatRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
		Options: map[string]any{
			"temperature": c.opts.Temperature,
			"top_p":       c.opts.TopP,
			"num_ctx":     c.opts.NumCtx,
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama error (status %d): %s", resp.StatusCode, string(b))
	}

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &chatResp.Message, nil
}
