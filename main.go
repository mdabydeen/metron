package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"metron/internal/agent"
	"metron/internal/config"
	"metron/internal/ollama"
	"metron/internal/openai"
	"metron/internal/session"
	"metron/internal/tools"
)

const helpText = `Commands:
  /help    show this message
  /config  show the active settings and where they came from
  /reset   clear the conversation history (keeps the system prompt)
  /history show how much conversation is being carried
  /plan    toggle read-only mode (apply_patch is refused while active)
  /undo    revert the most recently applied patch
  /tags    rebuild the ctags symbol index for the current directory
  /exit    quit (also: exit, quit, Ctrl-D)

Anything else is sent to the model. It can only see your code through
list_files, find_symbol, search_text and view_slice, and can only change it through
apply_patch, which shows you the diff and waits for your approval.

Ctrl-C cancels a reply in progress; at an idle prompt it quits.`

// stepper is the agent behaviour the REPL depends on.
type stepper interface {
	Step(ctx context.Context, userPrompt string) (string, error)
	Reset()
	HistorySize() (messages, bytes int)
	LastUsage() (ollama.Usage, int)
	SetPlanMode(enabled bool)
	PlanMode() bool
	Undo() (string, error)
	Messages() []ollama.Message
	LoadMessages(msgs []ollama.Message)
}

// exit is indirected so tests can exercise main without killing the process.
var exit = os.Exit

// version is stamped at build time; see the Makefile.
var version = "dev"

func main() {
	exit(runMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// flags is the command line. metron is a REPL first, so every flag here exists
// to make the same agent usable from a script.
type flags struct {
	prompt      string
	yes         bool
	showVersion bool
	plan        bool
	cont        bool
}

// parseFlags reads the command line. It reports ok=false when the process
// should stop -- for -h, which is not an error, and for a bad flag, which is.
func parseFlags(args []string, errOut io.Writer) (f flags, code int, ok bool) {
	fs := flag.NewFlagSet("metron", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&f.prompt, "p", "", "run one request non-interactively and exit")
	fs.StringVar(&f.prompt, "prompt", "", "run one request non-interactively and exit")
	fs.BoolVar(&f.yes, "yes", false, "apply patches without asking (required by -p to edit files)")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&f.plan, "plan", false, "start in read-only mode: apply_patch is refused")
	fs.BoolVar(&f.cont, "continue", false, "resume the last saved session ("+session.Path+")")
	fs.Usage = func() {
		fmt.Fprintf(errOut, "usage: metron [flags]\n\nRun from the root of the repository you want to work on.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return f, 0, false
		}
		return f, 2, false
	}
	return f, 0, true
}

// runMain wires up config, client and agent, then hands control to the REPL.
// It returns the process exit code.
func runMain(args []string, in io.Reader, out, errOut io.Writer) int {
	f, code, ok := parseFlags(args, errOut)
	if !ok {
		return code
	}
	if f.showVersion {
		fmt.Fprintf(out, "metron %s\n", version)
		return 0
	}

	cfg, path, err := config.Load()
	if err != nil {
		fmt.Fprintf(errOut, "\033[31mconfig error: %v\033[0m\n", err)
		return 1
	}

	instructions, err := config.LoadInstructions(cfg.InstructionsFile, cfg.MaxInstructionsBytes)
	if err != nil {
		fmt.Fprintf(errOut, "\033[31minstructions error: %v\033[0m\n", err)
		return 1
	}

	commands, err := loadCommands(commandsDir, cfg.MaxCommandBytes)
	if err != nil {
		fmt.Fprintf(errOut, "\033[31mcommands error: %v\033[0m\n", err)
		return 1
	}

	// One-shot mode wants exactly one clean answer on stdout, so it never
	// streams; there is no reader watching it arrive.
	streamed := cfg.Stream && f.prompt == ""
	client := newClient(cfg, streamed, out)

	// One scanner serves both the REPL and the patch prompt. Two would race for
	// the same stdin and the second would lose whatever the first had buffered.
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxInputLine)

	opts := agent.Options{
		MaxTurns:           cfg.MaxTurns,
		PlanMode:           cfg.PlanModeDefault || f.plan,
		CompactThreshold:   cfg.CompactThreshold,
		MaxHistoryMessages: cfg.MaxHistoryMessages,
		MaxSliceLines:      cfg.MaxSliceLines,
		MaxLineChars:       cfg.MaxLineChars,
		SearchMaxMatches:   cfg.SearchMaxMatches,
		SearchMaxPerFile:   cfg.SearchMaxPerFile,
		ListMaxEntries:     cfg.ListMaxEntries,
		MaxUndoStack:       cfg.MaxUndoStack,
		Instructions:       instructions,
		Progress:           out,
		PreToolHook:        preToolHook(trustedPreToolHook(cfg.PreToolHook, path, errOut)),
	}
	autoApprove := cfg.AutoApprovePatches || f.yes
	switch {
	case autoApprove:
		// nil approver: apply without asking.
	case f.prompt != "":
		// Nobody is at the keyboard to answer, so fail closed rather than
		// block forever or edit the tree unattended.
		opts.Approve = func(string) bool { return false }
		opts.Progress = errOut
	default:
		opts.Approve = func(diff string) bool { return approve(out, scanner, diff) }
	}
	bot := agent.New(client, opts)

	if f.cont {
		saved, err := session.Load(session.Path)
		if err != nil {
			fmt.Fprintf(errOut, "\033[31mcontinue error: %v\033[0m\n", err)
			return 1
		}
		bot.LoadMessages(saved)
	}

	if f.prompt != "" {
		return oneShot(context.Background(), out, errOut, bot, f.prompt)
	}

	run(context.Background(), scanner, out, cfg, path, bot, streamed, instructions, commands)
	return 0
}

