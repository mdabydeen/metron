// Package openai talks to a local server that speaks the OpenAI chat
// completions wire format -- llama.cpp's server, vLLM, LM Studio, and
// similar -- so metron isn't tied to Ollama specifically. It produces the
// same metron/internal/ollama.Reply/Message/Usage types Ollama's client does;
// those stay the canonical shape agent and main work with, and only the wire
// encoding differs per package.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"metron/internal/ollama"
)

// Options mirrors ollama.Options: same fields, same meaning (an idle
// watchdog, not a total deadline; Stream and Sink work identically).
type Options struct {
	Temperature float64
	TopP        float64
	Timeout     time.Duration
	Stream      bool
	Sink        io.Writer
}

// DefaultOptions matches metron's built-in configuration.
func DefaultOptions() Options {
	return Options{Temperature: 0.1, TopP: 0.95, Timeout: 180 * time.Second}
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
		http:     &http.Client{},
	}
}

// Wire types for the OpenAI chat completions API. Unlike Ollama, tool call
// arguments travel as a JSON-encoded *string*, not a nested object, and every
// tool call carries an id that the answering message must echo back as
// tool_call_id -- both handled in the conversion helpers below.
type toolCallWire struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type messageWire struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []toolCallWire `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type toolWire struct {
	Type     string `json:"type"`
	Function any    `json:"function"`
}

type usageWire struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []messageWire  `json:"messages"`
	Tools         []toolWire     `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Temperature   float64        `json:"temperature"`
	TopP          float64        `json:"top_p"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatResponse struct {
	Choices []struct {
		Message messageWire `json:"message"`
	} `json:"choices"`
	Usage usageWire `json:"usage"`
}

// toWireMessages converts metron's canonical messages to the wire format.
// json.Marshal on a map[string]any always succeeds for the kind of data tool
// arguments actually hold (strings, numbers, bools, nested plain values), so
// the error here is unreachable in practice, not a real failure mode --
// mirrored below in toWireArguments.
func toWireMessages(msgs []ollama.Message) []messageWire {
	out := make([]messageWire, len(msgs))
	for i, m := range msgs {
		wire := messageWire{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wc := toolCallWire{ID: tc.ID, Type: "function"}
			wc.Function.Name = tc.Function.Name
			wc.Function.Arguments = toWireArguments(tc.Function.Arguments)
			wire.ToolCalls = append(wire.ToolCalls, wc)
		}
		out[i] = wire
	}
	return out
}

func toWireArguments(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func toWireTools(tools []ollama.Tool) []toolWire {
	out := make([]toolWire, len(tools))
	for i, t := range tools {
		out[i] = toolWire{Type: t.Type, Function: t.Function}
	}
	return out
}

// fromWireMessage converts one OpenAI-shaped message back to metron's
// canonical Message, decoding each tool call's JSON-string arguments into the
// map ollama.ToolCall expects.
func fromWireMessage(m messageWire) ollama.Message {
	out := ollama.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
	for _, wc := range m.ToolCalls {
		var tc ollama.ToolCall
		tc.ID = wc.ID
		tc.Function.Name = wc.Function.Name
		if wc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(wc.Function.Arguments), &tc.Function.Arguments)
		}
		out.ToolCalls = append(out.ToolCalls, tc)
	}
	return out
}

func (c *Client) Chat(ctx context.Context, messages []ollama.Message, tools []ollama.Tool) (*ollama.Reply, error) {
	reqBody := chatRequest{
		Model:       c.model,
		Messages:    toWireMessages(messages),
		Tools:       toWireTools(tools),
		Stream:      c.opts.Stream,
		Temperature: c.opts.Temperature,
		TopP:        c.opts.TopP,
	}
	if c.opts.Stream {
		reqBody.StreamOptions = &streamOptions{IncludeUsage: true}
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
	if c.opts.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai error (status %d): %s", resp.StatusCode, string(b))
	}

	if c.opts.Stream {
		return c.readStream(resp.Body, watchdog)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("openai response had no choices")
	}
	return &ollama.Reply{
		Message: fromWireMessage(chatResp.Choices[0].Message),
		Usage: ollama.Usage{
			PromptTokens: chatResp.Usage.PromptTokens,
			GenTokens:    chatResp.Usage.CompletionTokens,
		},
	}, nil
}

// streamChunk is one Server-Sent Events "data:" payload. Content and each
// tool call's arguments arrive as fragments; toolCallFragment.Index is the
// stable key across chunks that lets fragments for the same call be
// concatenated in order even when several calls stream interleaved.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role      string             `json:"role"`
			Content   string             `json:"content"`
			ToolCalls []toolCallFragment `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usageWire `json:"usage"`
}

type toolCallFragment struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// readStream assembles Server-Sent Events chunks into one reply, echoing
// content to the sink as it arrives and resetting the idle watchdog on every
// chunk.
func (c *Client) readStream(body io.Reader, watchdog *time.Timer) (*ollama.Reply, error) {
	var (
		role    string
		content strings.Builder
		calls   []*openCall // index-ordered, nil-padded
		usage   usageWire
		scanner = bufio.NewScanner(body)
	)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode stream: %w", err)
		}
		watchdog.Reset(c.opts.Timeout)

		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.Role != "" {
			role = delta.Role
		}
		if delta.Content != "" {
			content.WriteString(delta.Content)
			if c.opts.Sink != nil {
				fmt.Fprint(c.opts.Sink, delta.Content)
			}
		}
		for _, f := range delta.ToolCalls {
			for len(calls) <= f.Index {
				calls = append(calls, &openCall{})
			}
			oc := calls[f.Index]
			if f.ID != "" {
				oc.id = f.ID
			}
			if f.Function.Name != "" {
				oc.name = f.Function.Name
			}
			oc.args.WriteString(f.Function.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	msg := ollama.Message{Role: role, Content: content.String()}
	for _, oc := range calls {
		var tc ollama.ToolCall
		tc.ID = oc.id
		tc.Function.Name = oc.name
		if s := oc.args.String(); s != "" {
			_ = json.Unmarshal([]byte(s), &tc.Function.Arguments)
		}
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}

	return &ollama.Reply{
		Message: msg,
		Usage: ollama.Usage{
			PromptTokens: usage.PromptTokens,
			GenTokens:    usage.CompletionTokens,
		},
	}, nil
}

// openCall accumulates one streamed tool call's fragments by index.
type openCall struct {
	id   string
	name string
	args strings.Builder
}
