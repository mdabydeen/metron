package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"metron/internal/config"
)

type fakeStepper struct {
	prompts []string
	reply   string
	err     error
	resets  int
}

func (f *fakeStepper) Step(ctx context.Context, userPrompt string) (string, error) {
	f.prompts = append(f.prompts, userPrompt)
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeStepper) Reset() { f.resets++ }

// cfgFor returns a default config with the model name overridden.
func cfgFor(model string) config.Config {
	c := config.Defaults()
	c.Model = model
	return c
}

func TestRunSendsEachLineToTheAgent(t *testing.T) {
	bot := &fakeStepper{reply: "an answer"}
	var out bytes.Buffer

	run(context.Background(), strings.NewReader("first\n  second  \n"), &out, cfgFor("test-model"), "", bot)

	if len(bot.prompts) != 2 || bot.prompts[0] != "first" || bot.prompts[1] != "second" {
		t.Fatalf("prompts = %q, want the trimmed input lines", bot.prompts)
	}
	if strings.Count(out.String(), "an answer") != 2 {
		t.Fatalf("output = %q, want both replies printed", out.String())
	}
	if !strings.Contains(out.String(), "test-model") {
		t.Fatalf("output = %q, want the banner to name the model", out.String())
	}
}

func TestRunSkipsBlankLines(t *testing.T) {
	bot := &fakeStepper{reply: "ok"}
	var out bytes.Buffer

	run(context.Background(), strings.NewReader("\n   \n\nreal\n"), &out, cfgFor("m"), "", bot)

	if len(bot.prompts) != 1 || bot.prompts[0] != "real" {
		t.Fatalf("prompts = %q, want blank lines ignored", bot.prompts)
	}
}

func TestRunStopsOnExitCommands(t *testing.T) {
	for _, cmd := range []string{"exit", "quit", "/exit", "/quit"} {
		t.Run(cmd, func(t *testing.T) {
			bot := &fakeStepper{reply: "ok"}
			var out bytes.Buffer

			run(context.Background(), strings.NewReader(cmd+"\nafter\n"), &out, cfgFor("m"), "", bot)

			if len(bot.prompts) != 0 {
				t.Fatalf("prompts = %q, want the loop to stop at %q", bot.prompts, cmd)
			}
		})
	}
}

func TestRunStopsAtEOF(t *testing.T) {
	bot := &fakeStepper{reply: "ok"}
	var out bytes.Buffer

	run(context.Background(), strings.NewReader("only\n"), &out, cfgFor("m"), "", bot)

	if len(bot.prompts) != 1 {
		t.Fatalf("prompts = %q, want a clean stop at EOF", bot.prompts)
	}
}

func TestRunKeepsGoingAfterAnAgentError(t *testing.T) {
	bot := &fakeStepper{err: errors.New("ollama unreachable")}
	var out bytes.Buffer

	run(context.Background(), strings.NewReader("one\ntwo\n"), &out, cfgFor("m"), "", bot)

	if len(bot.prompts) != 2 {
		t.Fatalf("prompts = %q, want the REPL to survive the error", bot.prompts)
	}
	if strings.Count(out.String(), "ollama unreachable") != 2 {
		t.Fatalf("output = %q, want both errors reported", out.String())
	}
}

func TestRunShowsConfigPathWhenOneIsUsed(t *testing.T) {
	var out bytes.Buffer
	run(context.Background(), strings.NewReader("exit\n"), &out, cfgFor("m"), "/etc/metron.json", &fakeStepper{})

	if !strings.Contains(out.String(), "config: /etc/metron.json") {
		t.Fatalf("output = %q, want the config path in the banner", out.String())
	}
}

func TestRunWarnsAboutMissingDependencies(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	var out bytes.Buffer

	run(context.Background(), strings.NewReader("exit\n"), &out, cfgFor("m"), "", &fakeStepper{})

	for _, want := range []string{"rg not found", "ctags not found", "git not found"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want a warning containing %q", out.String(), want)
		}
	}
}

func TestRunDoesNotSendCommandsToTheModel(t *testing.T) {
	bot := &fakeStepper{reply: "ok"}
	var out bytes.Buffer

	run(context.Background(), strings.NewReader("/help\n/reset\n/nope\nreal question\n"), &out, cfgFor("m"), "", bot)

	if len(bot.prompts) != 1 || bot.prompts[0] != "real question" {
		t.Fatalf("prompts = %q, want only the non-command line forwarded", bot.prompts)
	}
	if bot.resets != 1 {
		t.Fatalf("resets = %d, want 1", bot.resets)
	}
}

func TestCommandHelp(t *testing.T) {
	var out bytes.Buffer

	if quit := command(&out, "/help", config.Defaults(), "", &fakeStepper{}); quit {
		t.Fatal("/help asked the REPL to quit")
	}
	for _, want := range []string{"/help", "/config", "/reset", "/tags", "/exit"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help text %q missing %q", out.String(), want)
		}
	}
}

func TestCommandReset(t *testing.T) {
	bot := &fakeStepper{}
	var out bytes.Buffer

	command(&out, "/reset", config.Defaults(), "", bot)

	if bot.resets != 1 {
		t.Fatalf("resets = %d, want 1", bot.resets)
	}
	if !strings.Contains(out.String(), "cleared") {
		t.Fatalf("output = %q, want confirmation", out.String())
	}
}