// newClient builds the Chatter for the configured provider. Both clients
// produce the same ollama.Reply/Message/Usage shape -- agent and the rest of
// main work against that shape only, never against a provider's wire format
// -- so this switch is the only place provider selection is visible.
func newClient(cfg config.Config, streamed bool, out io.Writer) agent.Chatter {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	switch cfg.Provider {
	case "openai":
		opts := openai.Options{Temperature: cfg.Temperature, TopP: cfg.TopP, Timeout: timeout, Stream: cfg.Stream}
		if streamed {
			opts.Sink = out
		} else {
			opts.Stream = false
		}
		return openai.NewClient(cfg.Endpoint, cfg.Model, opts)
	default: // "ollama", and Config.Validate rejects anything else
		opts := ollama.Options{
			Temperature: cfg.Temperature, TopP: cfg.TopP, NumCtx: cfg.NumCtx,
			Timeout: timeout, Stream: cfg.Stream,
		}
		if streamed {
			opts.Sink = out
		} else {
			opts.Stream = false
		}
		return ollama.NewClient(cfg.Endpoint, cfg.Model, opts)
	}
}

// trustProjectHookEnv opts in to honoring pre_tool_hook when it came from the
// project-local .metron.json -- a file that may have arrived inside a cloned
// repository, not something the operator necessarily wrote themselves.
const trustProjectHookEnv = "METRON_TRUST_PROJECT_HOOK"

// trustedPreToolHook returns cmdline unchanged, unless it was sourced from the
// project-local config file and the operator has not opted in to trusting
// that file's hooks -- in which case it is refused with a warning rather than
// silently run. Without this gate, a repository's own .metron.json could run
// arbitrary shell commands with the operator's privileges the moment metron
// is started in it, before any approval prompt exists to stop it.
func trustedPreToolHook(cmdline, path string, errOut io.Writer) string {
	if cmdline == "" || !config.IsProjectFile(path) || os.Getenv(trustProjectHookEnv) != "" {
		return cmdline
	}
	fmt.Fprintf(errOut, "\033[33mwarning: ignoring pre_tool_hook from %s -- it came from the project "+
		"directory and may not be trustworthy (e.g. a cloned repository). Move it to your user config "+
		"instead, or set %s=1 if you trust this project.\033[0m\n", path, trustProjectHookEnv)
	return ""
}

// preToolHook wraps the configured shell command as an agent.Options
// PreToolHook, or returns nil when none is configured -- a nil hook allows
// everything, so an unconfigured hook has no effect.
func preToolHook(cmdline string) func(string, map[string]any) (bool, string) {
	if cmdline == "" {
		return nil
	}
	return func(tool string, args map[string]any) (bool, string) {
		allowed, reason, err := tools.RunPreToolHook(cmdline, tool, args)
		if err != nil {
			return false, fmt.Sprintf("pre_tool_hook failed to run: %v", err)
		}
		return allowed, reason
	}
}

// oneShot runs a single request and prints only the answer on stdout, so
// `metron -p ... 2>/dev/null` is pipeable. Progress and warnings go to stderr.
func oneShot(ctx context.Context, out, errOut io.Writer, bot stepper, prompt string) int {
	for _, w := range tools.Preflight() {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}
	resp, err := step(ctx, bot, prompt)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, resp)
	return 0
}

