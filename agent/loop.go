package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mdabydeen/metron/internal/repomap"
	"github.com/mdabydeen/metron/llm"
	"github.com/mdabydeen/metron/tools"
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

	// SystemPromptExtra is appended to the generated system prompt, for the
	// per-model nudges that would otherwise accumulate as folklore in the
	// binary. It is paid for on every request, like the rest of the prompt.
	SystemPromptExtra string

	// MaxPromptTokens caps what one Step may spend on prompt tokens across all
	// its round-trips. Zero means no ceiling.
	//
	// This is the feature the rest of the program has been arguing for: every
	// other budget bounds one tool, and this bounds the turn. Enforcement has
	// to be predictive, because a token count only arrives *after* the call it
	// describes -- see estimatePromptTokens.
	MaxPromptTokens int

	// RepoMapTokens budgets a structural summary of the project, injected once
	// per session. Zero disables it. It is paid for on every request of the
	// session, so it defaults off until the benchmark says the turns it saves
	// are worth more than the tokens it costs.
	RepoMapTokens int

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

	// seedCount is how many opening messages are structural rather than
	// conversational -- the system prompt, and the repo map if there is one.
	// It is fixed when the history is seeded rather than recounted, because an
	// elision note is also a system message: counting leading system messages
	// would fold each note into the seed, so the next trim would insert another
	// after it and the notes would accumulate without bound.
	seedCount int

	// bytesPerToken is calibrated from what the server reports, so the estimate
	// used to enforce MaxPromptTokens tracks the model actually in use rather
	// than a constant that is wrong for every tokeniser.
	bytesPerToken float64
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
	a := &Agent{
		client:      client,
		opts:        opts,
		advertised:  advertised,
		schemas:     schemas,
		unavailable: unavailable,
	}
	a.messages = a.seed()
	a.seedCount = len(a.messages)
	a.bytesPerToken = defaultBytesPerToken
	return a
}

// defaultBytesPerToken is the starting estimate, before any real count has been
// seen. Roughly right for English and for code in most tokenisers, and corrected
// after the first reply.
const defaultBytesPerToken = 4.0

// historyBytes is the size of what would be sent, schemas included. The schemas
// are part of every request and are large enough to matter.
func (a *Agent) historyBytes() int {
	n := 0
	for _, m := range a.messages {
		n += len(m.Content) + len(m.Role) + len(m.ToolName)
	}
	if b, err := json.Marshal(a.schemas); err == nil {
		n += len(b)
	}
	return n
}

// estimatePromptTokens guesses what the next call will cost.
//
// It has to be a guess. The server reports prompt_eval_count only once it has
// evaluated the prompt, which is after the tokens have been spent -- so a
// ceiling that waited for a real number could only ever report an overrun,
// never prevent one. The estimate is corrected against the real count after
// every call, so it converges on the tokeniser in use.
func (a *Agent) estimatePromptTokens() int {
	return int(float64(a.historyBytes()) / a.bytesPerToken)
}

// calibrate folds a real count into the estimate. The weighting is heavy on
// history because one anomalous call -- a cache hit, a server that counts
// differently -- should nudge the estimate rather than redefine it.
func (a *Agent) calibrate(promptTokens int) {
	if promptTokens <= 0 {
		return
	}
	// historyBytes is never zero -- the schemas alone marshal to something --
	// so the ratio is always positive.
	observed := float64(a.historyBytes()) / float64(promptTokens)
	a.bytesPerToken = clampRatio(0.7*a.bytesPerToken + 0.3*observed)
}

// seed builds the opening history: the system prompt, and the repo map when one
// is budgeted.
//
// The map is a separate message rather than part of the prompt so /reset and
// Restore rebuild it against the tree as it is now, not as it was when the
// session started.
func (a *Agent) seed() []llm.Message {
	prompt := systemPrompt(a.advertised)
	if extra := strings.TrimSpace(a.opts.SystemPromptExtra); extra != "" {
		prompt += "\n" + extra
	}
	msgs := []llm.Message{{Role: "system", Content: prompt}}
	if a.opts.RepoMapTokens <= 0 {
		return msgs
	}
	if m := repomap.Build(a.opts.Env.Root, a.opts.RepoMapTokens); m != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: "A map of this project, for orientation only. It is not a substitute " +
				"for reading code: use the tools before changing anything.\n" + m,
		})
	}
	return msgs
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
	a.messages = a.seed()
	a.seedCount = len(a.messages)
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
		if stop := a.enforceBudget(); stop != "" {
			// Running out of budget is an outcome, not a malfunction: the
			// operator asked for an answer within a ceiling and this is what
			// there was. Returning it as an error would throw away the work.
			a.compactContext()
			return stop, nil
		}
		resp, err := a.client.Chat(ctx, a.messages, a.schemas)
		if err != nil {
			return "", err
		}

		a.lastUsage.Add(resp.Usage)
		a.calibrate(resp.Usage.PromptTokens)
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

