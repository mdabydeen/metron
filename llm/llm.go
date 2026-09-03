// Package llm is the provider-neutral vocabulary the agent speaks.
//
// It exists so the agent loop does not know, or care, whose HTTP API is on the
// other end. Ollama's wire format was the first one metron spoke and its shapes
// had spread into the loop, the REPL and the session transcripts; moving them
// here is what lets an OpenAI-compatible endpoint -- which is to say llama.cpp,
// LM Studio, vLLM, OpenRouter and Ollama itself -- be a configuration choice
// rather than a rewrite.
package llm

import (
	"context"
	"io"
	"time"
)

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	Function struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"function"`
}

// Message is one turn of the conversation.
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

// Tool is one function schema offered to the model.
type Tool struct {
	Type     string `json:"type"`
	Function any    `json:"function"`
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

	// APIKey authenticates to providers that want it. Ollama does not. It is
	// read from an environment variable rather than from a config file, so a
	// key is never one `cat .metron.json` away from being shared.
	APIKey string
}

// DefaultOptions matches metron's built-in configuration.
func DefaultOptions() Options {
	return Options{Temperature: 0.1, TopP: 0.95, NumCtx: 16384, Timeout: 180 * time.Second}
}

// Provider is one endpoint the agent can talk to.
type Provider interface {
	Chat(ctx context.Context, messages []Message, tools []Tool) (*Reply, error)
}
