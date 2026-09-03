package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mdabydeen/metron/internal/llm"
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

	// DisabledTools are tools the operator has switched off. They are not
	// advertised and cannot be called, which also removes their schemas from
	// every request.
	DisabledTools []string

	// Progress receives one "[executing: tool]" line per dispatched call. A nil
	// writer discards them, so the loop stays usable as a library.
	Progress io.Writer

	// Approve is consulted before any tool causes an effect: apply_patch
	// touching the working tree, run_command executing anything. kind names
	// what is being asked ("patch", "command") so the caller can render the
	// preview appropriately; preview is the diff or the command line.
	//
	// A nil approver proceeds without asking, which is the behaviour every
	// non-interactive caller wants.
	Approve func(kind, preview string) bool
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
	Chat(ctx context.Context, messages []llm.Message, tools []llm.Tool) (*llm.Reply, error)
}

type Agent struct {
	client    Chatter
	opts      Options
	messages  []llm.Message
	lastUsage llm.Usage
	lastCalls int
	lastTools []ToolRun

	// advertised is the tool set sent with every request, fixed when the agent
	// is built. Schemas are paid for on every model call, so a tool that cannot
	// run here is left out rather than offered and refused.
	advertised  []string
	schemas     []llm.Tool
	unavailable map[string]string
}

// promptOpening and promptClosing bracket the per-tool directives. The contract
// they establish -- tools are the only way to see code -- is what the whole
// program exists to enforce, so it is stated first and unconditionally.
const (
	promptOpening = `You are an ultra-minimal token systems programming assistant.
Strict Behavioral Directives:
1. NEVER guess implementations or request whole files.`
	promptClosing = `Report what changed concisely. No chatty introductions.`
)

// toolDirectives is the one line of the system prompt each tool earns. A tool
// that is not advertised contributes nothing: naming a tool the model cannot
// call wastes tokens on every request and invites it to try anyway.
var toolDirectives = map[string]string{
	tools.ToolListFiles:  "Use 'list_files' to discover what exists before assuming a path.",
	tools.ToolFindSymbol: "Use 'find_symbol' to resolve interfaces, structs, or functions.",
	tools.ToolSearchText: "Use 'search_text' for text patterns.",
	tools.ToolViewSlice:  "Use 'view_slice' to view at most 20-60 relevant lines.",
	tools.ToolApplyPatch: "Use 'apply_patch' with a valid git unified diff (--- a/... +++ b/...) to execute changes.",
	tools.ToolEditFile:   "Use 'edit_file' to change code: quote the exact lines to replace in 'search'. No line numbers.",
	tools.ToolRunCommand: "Use 'run_command' to check your work; only the operator's allowed commands will run.",
}

// systemPrompt renders the directives for exactly the tools being advertised,
// numbered continuously so the list reads as one instruction set.
func systemPrompt(advertised []string) string {
	var sb strings.Builder
	sb.WriteString(promptOpening)
	n := 1 // the opening already carries directive 1
	for _, name := range advertised {
		n++
		fmt.Fprintf(&sb, "\n%d. %s", n, toolDirectives[name])
	}
	fmt.Fprintf(&sb, "\n%d. %s", n+1, promptClosing)
	return sb.String()
}

func New(client Chatter, opts Options) *Agent {
	// Resolving here rather than in DefaultOptions keeps that function pure and
	// means the root is found once, when the agent is built, instead of on
	// every tool call.
	// Rooted rather than NewEnv: replacing the whole Env would discard whatever
	// else the caller configured on it, the command allowlist included.
	opts.Env = opts.Env.Rooted()
	unavailable := opts.Env.UnavailableTools()
	advertised := advertise(unavailable, opts.DisabledTools)
	schemas := make([]llm.Tool, 0, len(advertised))
	for _, name := range advertised {
		schemas = append(schemas, describeTool(name, opts.Env))
	}
	return &Agent{
		client:      client,
		opts:        opts,
		advertised:  advertised,
		schemas:     schemas,
		unavailable: unavailable,
		messages:    []llm.Message{{Role: "system", Content: systemPrompt(advertised)}},
	}
}