// maxInputLine lets a pasted diff or a long request through; the scanner
// default of 64KB silently ends the session on anything larger.
const maxInputLine = 1 << 20

// commandsDir is where project-level custom slash commands live, alongside
// the session file and gitignored the same way.
const commandsDir = ".metron/commands"

// commandTruncatedMarker mirrors config's instructionsTruncatedMarker for the
// same reason: say plainly when a template was cut off rather than silently
// truncating it.
const commandTruncatedMarker = "\n[command truncated]"

// builtinCommands names every command handled by command() itself, so a
// custom command can never shadow one -- an operator typing /reset should
// always get the real /reset.
var builtinCommands = map[string]bool{
	"/help": true, "/config": true, "/reset": true, "/history": true,
	"/plan": true, "/undo": true, "/tags": true, "/exit": true, "/quit": true,
}

// loadCommands reads .metron/commands/*.md as name -> prompt-template pairs,
// keyed without the leading slash or the .md extension. A missing directory
// is not an error -- the feature is optional, same as AGENTS.md.
func loadCommands(dir string, maxBytes int) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read commands directory %s: %w", dir, err)
	}
	commands := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read command %s: %w", path, err)
		}
		text := strings.TrimSpace(string(data))
		if maxBytes > 0 && len(text) > maxBytes {
			text = text[:maxBytes] + commandTruncatedMarker
		}
		commands[strings.TrimSuffix(e.Name(), ".md")] = text
	}
	return commands, nil
}

// expandCommand substitutes $ARGUMENTS in a command template with whatever
// followed the command name, so a template like "Review $ARGUMENTS for bugs."
// invoked as "/review internal/tools/slice.go" becomes a real, usable prompt.
func expandCommand(template, args string) string {
	return strings.ReplaceAll(template, "$ARGUMENTS", args)
}

// saveSession persists the conversation so --continue can resume it. A
// failure is reported but never fatal -- losing the save is not worth losing
// the session the operator was just in.
func saveSession(out io.Writer, bot stepper) {
	if err := session.Save(session.Path, bot.Messages()); err != nil {
		fmt.Fprintf(out, "\033[33mwarning: could not save session: %v\033[0m\n", err)
	}
}

// approve shows the model's diff and waits for a yes. It reads from the REPL's
// own scanner, so a queued line is consumed here rather than being mistaken for
// the next request. EOF answers no: an operator who is gone has not consented.
func approve(out io.Writer, scanner *bufio.Scanner, diff string) bool {
	fmt.Fprintf(out, "\n\033[1mProposed patch:\033[0m\n%s\n", strings.TrimRight(diff, "\n"))
	fmt.Fprint(out, "\033[1;33mApply this patch? [y/N] \033[0m")
	if !scanner.Scan() {
		fmt.Fprintln(out, "\nNo input; patch not applied.")
		return false
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", "yes":
		return true
	default:
		fmt.Fprintln(out, "Patch not applied.")
		return false
	}
}

