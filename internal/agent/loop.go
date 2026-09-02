package agent

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"metron/internal/ollama"
	"metron/internal/tools"
)

// Options bounds the agent loop and the tool budgets it enforces.
type Options struct {
	MaxTurns           int // model round-trips allowed per Step
	CompactThreshold   int // tool output size, in bytes, above which slices are purged
	MaxHistoryMessages int // messages retained after a turn, excluding the system prompt
	MaxSliceLines      int // widest span view_slice will read
	MaxLineChars       int // longest single line view_slice will emit
	SearchMaxMatches   int // total ripgrep matches
	SearchMaxPerFile   int // ripgrep matches per file
	ListMaxEntries     int // paths list_files will return

	// Progress receives one "[executing: tool]" line per dispatched call. A nil
	// writer discards them, so the loop stays usable as a library.
	Progress io.Writer

	// Approve is consulted with the diff before apply_patch touches the working
	// tree. A nil approver applies without asking, which is the behaviour every
	// non-interactive caller wants.
	Approve func(diff string) bool

	// PlanMode starts the agent refusing apply_patch unconditionally, for safe
	// read-only exploration. It can be toggled at runtime via SetPlanMode.
	PlanMode bool

	// Instructions is project-specific guidance (e.g. from AGENTS.md), appended
	// to the system prompt when non-empty.
	Instructions string

	// PreToolHook is a scriptable policy gate consulted before every tool
	// call, all five of them -- unlike Approve, which is patch-only and drives
	// the interactive confirmation UI. A nil hook allows everything. Checked
	// after PlanMode and before Approve, so a hook cannot re-allow what plan
	// mode refused, but Approve still gets its own turn at apply_patch.
	PreToolHook func(tool string, args map[string]any) (allowed bool, reason string)

	// MaxUndoStack bounds how many successfully-applied patches /undo can
	// step back through in one session -- an unbounded stack would be the one
	// unbudgeted thing in an otherwise fully-budgeted agent.
	MaxUndoStack int
}

// DefaultOptions matches metron's built-in configuration.
func DefaultOptions() Options {
	return Options{
		MaxTurns:           10,
		CompactThreshold:   400,
		MaxHistoryMessages: 60,
		MaxUndoStack:       20,
		MaxSliceLines:      120,
		MaxLineChars:       500,
		SearchMaxMatches:   10,
		SearchMaxPerFile:   2,
		ListMaxEntries:     60,
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
	planMode  bool
	undoStack []string // successfully-applied diffs, most recent last
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
	prompt := systemPrompt
	if strings.TrimSpace(opts.Instructions) != "" {
		prompt += "\n\n# Project instructions\n" + opts.Instructions
	}
	return &Agent{
		client:   client,
		opts:     opts,
		messages: []ollama.Message{{Role: "system", Content: prompt}},
		planMode: opts.PlanMode,
	}
}

// SetPlanMode toggles read-only mode: while enabled, apply_patch is refused
// unconditionally, before any hook or approval prompt runs, so a misconfigured
// hook cannot override a mode the operator explicitly asked for.
func (a *Agent) SetPlanMode(enabled bool) {
	a.planMode = enabled
}

// PlanMode reports whether read-only mode is currently active.
func (a *Agent) PlanMode() bool {
	return a.planMode
}

// Reset discards the conversation history, keeping only the system prompt
// (including any injected project instructions). History is otherwise
// unbounded, so this is how an operator reclaims the context window without
// restarting the process.
func (a *Agent) Reset() {
	a.messages = a.messages[:1]
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
				Role:       "tool",
				ToolName:   call.Function.Name,
				ToolCallID: call.ID,
				Content:    out,
			})
		}
	}

	return "", fmt.Errorf("max turns exceeded")
}

func (a *Agent) dispatch(call ollama.ToolCall) string {
	name := call.Function.Name
	args := call.Function.Arguments
	fmt.Fprintf(a.progress(), "\033[33m[executing: %s]\033[0m\n", name)

	// Plan mode is checked first and unconditionally: it's a local guarantee
	// the operator asked for directly, so a misconfigured shell hook must not
	// be able to override it.
	if name == "apply_patch" && a.planMode {
		return "Patch rejected: plan mode is active. Do not retry; describe the change instead."
	}

	if a.opts.PreToolHook != nil {
		if allowed, reason := a.opts.PreToolHook(name, args); !allowed {
			if reason == "" {
				reason = fmt.Sprintf("pre_tool_hook denied %s", name)
			}
			return fmt.Sprintf("%s. Do not retry; try a different approach.", reason)
		}
	}

	switch name {
	case "list_files":
		pat, _ := args["pattern"].(string)
		res, err := tools.ListFiles(pat, a.opts.ListMaxEntries)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
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
		res, err := tools.ViewSlice(path, start, end, a.opts.MaxSliceLines, a.opts.MaxLineChars)
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
		res, applied, err := tools.ApplyPatch(diff)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		if applied {
			a.pushUndo(diff)
		}
		return res
	default:
		return fmt.Sprintf("Unknown tool %s", name)
	}
}

// pushUndo records a successfully-applied diff, trimming the oldest entry once
// MaxUndoStack is reached.
func (a *Agent) pushUndo(diff string) {
	a.undoStack = append(a.undoStack, diff)
	if max := a.opts.MaxUndoStack; max > 0 && len(a.undoStack) > max {
		a.undoStack = a.undoStack[len(a.undoStack)-max:]
	}
}

// Undo reverts the most recently applied patch via `git apply -R`, REPL-only
// -- not a sixth model tool, since the five-tool budget is the whole point.
// An empty stack is reported as text, not an error: there's nothing wrong,
// there's just nothing to undo.
func (a *Agent) Undo() (string, error) {
	if len(a.undoStack) == 0 {
		return "Nothing to undo.", nil
	}
	diff := a.undoStack[len(a.undoStack)-1]
	msg, reverted, err := tools.RevertPatch(diff)
	if err != nil {
		return "", err
	}
	if reverted {
		a.undoStack = a.undoStack[:len(a.undoStack)-1]
	}
	return msg, nil
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

// Messages returns a copy of the current conversation, for session persistence.
// A copy, not the live slice, so a caller cannot mutate history out from under
// the agent.
func (a *Agent) Messages() []ollama.Message {
	out := make([]ollama.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// LoadMessages replaces the conversation with a previously saved one, for
// resuming a session. msgs must have a system-role message first; if it
// doesn't, the current system prompt (including any injected project
// instructions) is prepended rather than silently trusting the file.
func (a *Agent) LoadMessages(msgs []ollama.Message) {
	if len(msgs) == 0 || msgs[0].Role != "system" {
		sys := a.messages[0]
		a.messages = append([]ollama.Message{sys}, msgs...)
		return
	}
	a.messages = append([]ollama.Message(nil), msgs...)
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
