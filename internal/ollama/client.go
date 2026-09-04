package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type ToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolName identifies which tool a role:"tool" message answers. Without it
	// a reply carrying several tool calls comes back as N anonymous results in
	// arrival order, leaving the model to guess the pairing -- exactly the
	// guessing the system prompt forbids.
	ToolName  string     `json:"tool_name,omitempty"`
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
	// Ollama reports what the exchange actually cost. metron's whole premise is
	// bounding that number, so it is measured rather than discarded.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// Usage is the token cost of one model call.
type Usage struct {
	PromptTokens int // tokens the request occupied, history included
	GenTokens    int // tokens the model produced
}

// Add accumulates the cost of another call.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.GenTokens += other.GenTokens
}

// Reply pairs the model's message with what it cost to obtain.
type Reply struct {
	Message
	Usage Usage
}

// Options carries the per-request generation settings, the idle timeout and
// where streamed content is echoed.
type Options struct {
	Temperature float64
	TopP        float64
	NumCtx      int

	// Timeout bounds silence, not total generation. A total deadline is the
	// wrong shape for a local model: a big one legitimately spends minutes on a
	// reply, and cutting it off mid-thought is the failure this avoids.
	Timeout time.Duration

	// Stream requests incremental output. When false the client waits for the
	// whole reply, which is what a scripted caller wants.
	Stream bool

	// Sink receives content as it arrives while streaming. nil discards it --
	// the assembled reply is returned either way.
	Sink io.Writer
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

// ModelInfo is the small part of Ollama's model metadata that metron needs to
// decide whether a configured model can drive tools.
type ModelInfo struct {
	Capabilities []string `json:"capabilities"`
}

// Probe verifies that the configured Ollama endpoint is reachable, the model
// exists, and the server can return its capabilities without running an
// inference. It powers the CLI's fast, side-effect-free --doctor check.
func (c *Client) Probe(ctx context.Context) (ModelInfo, error) {
	endpoint, err := showEndpoint(c.endpoint)
	if err != nil {
		return ModelInfo{}, err
	}
	var payload bytes.Buffer
	_ = json.NewEncoder(&payload).Encode(map[string]string{"model": c.model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &payload)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("create model probe: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("contact Ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return ModelInfo{}, fmt.Errorf("ollama model check failed (status %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info ModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ModelInfo{}, fmt.Errorf("decode Ollama model details: %w", err)
	}
	return info, nil
}

// Supports reports whether Ollama advertised a named model capability.
func (m ModelInfo) Supports(capability string) bool {
	return slices.Contains(m.Capabilities, capability)
}

func showEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid Ollama endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" || !strings.HasSuffix(u.Path, "/api/chat") {
		return "", fmt.Errorf("invalid Ollama chat endpoint %q (want http(s)://host/api/chat)", endpoint)
	}
	u.Path = strings.TrimSuffix(u.Path, "/api/chat") + "/api/show"
	u.RawPath = ""
	return u.String(), nil
}

func NewClient(endpoint, model string, opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultOptions().Timeout
	}
	return &Client{
		endpoint: endpoint,
		model:    model,
		opts:     opts,
		// No client-level timeout: it would cap the whole exchange. The idle
		// watchdog in Chat cancels a stalled request instead.
		http: &http.Client{},
	}
}

func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (*Reply, error) {
	reqBody := ChatRequest{
		Model:    c.model,
		Messages: messages,
		Tools:    tools,
		Stream:   c.opts.Stream,
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

	// The watchdog fires only if nothing has arrived for the whole timeout, so
	// a long-but-progressing generation is never cut off.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(c.opts.Timeout, cancel)
	defer watchdog.Stop()

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

	if c.opts.Stream {
		return c.readStream(resp.Body, watchdog)
	}

	var chatResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return replyFrom(chatResp, chatResp.Message), nil
}

// readStream assembles Ollama's NDJSON chunks into one reply, echoing content
// to the sink as it arrives and resetting the idle watchdog on every chunk.
func (c *Client) readStream(body io.Reader, watchdog *time.Timer) (*Reply, error) {
	var (
		assembled ollamaStream
		dec       = json.NewDecoder(body)
	)
	for {
		var chunk ChatResponse
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode stream: %w", err)
		}
		watchdog.Reset(c.opts.Timeout)

		if chunk.Message.Content != "" {
			assembled.content.WriteString(chunk.Message.Content)
			if c.opts.Sink != nil {
				fmt.Fprint(c.opts.Sink, chunk.Message.Content)
			}
		}
		assembled.toolCalls = append(assembled.toolCalls, chunk.Message.ToolCalls...)
		if chunk.Message.Role != "" {
			assembled.role = chunk.Message.Role
		}
		if chunk.Done {
			assembled.final = chunk
			break
		}
	}

	return replyFrom(assembled.final, Message{
		Role:      assembled.role,
		Content:   assembled.content.String(),
		ToolCalls: assembled.toolCalls,
	}), nil
}

// ollamaStream is the partial reply being assembled from chunks.
type ollamaStream struct {
	role      string
	content   strings.Builder
	toolCalls []ToolCall
	final     ChatResponse
}

func replyFrom(resp ChatResponse, msg Message) *Reply {
	return &Reply{
		Message: msg,
		Usage: Usage{
			PromptTokens: resp.PromptEvalCount,
			GenTokens:    resp.EvalCount,
		},
	}
}