// budgetPressure is the share of the ceiling at which the loop starts
// economising rather than waiting to hit it.
const budgetPressure = 0.8

// enforceBudget keeps a turn inside MaxPromptTokens, degrading rather than
// truncating. It returns a final answer when there is no room left to continue,
// and "" when the turn may proceed.
//
// The ladder matters more than the ceiling. Cutting a turn off mid-thought
// wastes everything it has done; shedding the most expendable context first
// usually leaves enough room to finish.
func (a *Agent) enforceBudget() string {
	ceiling := a.opts.MaxPromptTokens
	if ceiling <= 0 {
		return ""
	}
	if a.estimatePromptTokens() < int(float64(ceiling)*budgetPressure) {
		return ""
	}

	// First: purge file slices already read. They are the largest thing in the
	// history and the most re-requestable.
	a.compactContext()
	if a.estimatePromptTokens() < ceiling {
		return ""
	}

	// Then: drop the oldest exchanges, leaving the elision note so the model
	// knows the gap is there rather than reasoning from a history it believes
	// is complete.
	for a.estimatePromptTokens() >= ceiling && len(a.messages) > a.seedLen()+2 {
		saved := a.opts.MaxHistoryMessages
		a.opts.MaxHistoryMessages = max(1, len(a.messages)-a.seedLen()-2)
		a.trimHistory()
		a.opts.MaxHistoryMessages = saved
	}
	if a.estimatePromptTokens() < ceiling {
		return ""
	}

	// Nothing left to shed. Say so plainly rather than failing: the operator
	// asked for an answer within a ceiling, and this is what there was room for.
	return fmt.Sprintf("Stopped at roughly %d tokens of a %d prompt-token budget for this "+
		"turn. Raise max_prompt_tokens, or narrow the request.",
		a.estimatePromptTokens(), ceiling)
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
		m := &a.messages[i]
		if m.Role != "tool" || len(m.Content) <= a.opts.CompactThreshold {
			continue
		}
		if !strings.Contains(m.Content, " | ") {
			continue
		}
		// Name what was purged. An anonymous "[redacted]" tells the model that
		// something is gone but not what, so it either re-reads blindly or
		// carries on as if it still had the text. The header ViewSlice emits is
		// exactly the identifier needed.
		m.Content = fmt.Sprintf("[%s redacted after the turn; request it again if you still need it]",
			sliceHeader(m.Content))
	}
	a.trimHistory()
}

// sliceHeader returns the file:range line a view_slice result starts with, or a
// neutral description when the content does not carry one.
func sliceHeader(content string) string {
	header, _, found := strings.Cut(content, "\n")
	if !found || header == "" || strings.Contains(header, " | ") {
		return "a file slice"
	}
	return header
}

// trimHistory drops the oldest exchanges once history exceeds its budget.
// Redaction bounds how large any single message gets but not how many there
// are, so without this a long session still grows without limit.
//
// The system prompt is always kept, and trimming never leaves a leading tool
// result: a tool message that outlived the assistant call it answers is a
// dangling reference the model cannot interpret.
//
// What is dropped is replaced by one line saying so. Silently vanishing the
// earlier conversation leaves the model reasoning from a history it thinks is
// complete -- it will refer back to a decision it can no longer see, and be
// confused by its own absence rather than by a stated gap.
func (a *Agent) trimHistory() {
	budget := a.opts.MaxHistoryMessages
	keep := a.seedLen()
	if budget <= 0 || len(a.messages) < keep {
		return
	}
	body := a.messages[keep:]
	if len(body) <= budget {
		return
	}
	dropped := body[:len(body)-budget]
	body = body[len(body)-budget:]
	for len(body) > 0 && body[0].Role == "tool" {
		dropped = append(dropped, body[0])
		body = body[1:]
	}
	// The previous note is itself among the dropped messages, so it is folded
	// into the new one rather than counted as a single message. Without this a
	// long session accumulates one note per trim -- paying, on every request,
	// for a growing list of announcements that something was removed to save
	// tokens -- and the counts reset each time instead of adding up.
	a.messages = append(a.messages[:keep:keep], elisionNote(dropped))
	a.messages = append(a.messages, body...)
}