// describeTool returns a tool's schema, specialised to this project where the
// static description is not enough. run_command is the case that matters: a
// model told only that "permitted commands" exist will guess, and every guess
// costs a turn, so the allowlist itself goes into the description.
func describeTool(name string, env tools.Env) llm.Tool {
	def := toolDefs[name]
	if name != tools.ToolRunCommand {
		return def
	}
	fn, ok := def.Function.(map[string]any)
	if !ok {
		return def
	}
	// Copy before writing: toolDefs is package state shared by every agent.
	clone := make(map[string]any, len(fn))
	for k, v := range fn {
		clone[k] = v
	}
	clone["description"] = fmt.Sprintf("%s %s", fn["description"], env.AllowedPhrase())
	def.Function = clone
	return def
}

// advertise returns the tools worth sending: everything metron implements, less
// what cannot run here and less what the operator switched off.
func advertise(unavailable map[string]string, disabled []string) []string {
	off := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		off[name] = true
	}
	var out []string
	for _, name := range tools.ToolNames {
		if _, broken := unavailable[name]; broken || off[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// AdvertisedTools reports the tools sent to the model and the size of their
// schemas in bytes. The size is the point: it is paid on every single request,
// so an operator should be able to see it rather than infer it.
func (a *Agent) AdvertisedTools() (names []string, schemaBytes int) {
	if b, err := json.Marshal(a.schemas); err == nil {
		schemaBytes = len(b)
	}
	return a.advertised, schemaBytes
}

// Reset discards the conversation history, keeping only the system prompt.
// History is otherwise unbounded, so this is how an operator reclaims the
// context window without restarting the process.
func (a *Agent) Reset() {
	a.messages = []llm.Message{{Role: "system", Content: systemPrompt(a.advertised)}}
}

var toolDefs = map[string]llm.Tool{
	tools.ToolListFiles: {
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
	tools.ToolFindSymbol: {
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
	tools.ToolSearchText: {
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
	tools.ToolViewSlice: {
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
	tools.ToolEditFile: {
		Type: "function",
		Function: map[string]any{
			"name": "edit_file",
			// Every word here is paid for on every request, so the description
			// carries only what the model gets wrong without it: quote verbatim,
			// match exactly once, and the two empty-string special cases.
			"description": "Replace lines in a file. 'search' quotes the lines to replace verbatim; " +
				"it is matched, not numbered, so it must occur exactly once -- quote more " +
				"surrounding lines if it would not. Empty 'replace' deletes them. " +
				"Empty 'search' creates the file from 'replace'.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"search":  map[string]any{"type": "string"},
					"replace": map[string]any{"type": "string"},
				},
				"required": []string{"path", "search", "replace"},
			},
		},
	},
	tools.ToolRunCommand: {
		Type: "function",
		Function: map[string]any{
			"name": "run_command",
			"description": "Run one permitted command in the project and return its exit status and output. " +
				"There is no shell: the command is split on whitespace and executed directly, so pipes, " +
				"redirection, globs, quotes and ; && || are not interpreted.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
	},
	tools.ToolApplyPatch: {
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
	a.messages = append(a.messages, llm.Message{Role: "user", Content: userPrompt})
	a.lastUsage, a.lastCalls, a.lastTools = llm.Usage{}, 0, nil

	// Execution loop
	for turns := 0; turns < a.opts.MaxTurns; turns++ {
		resp, err := a.client.Chat(ctx, a.messages, a.schemas)
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
			started := time.Now()
			out := a.dispatch(ctx, call)
			a.lastTools = append(a.lastTools, ToolRun{
				Name: call.Function.Name,
				Ms:   time.Since(started).Milliseconds(),
			})
			a.messages = append(a.messages, llm.Message{
				Role:     "tool",
				ToolName: call.Function.Name,
				Content:  out,
			})
		}
	}

	return "", fmt.Errorf("max turns exceeded")
}

func (a *Agent) dispatch(ctx context.Context, call llm.ToolCall) string {
	name := call.Function.Name
	args := call.Function.Arguments

	// A tool that was not advertised is refused before it runs. The model
	// should not be asking -- the schema was never sent -- but a refusal
	// phrased like tools.missingBinary costs one turn, where letting the call
	// through costs a confusing failure and several retries.
	if refusal, ok := a.refuse(name); ok {
		fmt.Fprintf(a.progress(), "\033[33m[refused: %s]\033[0m\n", name)
		return refusal
	}
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
		if a.opts.Approve != nil && !a.opts.Approve("patch", diff) {
			// Phrased at the model, like tools.missingBinary: a refusal it can
			// act on beats an error it will retry until the turn budget is gone.
			return "Patch rejected by the operator. Do not retry; describe the change instead."
		}
		res, err := a.opts.Env.ApplyPatch(diff)
		if err != nil {
			return fmt.Sprintf("Error: %v", err)
		}
		return res
	case "edit_file":
		path, _ := args["path"].(string)
		search, _ := args["search"].(string)
		replace, _ := args["replace"].(string)
		// The operator is shown a unified diff, never a search/replace block:
		// the format exists to make the model's job easier, and should not make
		// the human's job harder.
		if a.opts.Approve != nil && !a.opts.Approve("patch", a.opts.Env.EditPreview(path, search, replace)) {
			return "Edit rejected by the operator. Do not retry; describe the change instead."
		}
		return a.opts.Env.EditFile(path, search, replace)
	case "run_command":
		command, _ := args["command"].(string)
		if a.opts.Approve != nil && !a.opts.Approve("command", command) {
			return "Command rejected by the operator. Do not retry; " +
				"describe what you would run and why instead."
		}
		// No error branch: RunCommand reports every outcome as text, because a
		// refusal, a timeout and a failing test are all things the model should
		// read rather than things that went wrong with metron.
		return a.opts.Env.RunCommand(ctx, command)
	default:
		return fmt.Sprintf("Unknown tool %s", name)
	}
}

// refuse reports whether a tool call should be turned down without running,
// and what to tell the model. Only tools metron implements are considered; an
// invented name falls through to dispatch's unknown-tool branch.
func (a *Agent) refuse(name string) (string, bool) {
	if _, implemented := toolDefs[name]; !implemented {
		return "", false
	}
	for _, adv := range a.advertised {
		if adv == name {
			return "", false
		}
	}
	if reason, broken := a.unavailable[name]; broken {
		return fmt.Sprintf("%s is unavailable in this project: %s. Do not retry it.", name, reason), true
	}
	return fmt.Sprintf("%s has been disabled by the operator. Do not retry it.", name), true
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

// ToolRun is one dispatched tool call and how long it took. The duration is
// wall clock, which for a local model is dwarfed by generation -- it is here so
// a benchmark can tell a slow tool from a slow model rather than guess.
type ToolRun struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

// LastTools reports the tools the most recent Step ran, in order.
func (a *Agent) LastTools() []ToolRun { return a.lastTools }

// LastUsage reports what the most recent Step cost: tokens across every model
// call it made, and how many tools it ran. A budget nobody can see is a budget
// nobody keeps.
func (a *Agent) LastUsage() (llm.Usage, int) {
	return a.lastUsage, a.lastCalls
}

// Messages returns a copy of the conversation, for saving. It is a copy so a
// caller writing it to disk cannot be surprised by the next turn mutating what
// it is halfway through serialising -- compaction rewrites messages in place.
func (a *Agent) Messages() []llm.Message {
	out := make([]llm.Message, len(a.messages))
	copy(out, a.messages)
	return out
}

// Restore replaces the conversation with a saved one, backing /resume.
//
// The system prompt is regenerated rather than restored: it describes the tools
// available *now*, and a session saved on a machine with ripgrep should not tell
// this one to call search_text.
func (a *Agent) Restore(messages []llm.Message) {
	a.Reset()
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		a.messages = append(a.messages, m)
	}
	a.trimHistory()
}

// HistorySize reports the number of retained messages and their combined
// content size, so an operator can see the context budget rather than guess it.
func (a *Agent) HistorySize() (messages, bytes int) {
	for _, m := range a.messages {
		bytes += len(m.Content)
	}
	return len(a.messages), bytes
}

// maxToolInt bounds a number taken from a tool call. JSON numbers arrive as
// float64, and converting an enormous one straight to int produces a value that
// then overflows in ordinary arithmetic downstream.
const maxToolInt = 1 << 31

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		if n > maxToolInt || n < -maxToolInt {
			return 0
		}
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