func TestCommandConfigPrintsEffectiveSettings(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Defaults()
	cfg.Model = "printed-model"

	command(&out, "/config", cfg, "/tmp/metron.json", &fakeStepper{})

	if !strings.Contains(out.String(), "source: /tmp/metron.json") {
		t.Fatalf("output = %q, want the config source", out.String())
	}
	body := out.String()[strings.Index(out.String(), "{"):]
	var got config.Config
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("config output is not valid JSON: %v\n%s", err, body)
	}
	if got.Model != "printed-model" || got.MaxSliceLines != cfg.MaxSliceLines {
		t.Fatalf("printed config = %+v, want the effective settings", got)
	}
}

func TestCommandConfigWithoutAFile(t *testing.T) {
	var out bytes.Buffer

	command(&out, "/config", config.Defaults(), "", &fakeStepper{})

	if !strings.Contains(out.String(), "built-in defaults") {
		t.Fatalf("output = %q, want the defaults noted as the source", out.String())
	}
}

func TestCommandTagsRebuildsIndex(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(".tags", []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"-f\" ]; then shift; out=\"$1\"; fi\n  shift\ndone\necho fresh > \"$out\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ctags"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	var out bytes.Buffer
	command(&out, "/tags", config.Defaults(), "", &fakeStepper{})

	if !strings.Contains(out.String(), "rebuilt") {
		t.Fatalf("output = %q, want a rebuild confirmation", out.String())
	}
	b, err := os.ReadFile(".tags")
	if err != nil || !strings.Contains(string(b), "fresh") {
		t.Fatalf(".tags = %q (err %v), want it regenerated", b, err)
	}
}

func TestCommandTagsReportsFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	var out bytes.Buffer
	command(&out, "/tags", config.Defaults(), "", &fakeStepper{})

	if !strings.Contains(out.String(), "Error:") {
		t.Fatalf("output = %q, want the rebuild failure reported", out.String())
	}
}

func TestCommandUnknownSlashCommand(t *testing.T) {
	var out bytes.Buffer

	command(&out, "/bogus", config.Defaults(), "", &fakeStepper{})

	if !strings.Contains(out.String(), "Unknown command") {
		t.Fatalf("output = %q, want an unknown-command notice", out.String())
	}
}

func TestCommandIgnoresPlainInput(t *testing.T) {
	var out bytes.Buffer

	if quit := command(&out, "explain the loop", config.Defaults(), "", &fakeStepper{}); quit {
		t.Fatal("plain input asked the REPL to quit")
	}
	if out.String() != "" {
		t.Fatalf("output = %q, want plain input left for the model", out.String())
	}
}

func TestRunMainStartsAndExitsCleanly(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("METRON_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1/api/chat")
	t.Setenv("OLLAMA_MODEL", "banner-model")

	var out, errOut bytes.Buffer
	// "exit" returns before any request is made, so nothing has to listen.
	code := runMain(strings.NewReader("exit\n"), &out, &errOut)

	if code != 0 {
		t.Fatalf("runMain() = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "banner-model") {
		t.Fatalf("stdout = %q, want the banner naming the configured model", out.String())
	}
	if errOut.String() != "" {
		t.Fatalf("stderr = %q, want it empty", errOut.String())
	}
}

func TestRunMainReportsBadConfig(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "config.json")
	if err := os.WriteFile(bad, []byte(`{"max_turns": -3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METRON_CONFIG", bad)

	var out, errOut bytes.Buffer
	code := runMain(strings.NewReader(""), &out, &errOut)

	if code != 1 {
		t.Fatalf("runMain() = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "config error") {
		t.Fatalf("stderr = %q, want the config error reported", errOut.String())
	}
}

func TestMainExitsWithRunMainCode(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("METRON_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.WriteString("exit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	oldIn, oldOut, oldExit := os.Stdin, os.Stdout, exit
	discard, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	got := -1
	os.Stdin, os.Stdout, exit = stdin, discard, func(code int) { got = code }
	t.Cleanup(func() { os.Stdin, os.Stdout, exit = oldIn, oldOut, oldExit })

	main()

	if got != 0 {
		t.Fatalf("main() exited with %d, want 0", got)
	}
}

// jsonFailingWriter records output but fails the JSON write, so showConfig's
// error path can be observed.
type jsonFailingWriter struct{ buf bytes.Buffer }

func (w *jsonFailingWriter) Write(p []byte) (int, error) {
	if len(p) > 0 && p[0] == '{' {
		return 0, errors.New("disk full")
	}
	return w.buf.Write(p)
}

func TestShowConfigReportsEncodingFailure(t *testing.T) {
	var w jsonFailingWriter

	showConfig(&w, config.Defaults(), "somewhere")

	if !strings.Contains(w.buf.String(), "source: somewhere") {
		t.Fatalf("output = %q, want the source line written", w.buf.String())
	}
	if !strings.Contains(w.buf.String(), "disk full") {
		t.Fatalf("output = %q, want the encoding failure reported", w.buf.String())
	}
}
