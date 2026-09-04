// Package openai speaks the OpenAI chat-completions API.
//
// One wire format reaches llama.cpp's server, LM Studio, vLLM, OpenRouter, and
// Ollama's own compatibility endpoint -- so supporting it is what makes metron
// usable with whatever the operator already runs, rather than only with Ollama.
package openai

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

	"github.com/mdabydeen/metron/llm"
)

// chatRequest is the wire format. It is not the agent's vocabulary: that lives
// in internal/llm, and this package translates at the edge.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []llm.Tool    `json:"tools,omitempty"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature"`
	TopP        float64       `json:"top_p"`
	// StreamOptions asks for a usage block on the final streamed chunk. Without
	// it most servers report no token counts at all while streaming, and this
	// program exists to measure exactly that number.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// wireMessage differs from llm.Message in two ways that matter: tool results
// carry a tool_call_id rather than a name, and arguments are a JSON *string*
// rather than an object.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name string `json:"name,omitempty"`
		// Arguments is a string containing JSON, not JSON. While streaming it
		// arrives in fragments that have to be concatenated before they parse.
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message wireMessage `json:"message"`
		Delta   wireMessage `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	endpoint string
	model    string
	opts     llm.Options
	http     *http.Client
}

// NewClient builds a client. The endpoint is the full chat-completions URL.
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
	body := chatRequest{
		Model:       c.model,
		Messages:    toWire(messages),
		Tools:       tools,
		Stream:      c.opts.Stream,
		Temperature: c.opts.Temperature,
		TopP:        c.opts.TopP,
	}
	if c.opts.Stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	// Marshalling cannot fail here: every field is a plain type, and the tool
	// schemas and arguments arrived as JSON in the first place.
	payload, _ := json.Marshal(body)

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
	if c.opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		return nil, fmt.Errorf("openai error (status %d): %s", resp.StatusCode, string(b))
	}

	// Bounded before anything reads it. The idle watchdog resets on every chunk,
	// so a server that streams steadily is never idle and never cancelled --
	// which without a size limit is an out-of-memory condition a remote endpoint
	// can trigger at will.
	bounded := io.LimitReader(resp.Body, maxResponseBytes)
	if c.opts.Stream {
		return c.readStream(bounded, watchdog)
	}

	var parsed chatResponse
	if err := json.NewDecoder(bounded).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("openai error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("openai returned no choices")
	}
	msg := parsed.Choices[0].Message
	calls, err := fromWireCalls(msg.ToolCalls)
	if err != nil {
		return nil, err
	}
	return replyFrom(parsed, llm.Message{Role: msg.Role, Content: msg.Content, ToolCalls: calls}), nil
}

// Limits on what a server may send. metron's whole premise is that the context
// window is bounded; a reply that cannot be bounded is a reply that defeats it,
// and a remote endpoint is not necessarily one the operator controls.
const (
	maxResponseBytes = 32 << 20 // one whole response
	maxContentBytes  = 8 << 20  // assembled reply text
	maxStreamCalls   = 64       // distinct tool-call indices in one reply
	maxErrorBytes    = 8 << 10  // a server's own error message
)

// readStream assembles a server-sent-events stream into one reply.
//
// This is the part that differs most from Ollama. Content arrives in deltas,
// which is easy; tool calls arrive as fragments keyed by index, where the name
// appears once and the arguments are a JSON string split across any number of
// chunks. Reassembling by index -- rather than by arrival order -- is what makes
// a reply carrying two tool calls come out as two calls rather than one
// corrupt one.
func (c *Client) readStream(body io.Reader, watchdog *time.Timer) (*llm.Reply, error) {
	var (
		content strings.Builder
		role    string
		partial = map[int]*wireToolCall{}
		order   []int
		final   chatResponse
		scanner = newEventScanner(body)
	)
	for {
		data, ok := scanner.next()
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		watchdog.Reset(c.opts.Timeout)

		var chunk chatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode stream: %w", err)
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("openai error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			final.Usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Role != "" {
			role = delta.Role
		}
		if delta.Content != "" {
			if content.Len()+len(delta.Content) > maxContentBytes {
				return nil, fmt.Errorf("reply exceeded %d bytes", maxContentBytes)
			}
			content.WriteString(delta.Content)
			if c.opts.Sink != nil {
				fmt.Fprint(c.opts.Sink, delta.Content)
			}
		}
		for _, frag := range delta.ToolCalls {
			idx := 0
			if frag.Index != nil {
				idx = *frag.Index
			}
			existing, seen := partial[idx]
			if !seen {
				// A real reply carries a handful of calls. An index the server
				// picks is otherwise an unbounded map key.
				if len(partial) >= maxStreamCalls {
					return nil, fmt.Errorf("reply carried more than %d tool calls", maxStreamCalls)
				}
				existing = &wireToolCall{}
				partial[idx] = existing
				order = append(order, idx)
			}
			if frag.Function.Name != "" {
				existing.Function.Name = frag.Function.Name
			}
			if len(existing.Function.Arguments)+len(frag.Function.Arguments) > maxContentBytes {
				return nil, fmt.Errorf("tool call arguments exceeded %d bytes", maxContentBytes)
			}
			existing.Function.Arguments += frag.Function.Arguments
		}
	}
	if err := scanner.err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	assembled := make([]wireToolCall, 0, len(order))
	for _, idx := range order {
		assembled = append(assembled, *partial[idx])
	}
	calls, err := fromWireCalls(assembled)
	if err != nil {
		return nil, err
	}
	return replyFrom(final, llm.Message{Role: role, Content: content.String(), ToolCalls: calls}), nil
}

// toWire converts the agent's messages to the wire shape. Tool results need a
// tool_call_id, and metron does not carry the server's ids around, so the tool
// name is reused as one: servers echo it back and it keeps the pairing the
// agent depends on.
func toWire(messages []llm.Message) []wireMessage {
	out := make([]wireMessage, 0, len(messages))
	for _, m := range messages {
		w := wireMessage{Role: m.Role, Content: m.Content}
		if m.Role == "tool" {
			w.ToolCallID = m.ToolName
		}
		for i, call := range m.ToolCalls {
			// Arguments arrived from the model as JSON, so they round-trip.
			args, _ := json.Marshal(call.Function.Arguments)
			wc := wireToolCall{ID: call.Function.Name, Type: "function", Index: intPtr(i)}
			wc.Function.Name = call.Function.Name
			wc.Function.Arguments = string(args)
			w.ToolCalls = append(w.ToolCalls, wc)
		}
		out = append(out, w)
	}
	return out
}

func intPtr(i int) *int { return &i }

// fromWireCalls parses the arguments string of each call into the object the
// agent's dispatch expects.
func fromWireCalls(calls []wireToolCall) ([]llm.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]llm.ToolCall, 0, len(calls))
	for _, c := range calls {
		if c.Function.Name == "" {
			continue
		}
		var call llm.ToolCall
		call.Function.Name = c.Function.Name
		call.Function.Arguments = map[string]any{}
		if args := strings.TrimSpace(c.Function.Arguments); args != "" {
			if err := json.Unmarshal([]byte(args), &call.Function.Arguments); err != nil {
				return nil, fmt.Errorf("parse arguments for %s: %w", c.Function.Name, err)
			}
		}
		out = append(out, call)
	}
	return out, nil
}

// replyFrom attaches the usage counts. Servers that report none leave the
// counts at zero, which the REPL reads as "say nothing rather than 0" -- the
// same posture Ollama's client takes.
func replyFrom(resp chatResponse, msg llm.Message) *llm.Reply {
	var usage llm.Usage
	if resp.Usage != nil {
		usage = llm.Usage{
			PromptTokens: resp.Usage.PromptTokens,
			GenTokens:    resp.Usage.CompletionTokens,
		}
	}
	return &llm.Reply{Message: msg, Usage: usage}
}