// run is the REPL: one Step per line of input until EOF or an exit command.
// streamed tells run that replies already reached out as they arrived, so it
// must not print them a second time. It is a parameter rather than a read of
// cfg.Stream because the caller is what actually wires the client's sink.
func run(ctx context.Context, scanner *bufio.Scanner, out io.Writer, cfg config.Config, cfgPath string, bot stepper, streamed bool, instructions string, commands map[string]string) {
	fmt.Fprintf(out, "\033[1;36m=== metron (model: %s) ===\033[0m\n", cfg.Model)
	fmt.Fprintln(out, "Context-disciplined terminal coder. /help for commands, /exit to quit.")
	if cfgPath != "" {
		fmt.Fprintf(out, "config: %s\n", cfgPath)
	}
	if instructions != "" {
		fmt.Fprintf(out, "project instructions: %s (%d bytes)\n", cfg.InstructionsFile, len(instructions))
	}
	for _, w := range tools.Preflight() {
		fmt.Fprintf(out, "\033[33mwarning: %s\033[0m\n", w)
	}

	for {
		fmt.Fprint(out, "\n\033[1;32mmetron > \033[0m")
		if !scanner.Scan() {
			saveSession(out, bot)
			return
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		// A custom command (from .metron/commands/) is expanded and falls
		// through to be sent to the model below, exactly as if the operator
		// had typed the expansion. Anything else -- including bare "exit"/
		// "quit" and unknown "/foo" -- goes through the built-in dispatch,
		// unconditionally, matching pre-custom-command behaviour.
		expandedFromCommand := false
		if strings.HasPrefix(input, "/") {
			name, rest, _ := strings.Cut(input, " ")
			if template, ok := commands[strings.TrimPrefix(name, "/")]; ok && !builtinCommands[name] {
				input = expandCommand(template, rest)
				expandedFromCommand = true
			}
		}
		if !expandedFromCommand {
			if done := command(out, input, cfg, cfgPath, bot); done {
				saveSession(out, bot)
				return
			}
			if strings.HasPrefix(input, "/") {
				continue
			}
		}

		if streamed {
			// The reply arrives through the client's sink, so open the styled
			// block here and let the text land inside it.
			fmt.Fprint(out, "\n\033[1m")
		}
		resp, err := step(ctx, bot, input)
		if streamed {
			fmt.Fprint(out, "\033[0m")
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(out, "\n\033[33mcancelled\033[0m\n")
			continue
		}
		if err != nil {
			fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
			continue
		}

		if streamed {
			// Already printed as it arrived; reprinting would double it.
			fmt.Fprintln(out)
		} else {
			fmt.Fprintf(out, "\n\033[1m%s\033[0m\n", resp)
		}
		reportUsage(out, bot, cfg.NumCtx)
	}
}

// contextPressure is the share of the requested context window at which the
// prompt is close enough to the ceiling to be worth saying out loud.
const contextPressure = 0.8

// reportUsage prints what the turn cost. Token discipline is metron's whole
// premise, and an unmeasured budget is an aspiration rather than a constraint.
func reportUsage(out io.Writer, bot stepper, numCtx int) {
	usage, calls := bot.LastUsage()
	if usage.PromptTokens == 0 && usage.GenTokens == 0 {
		return // the server did not report counts; say nothing rather than "0"
	}
	fmt.Fprintf(out, "\033[2m[%d prompt / %d generated tokens · %d tool calls]\033[0m\n",
		usage.PromptTokens, usage.GenTokens, calls)
	if numCtx > 0 && float64(usage.PromptTokens) > float64(numCtx)*contextPressure {
		fmt.Fprintf(out, "\033[33mwarning: prompt used %d of %d context tokens - /reset to reclaim it\033[0m\n",
			usage.PromptTokens, numCtx)
	}
}

// step runs one turn under a context that Ctrl-C cancels, so an operator can
// abandon a slow local generation without losing the session.
//
// The handler is installed only for the duration of the turn. At an idle prompt
// the default handler is back in place, so a second Ctrl-C quits -- which is
// what pressing it at an empty prompt should do.
func step(ctx context.Context, bot stepper, input string) (string, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-done:
		}
	}()

	return bot.Step(turnCtx, input)
}

// command handles the REPL's own directives. It reports whether the REPL
// should stop; non-command input is left for the model.
func command(out io.Writer, input string, cfg config.Config, cfgPath string, bot stepper) (quit bool) {
	switch input {
	case "exit", "quit", "/exit", "/quit":
		return true
	case "/help":
		fmt.Fprintln(out, helpText)
	case "/config":
		showConfig(out, cfg, cfgPath)
	case "/reset":
		bot.Reset()
		fmt.Fprintln(out, "Conversation history cleared.")
	case "/history":
		msgs, bytes := bot.HistorySize()
		fmt.Fprintf(out, "history: %d messages, ~%d bytes (budget: %d messages; /reset clears it)\n",
			msgs, bytes, cfg.MaxHistoryMessages)
	case "/plan":
		bot.SetPlanMode(!bot.PlanMode())
		if bot.PlanMode() {
			fmt.Fprintln(out, "plan mode: on (apply_patch is refused until toggled off)")
		} else {
			fmt.Fprintln(out, "plan mode: off (apply_patch behaves normally)")
		}
	case "/undo":
		msg, err := bot.Undo()
		if err != nil {
			fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
			break
		}
		fmt.Fprintln(out, msg)
	case "/tags":
		if err := tools.RebuildTags(); err != nil {
			fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
			break
		}
		fmt.Fprintln(out, "Symbol index rebuilt.")
	default:
		if strings.HasPrefix(input, "/") {
			fmt.Fprintf(out, "Unknown command %q. Try /help.\n", input)
		}
	}
	return false
}

// showConfig prints the effective settings, so an operator can tell at a glance
// which file (if any) is in play and what the budgets actually are.
func showConfig(out io.Writer, cfg config.Config, cfgPath string) {
	source := cfgPath
	if source == "" {
		source = "built-in defaults (no config file found)"
	}
	fmt.Fprintf(out, "source: %s\n", source)
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
	}
}
