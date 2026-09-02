package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"metron/internal/ollama"
	"metron/internal/tools"
)

// Options bounds the agent loop and the tool budgets it enforces.
type Options struct {
	MaxTurns         int // model round-trips allowed per Step
	CompactThreshold int // tool output size, in bytes, above which slices are purged
	MaxSliceLines    int // widest span view_slice will read
	SearchMaxMatches int // total ripgrep matches
	SearchMaxPerFile int // ripgrep matches per file
}

// DefaultOptions matches metron's built-in configuration.
func DefaultOptions() Options {
	return Options{
		MaxTurns:         10,
		CompactThreshold: 400,
		MaxSliceLines:    120,
		SearchMaxMatches: 10,
		SearchMaxPerFile: 2,
	}
}

// Chatter is the subset of *ollama.Client that the agent depends on.
// It exists so the loop can be exercised without a live Ollama server.
type Chatter interface {
	Chat(ctx context.Context, messages []ollama.Message, tools []ollama.Tool) (*ollama.Message, error)
}

type Agent struct {
	client   Chatter
	opts     Options
	messages []ollama.Message
}

// systemPrompt establishes the tool-only-access-to-code contract.
const systemPrompt = `You are an ultra-minimal token systems programming assistant.
Strict Behavioral Directives:
1. NEVER guess implementations or request whole files.
2. Use 'find_symbol' to resolve interfaces, structs, or functions.
3. Use 'search_text' for text patterns.
4. Use 'view_slice' to view at most 20-60 relevant lines.
5. Use 'apply_patch' with a valid git unified diff (--- a/... +++ b/...) to execute changes.
6. Once the patch is applied, report what changed concisely. No chatty introductions.`

func New(client Chatter, opts Options) *Agent {
	return &Agent{
		client:   client,
		opts:     opts,
		messages: []ollama.Message{{Role: "system", Content: systemPrompt}},
	}
}

// Reset discards the conversation history, keeping only the system prompt.
// History is otherwise unbounded, so this is how an operator reclaims the
// context window without restarting the process.
func (a *Agent) Reset() {
	a.messages = []ollama.Message{{Role: "system", Content: systemPrompt}}
}

var toolDefs = []ollama.Tool{
	{
		Type: "function",
		Function: map[string]any{
			"name":        "find_symbol",
			"description": "Lookup definition locations for structs, functions, or types.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol": map[string]any{"type": "string"},
				},
				"required": []string{"symbol"},
			},
		},
	},
	{
		Type: "function",
		Function: map[string]any{
			"name":        "search_text",
			"description": "Run a scoped ripgrep across files.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
				},
				"required": []string{"pattern"},
			},
		},
	},
	{
		Type: "function",
		Function: map[string]any{
			"name":        "view_slice",
			"description": "Extract specific line ranges from a file. Keep ranges narrow.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"start": map[string]any{"type": "integer"},
					"end":   map[string]any{"type": "integer"},
				},
				"required": []string{"path", "start", "end"},
			},
		},
	},
	{
		Type: "function",
		Function: map[string]any{
			"name":        "apply_patch",
			"description": "Apply standard unified diff via git apply.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"diff": map[string]any{"type": "string"},
				},
				"required": []string{"diff"},
			},
		},
	},
}

func (a *Agent) Step(ctx context.Context, userPrompt string) (string, error) {
	a.messages = append(a.messages, ollama.Message{Role: "user", Content: userPrompt})

	// Execution loop
	for turns := 0; turns < a.opts.MaxTurns; turns++ {
		resp, err := a.client.Chat(ctx, a.messages, toolDefs)
		if err != nil {
			return "", err
		}

		a.messages = append(a.messages, *resp)

		if len(resp.ToolCalls) == 0 {
			a.compactContext()
			return resp.Content, nil
		}

		for _, call := range resp.ToolCalls {
			out := a.dispatch(call)
			a.messages = append(a.messages, ollama.Message{
				Role:    "tool",
				Content: out,
			})
		}
	}

	return "", fmt.Errorf("max turns exceeded")
}

func (a *Agent) dispatch(call ollama.ToolCall) string {
	name := call.Function.Name
	args := call.Function.Arguments
	fmt.Printf("\033[33m[executing: %s]\033[0m\n", name)

	switch name {
	case "find_symbol":
		sym, _ := args["symbol"].(string)
		res, err := tools.FindSymbol(sym)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "search_text":
		pat, _ := args["pattern"].(string)
		res, err := tools.SearchText(pat, a.opts.SearchMaxMatches, a.opts.SearchMaxPerFile)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "view_slice":
		path, _ := args["path"].(string)
		start := toInt(args["start"])
		end := toInt(args["end"])
		res, err := tools.ViewSlice(path, start, end, a.opts.MaxSliceLines)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "apply_patch":
		diff, _ := args["diff"].(string)
		res, err := tools.ApplyPatch(diff)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	default:
		return fmt.Sprintf("Unknown tool %s", name)
	}
}

// compactContext purges large raw file slices from history once a turn finishes
func (a *Agent) compactContext() {
	for i := range a.messages {
		if a.messages[i].Role == "tool" && len(a.messages[i].Content) > a.opts.CompactThreshold {
			if strings.Contains(a.messages[i].Content, " | ") {
				a.messages[i].Content = "[File slice redacted after turn completion]"
			}
		}
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	default:
		return 0
	}
}
