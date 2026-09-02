package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"metron/internal/agent"
	"metron/internal/config"
	"metron/internal/ollama"
	"metron/internal/tools"
)

const helpText = `Commands:
  /help    show this message
  /config  show the active settings and where they came from
  /reset   clear the conversation history (keeps the system prompt)
  /tags    rebuild the ctags symbol index for the current directory
  /exit    quit (also: exit, quit, Ctrl-D)

Anything else is sent to the model. It can only see your code through
find_symbol, search_text and view_slice, and can only change it through
apply_patch, which edits the working tree directly.`

// stepper is the agent behaviour the REPL depends on.
type stepper interface {
	Step(ctx context.Context, userPrompt string) (string, error)
	Reset()
}

// exit is indirected so tests can exercise main without killing the process.
var exit = os.Exit

func main() {
	exit(runMain(os.Stdin, os.Stdout, os.Stderr))
}

// runMain wires up config, client and agent, then hands control to the REPL.
// It returns the process exit code.
func runMain(in io.Reader, out, errOut io.Writer) int {
	cfg, path, err := config.Load()
	if err != nil {
		fmt.Fprintf(errOut, "\033[31mconfig error: %v\033[0m\n", err)
		return 1
	}

	client := ollama.NewClient(cfg.Endpoint, cfg.Model, ollama.Options{
		Temperature: cfg.Temperature,
		TopP:        cfg.TopP,
		NumCtx:      cfg.NumCtx,
		Timeout:     time.Duration(cfg.TimeoutSeconds) * time.Second,
	})
	bot := agent.New(client, agent.Options{
		MaxTurns:         cfg.MaxTurns,
		CompactThreshold: cfg.CompactThreshold,
		MaxSliceLines:    cfg.MaxSliceLines,
		SearchMaxMatches: cfg.SearchMaxMatches,
		SearchMaxPerFile: cfg.SearchMaxPerFile,
	})

	run(context.Background(), in, out, cfg, path, bot)
	return 0
}

// run is the REPL: one Step per line of input until EOF or an exit command.
func run(ctx context.Context, in io.Reader, out io.Writer, cfg config.Config, cfgPath string, bot stepper) {
	fmt.Fprintf(out, "\033[1;36m=== metron (model: %s) ===\033[0m\n", cfg.Model)
	fmt.Fprintln(out, "Context-disciplined terminal coder. /help for commands, /exit to quit.")
	if cfgPath != "" {
		fmt.Fprintf(out, "config: %s\n", cfgPath)
	}
	for _, w := range tools.Preflight() {
		fmt.Fprintf(out, "\033[33mwarning: %s\033[0m\n", w)
	}

	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "\n\033[1;32mmetron > \033[0m")
		if !scanner.Scan() {
			return
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if done := command(out, input, cfg, cfgPath, bot); done {
			return
		}
		if strings.HasPrefix(input, "/") {
			continue
		}

		resp, err := bot.Step(ctx, input)
		if err != nil {
			fmt.Fprintf(out, "\033[31mError: %v\033[0m\n", err)
			continue
		}

		fmt.Fprintf(out, "\n\033[1m%s\033[0m\n", resp)
	}
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
