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
	"strings"
	"time"

	"github.com/mdabydeen/metron/internal/agent"
	"github.com/mdabydeen/metron/internal/config"
	"github.com/mdabydeen/metron/internal/ollama"
	"github.com/mdabydeen/metron/internal/tools"
)

const helpText = `Commands:
  /help    show this message
  /config  show the active settings and where they came from
  /reset   clear the conversation history (keeps the system prompt)
  /history show how much conversation is being carried
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
	AdvertisedTools() (names []string, schemaBytes int)
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
	doctor      bool
	showVersion bool
}

// parseFlags reads the command line. It reports ok=false when the process
// should stop -- for -h, which is not an error, and for a bad flag, which is.
func parseFlags(args []string, errOut io.Writer) (f flags, code int, ok bool) {
	fs := flag.NewFlagSet("metron", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&f.prompt, "p", "", "run one request non-interactively and exit")
	fs.StringVar(&f.prompt, "prompt", "", "run one request non-interactively and exit")
	fs.BoolVar(&f.yes, "yes", false, "apply patches without asking (required by -p to edit files)")
	fs.BoolVar(&f.doctor, "doctor", false, "check configuration, dependencies, endpoint and model, then exit")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")
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

	// Resolve the project once and use it for both configuration and tools.
	// Otherwise a launch from pkg/deep would operate on the repository root but
	// silently ignore the .metron.json stored there.
	projectRoot := tools.NewEnv(tools.DefaultBudgets()).Root
	cfg, path, err := config.LoadFrom(projectRoot)
	if err != nil {
		fmt.Fprintf(errOut, "\033[31mconfig error: %v\033[0m\n", err)
		return 1
	}

	clientOpts := ollama.Options{
		Temperature:     cfg.Temperature,
		TopP:            cfg.TopP,
		NumCtx:          cfg.NumCtx,
		MaxOutputTokens: cfg.MaxOutputTokens,
		Timeout:         time.Duration(cfg.TimeoutSeconds) * time.Second,
		Stream:          cfg.Stream,
	}
	// One-shot mode wants exactly one clean answer on stdout, so it never
	// streams; there is no reader watching it arrive.
	streamed := cfg.Stream && f.prompt == ""
	if streamed {
		clientOpts.Sink = out
	} else {
		clientOpts.Stream = false
	}
	client := ollama.NewClient(cfg.Endpoint, cfg.Model, clientOpts)

	// One scanner serves both the REPL and the patch prompt. Two would race for
	// the same stdin and the second would lose whatever the first had buffered.
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxInputLine)

	// One Env, shared by the agent and by /tags, so both agree on which
	// project they are pointed at.
	env := tools.Env{Root: projectRoot, Budgets: tools.Budgets{
		MaxSliceLines:    cfg.MaxSliceLines,
		MaxLineChars:     cfg.MaxLineChars,
		SearchMaxMatches: cfg.SearchMaxMatches,
		SearchMaxPerFile: cfg.SearchMaxPerFile,
		ListMaxEntries:   cfg.ListMaxEntries,

		CommandTimeout:        time.Duration(cfg.CommandTimeoutSeconds) * time.Second,
		MaxCommandOutputBytes: cfg.MaxCommandOutputBytes,
	}}
	env.Allowed = tools.ParseAllowlist(cfg.AllowedCommands)
	if f.doctor {
		return runDoctor(context.Background(), out, cfg, path, env, client)
	}
	opts := agent.Options{
		MaxTurns:           cfg.MaxTurns,
		CompactThreshold:   cfg.CompactThreshold,
		MaxHistoryMessages: cfg.MaxHistoryMessages,
		Env:                env,
		DisabledTools:      cfg.DisabledTools,
		Progress:           out,
	}
	autoApprove := cfg.AutoApprovePatches || f.yes
	switch {
	case autoApprove:
		// nil approver: apply without asking.
	case f.prompt != "":
		// Nobody is at the keyboard to answer, so fail closed rather than
		// block forever or edit the tree unattended.
		opts.Approve = func(string, string) bool { return false }
		opts.Progress = errOut
	default:
		opts.Approve = func(kind, preview string) bool { return approve(out, scanner, kind, preview) }
	}
	bot := agent.New(client, opts)

	if f.prompt != "" {
		return oneShot(context.Background(), out, errOut, env, bot, f.prompt)
	}

	run(context.Background(), scanner, out, cfg, path, env, bot, streamed)
	return 0
}

type modelProber interface {
	Probe(context.Context) (ollama.ModelInfo, error)
}

// runDoctor checks everything needed for a useful first run without loading a
// model or changing the project. Its exit code makes it usable as a setup and
// CI health check, while the line-oriented output stays easy to diagnose.
func runDoctor(ctx context.Context, out io.Writer, cfg config.Config, cfgPath string, env tools.Env, prober modelProber) int {
	if cfgPath == "" {
		cfgPath = "built-in defaults"
	}
	fmt.Fprintf(out, "project: ok (%s)\n", env.Root)
	fmt.Fprintf(out, "config: ok (%s)\n", cfgPath)

	ready := true
	warnings := env.Preflight()
	if len(warnings) == 0 {
		fmt.Fprintln(out, "tools: ok")
	} else {
		ready = false
		for _, warning := range warnings {
			fmt.Fprintf(out, "tools: fail (%s)\n", warning)
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := prober.Probe(probeCtx)
	if err != nil {
		ready = false
		fmt.Fprintf(out, "ollama: fail (%v)\n", err)
	} else if !info.Supports("tools") {
		ready = false
		fmt.Fprintf(out, "ollama: fail (model %q does not advertise tool support)\n", cfg.Model)
	} else {
		fmt.Fprintf(out, "ollama: ok (%s, model %s supports tools)\n", cfg.Endpoint, cfg.Model)
	}

	if !ready {
		fmt.Fprintln(out, "doctor: issues found")
		return 1
	}
	fmt.Fprintln(out, "doctor: ready")
	return 0
}

// oneShot runs a single request and prints only the answer on stdout, so
// `metron -p ... 2>/dev/null` is pipeable. Progress and warnings go to stderr.
func oneShot(ctx context.Context, out, errOut io.Writer, env tools.Env, bot stepper, prompt string) int {
	for _, w := range env.Preflight() {
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

// approvalWording is what the prompt says for each kind of effect. A command is
// not a patch: the operator is being asked to let something execute, not to
// accept an edit, and the prompt should not blur the two.
var approvalWording = map[string]struct{ heading, question, refusal string }{
	"patch":   {"Proposed patch", "Apply this patch?", "Patch not applied."},
	"command": {"Proposed command", "Run this command?", "Command not run."},
}

// approve shows what the model wants to do and waits for a yes. It reads from
// the REPL's own scanner, so a queued line is consumed here rather than being
// mistaken for the next request. EOF answers no: an operator who is gone has
// not consented.
func approve(out io.Writer, scanner *bufio.Scanner, kind, preview string) bool {
	w, ok := approvalWording[kind]
	if !ok {
		w = struct{ heading, question, refusal string }{"Proposed action", "Allow this?", "Not allowed."}
	}
	fmt.Fprintf(out, "\n\033[1m%s:\033[0m\n%s\n", w.heading, strings.TrimRight(preview, "\n"))
	fmt.Fprintf(out, "\033[1;33m%s [y/N] \033[0m", w.question)
	if !scanner.Scan() {
		fmt.Fprintf(out, "\nNo input; %s\n", strings.ToLower(w.refusal))
		return false
	}
	switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
	case "y", "yes":
		return true
	default:
		fmt.Fprintln(out, w.refusal)
		return false
	}
}

// run is the REPL: one Step per line of input until EOF or an exit command.
// streamed tells run that replies already reached out as they arrived, so it
// must not print them a second time. It is a parameter rather than a read of
// cfg.Stream because the caller is what actually wires the client's sink.
func run(ctx context.Context, scanner *bufio.Scanner, out io.Writer, cfg config.Config, cfgPath string, env tools.Env, bot stepper, streamed bool) {
	fmt.Fprintf(out, "\033[1;36m=== metron (model: %s) ===\033[0m\n", cfg.Model)
	fmt.Fprintln(out, "Context-disciplined terminal coder. /help for commands, /exit to quit.")
	if cfgPath != "" {
		fmt.Fprintf(out, "config: %s\n", cfgPath)
	}
	for _, w := range env.Preflight() {
		fmt.Fprintf(out, "\033[33mwarning: %s\033[0m\n", w)
	}

	for {
		fmt.Fprint(out, "\n\033[1;32mmetron > \033[0m")
		if !scanner.Scan() {
			return
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if done := command(out, input, cfg, cfgPath, env, bot); done {
			return
		}
		if strings.HasPrefix(input, "/") {
			continue
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
func command(out io.Writer, input string, cfg config.Config, cfgPath string, env tools.Env, bot stepper) (quit bool) {
	switch input {
	case "exit", "quit", "/exit", "/quit":
		return true
	case "/help":
		fmt.Fprintln(out, helpText)
	case "/config":
		showConfig(out, cfg, cfgPath, bot)
	case "/reset":
		bot.Reset()
		fmt.Fprintln(out, "Conversation history cleared.")
	case "/history":
		msgs, bytes := bot.HistorySize()
		fmt.Fprintf(out, "history: %d messages, ~%d bytes (budget: %d messages; /reset clears it)\n",
			msgs, bytes, cfg.MaxHistoryMessages)
	case "/tags":
		if err := env.RebuildTags(); err != nil {
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

// bytesPerToken is a rough conversion for reporting schema cost. Tokenisation
// is model-specific and metron does not ship a tokeniser, so the figure is
// labelled as an estimate rather than presented as a count.
const bytesPerToken = 4

// showConfig prints the effective settings, so an operator can tell at a glance
// which file (if any) is in play and what the budgets actually are.
//
// The advertised tool set is part of that picture: their schemas are sent with
// every single request, so they are a standing cost, and which tools are
// present depends on what is installed.
func showConfig(out io.Writer, cfg config.Config, cfgPath string, bot stepper) {
	source := cfgPath
	if source == "" {
		source = "built-in defaults (no config file found)"
	}
	fmt.Fprintf(out, "source: %s\n", source)

	names, schemaBytes := bot.AdvertisedTools()
	if len(names) == 0 {
		fmt.Fprintln(out, "tools: none advertised")
	} else {
		fmt.Fprintf(out, "tools: %s (%d schema bytes, ~%d tokens per request)\n",
			strings.Join(names, ", "), schemaBytes, schemaBytes/bytesPerToken)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
	}
}
