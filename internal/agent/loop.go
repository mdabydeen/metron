package agent

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mdabydeen/metron/internal/ollama"
	"github.com/mdabydeen/metron/internal/tools"
)

// Options bounds the agent loop and carries the environment the tools run in.
type Options struct {
	MaxTurns           int // model round-trips allowed per Step
	CompactThreshold   int // tool output size, in bytes, above which slices are purged
	MaxHistoryMessages int // messages retained after a turn, excluding the system prompt

	// Env is the project the tools operate on: the root they are confined to
	// and the budgets they enforce. A zero Root is resolved at New, so a caller
	// that only cares about the loop can leave it alone.
	Env tools.Env

	// Progress receives one "[executing: tool]" line per dispatched call. A nil
	// writer discards them, so the loop stays usable as a library.
	Progress io.Writer

	// Approve is consulted with the diff before apply_patch touches the working
	// tree. A nil approver applies without asking, which is the behaviour every
	// non-interactive caller wants.
	Approve func(diff string) bool
}

// DefaultOptions matches metron's built-in configuration. The tool environment
// is left unrooted; New resolves it against the current project.
func DefaultOptions() Options {
	return Options{
		MaxTurns:           10,
		CompactThreshold:   400,
		MaxHistoryMessages: 60,
		Env:                tools.Env{Budgets: tools.DefaultBudgets()},
	}
}

// Chatter is the subset of *ollama.Client that the agent depends on.
// It exists so the loop can be exercised without a live Ollama server.
type Chatter interface {
	Chat(ctx context.Context, messages []ollama.Message, tools []ollama.Tool) (*ollama.Reply, error)
}

type Agent struct {
	client    Chatter
	opts      Options
	messages  []ollama.Message
	lastUsage ollama.Usage
	lastCalls int
}

// systemPrompt establishes the tool-only-access-to-code contract.
const systemPrompt = `You are an ultra-minimal token systems programming assistant.
Strict Behavioral Directives:
1. NEVER guess implementations or request whole files.
2. Use 'list_files' to discover what exists before assuming a path.
3. Use 'find_symbol' to resolve interfaces, structs, or functions.
4. Use 'search_text' for text patterns.
5. Use 'view_slice' to view at most 20-60 relevant lines.
6. Use 'apply_patch' with a valid git unified diff (--- a/... +++ b/...) to execute changes.
7. Once the patch is applied, report what changed concisely. No chatty introductions.`

func New(client Chatter, opts Options) *Agent {
	// Resolving here rather than in DefaultOptions keeps that function pure and
	// means the root is found once, when the agent is built, instead of on
	// every tool call.
	if opts.Env.Root == "" {
		opts.Env = tools.NewEnv(opts.Env.Budgets)
	}
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
			"name":        "list_files",
			"description": "List files in the project, optionally narrowed by a glob such as 'internal/**/*.go'.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string"},
				},
			},
		},
	},
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
	a.lastUsage, a.lastCalls = ollama.Usage{}, 0

	// Execution loop
	for turns := 0; turns < a.opts.MaxTurns; turns++ {
		resp, err := a.client.Chat(ctx, a.messages, toolDefs)
		if err != nil {
			return "", err
		}

		a.lastUsage.Add(resp.Usage)
		a.messages = append(a.messages, resp.Message)

		if len(resp.ToolCalls) == 0 {
			a.compactContext()
			return resp.Content, nil
		}

		a.lastCalls += len(resp.ToolCalls)
		for _, call := range resp.ToolCalls {
			out := a.dispatch(call)
			a.messages = append(a.messages, ollama.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  out,
			})
		}
	}

	return "", fmt.Errorf("max turns exceeded")
}

func (a *Agent) dispatch(call ollama.ToolCall) string {
	name := call.Function.Name
	args := call.Function.Arguments
	fmt.Fprintf(a.progress(), "\033[33m[executing: %s]\033[0m\n", name)

	switch name {
	case "list_files":
		pat, _ := args["pattern"].(string)
		res, err := a.opts.Env.ListFiles(pat)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "find_symbol":
		sym, _ := args["symbol"].(string)
		res, err := a.opts.Env.FindSymbol(sym)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "search_text":
		pat, _ := args["pattern"].(string)
		res, err := a.opts.Env.SearchText(pat)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "view_slice":
		path, _ := args["path"].(string)
		start := toInt(args["start"])
		end := toInt(args["end"])
		res, err := a.opts.Env.ViewSlice(path, start, end)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "apply_patch":
		diff, _ := args["diff"].(string)
		if a.opts.Approve != nil && !a.opts.Approve(diff) {
			// Phrased at the model, like tools.missingBinary: a refusal it can
			// act on beats an error it will retry until the turn budget is gone.
			return "Patch rejected by the operator. Do not retry; describe the change instead."
		}
		res, err := a.opts.Env.ApplyPatch(diff)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	default:
		return fmt.Sprintf("Unknown tool %s", name)
	}
}

// progress returns the writer for tool-execution notices, defaulting to a sink
// so a zero-valued Options never writes to whatever stdout happens to be.
func (a *Agent) progress() io.Writer {
	if a.opts.Progress == nil {
		return io.Discard
	}
	return a.opts.Progress
}

// compactContext purges large raw file slices from history once a turn finishes,
// then trims the history to its message budget.
func (a *Agent) compactContext() {
	for i := range a.messages {
		if a.messages[i].Role == "tool" && len(a.messages[i].Content) > a.opts.CompactThreshold {
			if strings.Contains(a.messages[i].Content, " | ") {
				a.messages[i].Content = "[File slice redacted after turn completion]"
			}
		}
	}
	a.trimHistory()
}

// trimHistory drops the oldest exchanges once history exceeds its budget.
// Redaction bounds how large any single message gets but not how many there
// are, so without this a long session still grows without limit.
//
// The system prompt is always kept, and trimming never leaves a leading tool
// result: a tool message that outlived the assistant call it answers is a
// dangling reference the model cannot interpret.
func (a *Agent) trimHistory() {
	budget := a.opts.MaxHistoryMessages
	if budget <= 0 || len(a.messages) == 0 {
		return
	}
	body := a.messages[1:]
	if len(body) <= budget {
		return
	}
	body = body[len(body)-budget:]
	for len(body) > 0 && body[0].Role == "tool" {
		body = body[1:]
	}
	a.messages = append(a.messages[:1:1], body...)
}

// LastUsage reports what the most recent Step cost: tokens across every model
// call it made, and how many tools it ran. A budget nobody can see is a budget
// nobody keeps.
func (a *Agent) LastUsage() (ollama.Usage, int) {
	return a.lastUsage, a.lastCalls
}

// HistorySize reports the number of retained messages and their combined
// content size, so an operator can see the context budget rather than guess it.
func (a *Agent) HistorySize() (messages, bytes int) {
	for _, m := range a.messages {
		bytes += len(m.Content)
	}
	return len(a.messages), bytes
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