// seedLen is the number of structural opening messages, never fewer than one:
// a zero-valued Agent still has to keep its first message.
func (a *Agent) seedLen() int {
	if a.seedCount < 1 {
		return 1
	}
	return a.seedCount
}

// elisionMarker is what makes a note recognisable to a later trim.
const elisionMarker = "earlier messages elided"

func isElisionNote(m llm.Message) bool {
	return m.Role == "system" && strings.Contains(m.Content, elisionMarker)
}

// countElided reads the number back out of a note, so a later trim can carry it
// forward. A note may come from a restored transcript, so it is not necessarily
// one metron wrote: anything unparseable counts as nothing.
func countElided(m llm.Message) int {
	digits, _, found := strings.Cut(strings.TrimPrefix(m.Content, "["), " ")
	if !found {
		return 0
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// elisionNote summarises what trimming removed. It is mechanical on purpose: an
// LLM-written summary costs a whole model call, and whether that buys anything
// is a question for the benchmark rather than for an assumption.
func elisionNote(dropped []llm.Message) llm.Message {
	counts := map[string]int{}
	var order []string
	total := 0
	for _, m := range dropped {
		// A note already among the dropped messages stands for everything it
		// summarised, not for one message.
		if isElisionNote(m) {
			total += countElided(m)
			continue
		}
		total++
		if m.Role != "tool" || m.ToolName == "" {
			continue
		}
		if counts[m.ToolName] == 0 {
			order = append(order, m.ToolName)
		}
		counts[m.ToolName]++
	}
	var parts []string
	for _, name := range order {
		if n := counts[name]; n > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", name, n))
		} else {
			parts = append(parts, name)
		}
	}
	note := fmt.Sprintf("[%d %s to stay within the context budget", total, elisionMarker)
	if len(parts) > 0 {
		note += ": " + strings.Join(parts, ", ")
	}
	return llm.Message{Role: "system", Content: note + "]"}
}

// ToolRun is one dispatched tool call and how long it took. The duration is
// wall clock, which for a local model is dwarfed by generation -- it is here so
// a benchmark can tell a slow tool from a slow model rather than guess.
type ToolRun struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

// SetMaxPromptTokens changes the per-turn ceiling mid-session, backing /budget.
// Zero removes it.
func (a *Agent) SetMaxPromptTokens(n int) { a.opts.MaxPromptTokens = n }

// MaxPromptTokens reports the current per-turn ceiling.
func (a *Agent) MaxPromptTokens() int { return a.opts.MaxPromptTokens }

// EstimatedPromptTokens reports what the next call is expected to cost, so an
// operator can see the number the ceiling is compared against rather than
// having to trust it.
func (a *Agent) EstimatedPromptTokens() int { return a.estimatePromptTokens() }

// Bytes per token is between about 2 and 6 for every tokeniser metron is likely
// to meet, on code or on prose. Clamping to a wider band than that admits real
// variation while refusing nonsense.
//
// The nonsense is not hypothetical, and is a correctness problem before it is a
// security one: a server reporting only *uncached* prompt tokens under prompt
// caching reports a fraction of the real count, which without a clamp drives the
// ratio up until the estimate collapses and the ceiling silently stops holding.
// A server that wanted to defeat the ceiling would do exactly the same thing on
// purpose. Driven the other way the ratio approaches zero, and dividing by it
// yields a value Go leaves undefined -- which also disables the ceiling.
const (
	minBytesPerToken = 1.5
	maxBytesPerToken = 8.0
)

func clampRatio(r float64) float64 {
	if r < minBytesPerToken {
		return minBytesPerToken
	}
	if r > maxBytesPerToken {
		return maxBytesPerToken
	}
	return r
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
		// An allowlist, not a "skip system" check. A transcript is a file, and
		// the package doc invites people to attach one to a bug report -- so a
		// role of "developer" (system-equivalent on OpenAI-compatible servers)
		// or "system " with a trailing space would otherwise be restored with
		// authority the operator never granted.
		switch m.Role {
		case "user", "assistant", "tool":
			a.messages = append(a.messages, m)
		}
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
