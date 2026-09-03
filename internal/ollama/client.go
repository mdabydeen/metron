package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mdabydeen/metron/internal/llm"
)

// ChatRequest is Ollama's wire format for /api/chat. It is deliberately not the
// agent's vocabulary: those types live in internal/llm, and this package
// translates at the edge.
type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []llm.Message  `json:"messages"`
	Tools    []llm.Tool     `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}

type ChatResponse struct {
	Message llm.Message `json:"message"`
	Done    bool        `json:"done"`
	// Ollama reports what the exchange actually cost. metron's whole premise is
	// bounding that number, so it is measured rather than discarded.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

type Client struct {
	endpoint string
	model    string
	opts     llm.Options
	http     *http.Client
}

func NewClient(endpoint, model string, opts llm.Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = llm.DefaultOptions().Timeout
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

func (c *Client) Chat(ctx context.Context, messages []llm.Message, tools []llm.Tool) (*llm.Reply, error) {
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
func (c *Client) readStream(body io.Reader, watchdog *time.Timer) (*llm.Reply, error) {
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

	return replyFrom(assembled.final, llm.Message{
		Role:      assembled.role,
		Content:   assembled.content.String(),
		ToolCalls: assembled.toolCalls,
	}), nil
}

// ollamaStream is the partial reply being assembled from chunks.
type ollamaStream struct {
	role      string
	content   strings.Builder
	toolCalls []llm.ToolCall
	final     ChatResponse
}

func replyFrom(resp ChatResponse, msg llm.Message) *llm.Reply {
	return &llm.Reply{
		Message: msg,
		Usage: llm.Usage{
			PromptTokens: resp.PromptEvalCount,
			GenTokens:    resp.EvalCount,
		},
	}
}
