package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mdabydeen/metron/internal/config"
	"github.com/mdabydeen/metron/internal/ollama"
	"github.com/mdabydeen/metron/internal/tools"
)

// testEnv returns a tools.Env rooted at the test's working directory, so a test
// that chdirs into a scratch tree gets tools confined to that tree rather than
// to whatever repository the suite happens to be running inside.
func testEnv(t *testing.T) tools.Env {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	real, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatalf("resolve %s: %v", wd, err)
	}
	return tools.Env{Root: real, Budgets: tools.DefaultBudgets()}
}

type fakeStepper struct {
	prompts []string
	reply   string
	err     error
	resets  int
	msgs    int
	bytes   int
	usage   ollama.Usage
	calls   int

	advertised  []string
	schemaBytes int
}

func (f *fakeStepper) Step(ctx context.Context, userPrompt string) (string, error) {
	f.prompts = append(f.prompts, userPrompt)
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func (f *fakeStepper) Reset() { f.resets++ }

func (f *fakeStepper) HistorySize() (int, int) { return f.msgs, f.bytes }

func (f *fakeStepper) LastUsage() (ollama.Usage, int) { return f.usage, f.calls }

func (f *fakeStepper) AdvertisedTools() ([]string, int) { return f.advertised, f.schemaBytes }

// gitDir makes a scratch git repository the working directory. apply_patch is
// only advertised inside a work tree, so any test that exercises the patch path
// end to end needs one.
func gitDir(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// replFor wraps REPL input the way runMain does, since run reads from a shared
// scanner rather than an io.Reader of its own.
func replFor(input string) *bufio.Scanner {
	return bufio.NewScanner(strings.NewReader(input))
}

// cfgFor returns a default config with the model name overridden.
func cfgFor(model string) config.Config {
	c := config.Defaults()
	c.Model = model
	return c
}

func TestRunSendsEachLineToTheAgent(t *testing.T) {
	bot := &fakeStepper{reply: "an answer"}
	var out bytes.Buffer

	run(context.Background(), replFor("first\n  second  \n"), &out, cfgFor("test-model"), "", testEnv(t), bot, false)

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

	run(context.Background(), replFor("\n   \n\nreal\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	if len(bot.prompts) != 1 || bot.prompts[0] != "real" {
		t.Fatalf("prompts = %q, want blank lines ignored", bot.prompts)
	}
}

func TestRunStopsOnExitCommands(t *testing.T) {
	for _, cmd := range []string{"exit", "quit", "/exit", "/quit"} {
		t.Run(cmd, func(t *testing.T) {
			bot := &fakeStepper{reply: "ok"}
			var out bytes.Buffer

			run(context.Background(), replFor(cmd+"\nafter\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

			if len(bot.prompts) != 0 {
				t.Fatalf("prompts = %q, want the loop to stop at %q", bot.prompts, cmd)
			}
		})
	}
}

func TestRunStopsAtEOF(t *testing.T) {
	bot := &fakeStepper{reply: "ok"}
	var out bytes.Buffer

	run(context.Background(), replFor("only\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	if len(bot.prompts) != 1 {
		t.Fatalf("prompts = %q, want a clean stop at EOF", bot.prompts)
	}
}

func TestRunKeepsGoingAfterAnAgentError(t *testing.T) {
	bot := &fakeStepper{err: errors.New("ollama unreachable")}
	var out bytes.Buffer

	run(context.Background(), replFor("one\ntwo\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	if len(bot.prompts) != 2 {
		t.Fatalf("prompts = %q, want the REPL to survive the error", bot.prompts)
	}
	if strings.Count(out.String(), "ollama unreachable") != 2 {
		t.Fatalf("output = %q, want both errors reported", out.String())
	}
}

func TestRunShowsConfigPathWhenOneIsUsed(t *testing.T) {
	var out bytes.Buffer
	run(context.Background(), replFor("exit\n"), &out, cfgFor("m"), "/etc/metron.json", testEnv(t), &fakeStepper{}, false)

	if !strings.Contains(out.String(), "config: /etc/metron.json") {
		t.Fatalf("output = %q, want the config path in the banner", out.String())
	}
}

func TestRunWarnsAboutMissingDependencies(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))
	var out bytes.Buffer

	run(context.Background(), replFor("exit\n"), &out, cfgFor("m"), "", testEnv(t), &fakeStepper{}, false)

	for _, want := range []string{"rg not found", "ctags not found", "git not found"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want a warning containing %q", out.String(), want)
		}
	}
}

func TestRunDoesNotSendCommandsToTheModel(t *testing.T) {
	bot := &fakeStepper{reply: "ok"}
	var out bytes.Buffer

	run(context.Background(), replFor("/help\n/reset\n/nope\nreal question\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	if len(bot.prompts) != 1 || bot.prompts[0] != "real question" {
		t.Fatalf("prompts = %q, want only the non-command line forwarded", bot.prompts)
	}
	if bot.resets != 1 {
		t.Fatalf("resets = %d, want 1", bot.resets)
	}
}

func TestCommandHelp(t *testing.T) {
	var out bytes.Buffer

	if quit := command(&out, "/help", config.Defaults(), "", testEnv(t), &fakeStepper{}); quit {
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

	command(&out, "/reset", config.Defaults(), "", testEnv(t), bot)

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

	command(&out, "/config", cfg, "/tmp/metron.json", testEnv(t), &fakeStepper{})

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

	command(&out, "/config", config.Defaults(), "", testEnv(t), &fakeStepper{})

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
	command(&out, "/tags", config.Defaults(), "", testEnv(t), &fakeStepper{})

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
	command(&out, "/tags", config.Defaults(), "", testEnv(t), &fakeStepper{})

	if !strings.Contains(out.String(), "Error:") {
		t.Fatalf("output = %q, want the rebuild failure reported", out.String())
	}
}

func TestCommandUnknownSlashCommand(t *testing.T) {
	var out bytes.Buffer

	command(&out, "/bogus", config.Defaults(), "", testEnv(t), &fakeStepper{})

	if !strings.Contains(out.String(), "Unknown command") {
		t.Fatalf("output = %q, want an unknown-command notice", out.String())
	}
}

func TestCommandIgnoresPlainInput(t *testing.T) {
	var out bytes.Buffer

	if quit := command(&out, "explain the loop", config.Defaults(), "", testEnv(t), &fakeStepper{}); quit {
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
	code := runMain(nil, strings.NewReader("exit\n"), &out, &errOut)

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
	code := runMain(nil, strings.NewReader(""), &out, &errOut)

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

	// main reads os.Args; under `go test` those are the test binary's own
	// flags, which metron's flag set would rightly reject.
	oldArgs := os.Args
	os.Args = []string{"metron"}
	t.Cleanup(func() { os.Args = oldArgs })

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

	showConfig(&w, config.Defaults(), "somewhere", &fakeStepper{})

	if !strings.Contains(w.buf.String(), "source: somewhere") {
		t.Fatalf("output = %q, want the source line written", w.buf.String())
	}
	if !strings.Contains(w.buf.String(), "disk full") {
		t.Fatalf("output = %q, want the encoding failure reported", w.buf.String())
	}
}

func TestCommandHistoryReportsTheBudget(t *testing.T) {
	var out bytes.Buffer
	bot := &fakeStepper{msgs: 12, bytes: 3400}
	cfg := cfgFor("m")
	cfg.MaxHistoryMessages = 60

	command(&out, "/history", cfg, "", testEnv(t), bot)

	for _, want := range []string{"12 messages", "3400 bytes", "budget: 60"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("/history = %q, missing %q", out.String(), want)
		}
	}
}

func TestApproveAcceptsYes(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "  YES  \n"} {
		var out bytes.Buffer
		if !approve(&out, replFor(answer), "--- a/x\n+++ b/x\n") {
			t.Fatalf("approve(%q) = false, want the patch approved", answer)
		}
		if !strings.Contains(out.String(), "--- a/x") {
			t.Fatalf("approve() output = %q, want the diff shown first", out.String())
		}
	}
}

func TestApproveRejectsAnythingElse(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "no\n", "later\n"} {
		var out bytes.Buffer
		if approve(&out, replFor(answer), "diff") {
			t.Fatalf("approve(%q) = true, want anything but yes to decline", answer)
		}
		if !strings.Contains(out.String(), "not applied") {
			t.Fatalf("approve() output = %q, want the refusal reported", out.String())
		}
	}
}

func TestApproveTreatsEOFAsNo(t *testing.T) {
	var out bytes.Buffer
	if approve(&out, replFor(""), "diff") {
		t.Fatal("approve() = true at EOF, want an absent operator to decline")
	}
	if !strings.Contains(out.String(), "No input") {
		t.Fatalf("approve() output = %q, want the reason reported", out.String())
	}
}

func TestStepReturnsTheAgentResult(t *testing.T) {
	bot := &fakeStepper{reply: "answer"}

	got, err := step(context.Background(), bot, "hello")

	if err != nil || got != "answer" {
		t.Fatalf("step() = (%q, %v), want the agent reply", got, err)
	}
}

func TestStepPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bot := &contextStepper{}

	_, err := step(ctx, bot, "hello")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("step() error = %v, want context.Canceled", err)
	}
}

func TestRunReportsACancelledTurnAndKeepsGoing(t *testing.T) {
	var out bytes.Buffer
	bot := &fakeStepper{err: context.Canceled}

	run(context.Background(), replFor("first\nsecond\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	if !strings.Contains(out.String(), "cancelled") {
		t.Fatalf("output = %q, want the cancellation reported", out.String())
	}
	if len(bot.prompts) != 2 {
		t.Fatalf("prompts = %v, want the REPL still accepting input after a cancel", bot.prompts)
	}
}

// contextStepper reports whatever the turn context is carrying, so the signal
// plumbing in step can be observed without raising a real SIGINT.
type contextStepper struct{}

func (contextStepper) Step(ctx context.Context, _ string) (string, error) { return "", ctx.Err() }
func (contextStepper) Reset()                                             {}
func (contextStepper) HistorySize() (int, int)                            { return 0, 0 }
func (contextStepper) LastUsage() (ollama.Usage, int)                     { return ollama.Usage{}, 0 }
func (contextStepper) AdvertisedTools() ([]string, int)                   { return nil, 0 }

// signallingStepper raises a real SIGINT from inside the turn, which the
// handler step installs is expected to intercept and turn into a cancellation
// rather than letting it kill the test binary.
type signallingStepper struct{}

func (signallingStepper) Step(ctx context.Context, _ string) (string, error) {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return "", err
	}
	if err := p.Signal(os.Interrupt); err != nil {
		return "", err
	}
	<-ctx.Done()
	return "", ctx.Err()
}
func (signallingStepper) Reset()                           {}
func (signallingStepper) HistorySize() (int, int)          { return 0, 0 }
func (signallingStepper) LastUsage() (ollama.Usage, int)   { return ollama.Usage{}, 0 }
func (signallingStepper) AdvertisedTools() ([]string, int) { return nil, 0 }

func TestStepCancelsTheTurnOnInterrupt(t *testing.T) {
	_, err := step(context.Background(), signallingStepper{}, "slow request")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("step() error = %v, want the interrupt to cancel the turn", err)
	}
}

func TestRunMainWiresThePatchApprovalPrompt(t *testing.T) {
	dir := gitDir(t)
	t.Setenv("METRON_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	diff := "--- a/x.txt\n+++ b/x.txt\n@@ -1 +1 @@\n-a\n+b\n"
	script := []string{
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"apply_patch","arguments":` +
			mustJSONMain(t, map[string]any{"diff": diff}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"Nothing changed."},"done":true}`,
	}
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if turn >= len(script) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(script[turn]))
		turn++
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL+"/api/chat")
	t.Setenv("OLLAMA_MODEL", "approval-model")

	var out, errOut bytes.Buffer
	// The request, then "n" answering the approval prompt, then quit.
	code := runMain(nil, strings.NewReader("change x\nn\nexit\n"), &out, &errOut)

	if code != 0 {
		t.Fatalf("runMain() = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Apply this patch?") {
		t.Fatalf("stdout = %q, want the approval prompt", out.String())
	}
	if !strings.Contains(out.String(), "Patch not applied.") {
		t.Fatalf("stdout = %q, want the refusal reported", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("x.txt exists, want a refused patch to leave the tree untouched")
	}
}

func mustJSONMain(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExampleConfigMatchesDefaults guards the documented example against drift:
// config loading rejects unknown fields, so a key that outlives the struct
// turns the file metron tells people to copy into a startup error.
func TestExampleConfigMatchesDefaults(t *testing.T) {
	// The example lives at the repository root, two levels above this package.
	path, err := filepath.Abs(filepath.Join("..", "..", "metron.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METRON_CONFIG", path)
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("OLLAMA_MODEL", "")

	cfg, used, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() error = %v, want the example file to load cleanly", err)
	}
	if used != path {
		t.Fatalf("config.Load() used %q, want the example file", used)
	}
	if !reflect.DeepEqual(cfg, config.Defaults()) {
		t.Fatalf("example config = %+v, want it to reproduce the defaults exactly", cfg)
	}

	// Equality above cannot catch a *new* setting, since an absent key simply
	// keeps its default. Compare key sets so the example stays exhaustive.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var documented map[string]any
	if err := json.Unmarshal(raw, &documented); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	for key := range actual {
		if _, ok := documented[key]; !ok {
			t.Errorf("metron.example.json is missing the %q setting", key)
		}
	}
}

func TestRunReportsTokenUsage(t *testing.T) {
	var out bytes.Buffer
	bot := &fakeStepper{reply: "ok", usage: ollama.Usage{PromptTokens: 1240, GenTokens: 89}, calls: 3}

	run(context.Background(), replFor("hi\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	for _, want := range []string{"1240 prompt", "89 generated", "3 tool calls"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, missing %q", out.String(), want)
		}
	}
}

func TestRunStaysQuietWhenTheServerReportsNoCounts(t *testing.T) {
	var out bytes.Buffer
	bot := &fakeStepper{reply: "ok"}

	run(context.Background(), replFor("hi\n"), &out, cfgFor("m"), "", testEnv(t), bot, false)

	if strings.Contains(out.String(), "tool calls") {
		t.Fatalf("output = %q, want no usage line when there is nothing to report", out.String())
	}
}

func TestRunWarnsWhenThePromptCrowdsTheContextWindow(t *testing.T) {
	var out bytes.Buffer
	cfg := cfgFor("m")
	cfg.NumCtx = 1000
	bot := &fakeStepper{reply: "ok", usage: ollama.Usage{PromptTokens: 900, GenTokens: 10}}

	run(context.Background(), replFor("hi\n"), &out, cfg, "", testEnv(t), bot, false)

	if !strings.Contains(out.String(), "900 of 1000 context tokens") {
		t.Fatalf("output = %q, want the context-pressure warning", out.String())
	}
}

func TestRunDoesNotWarnBelowTheContextThreshold(t *testing.T) {
	var out bytes.Buffer
	cfg := cfgFor("m")
	cfg.NumCtx = 1000
	bot := &fakeStepper{reply: "ok", usage: ollama.Usage{PromptTokens: 100, GenTokens: 10}}

	run(context.Background(), replFor("hi\n"), &out, cfg, "", testEnv(t), bot, false)

	// Startup dependency warnings are unrelated; only the context one matters.
	if strings.Contains(out.String(), "context tokens") {
		t.Fatalf("output = %q, want no context warning well under the window", out.String())
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   flags
		wantOK bool
		code   int
	}{
		{"no flags", nil, flags{}, true, 0},
		{"short prompt", []string{"-p", "fix it"}, flags{prompt: "fix it"}, true, 0},
		{"long prompt", []string{"--prompt", "fix it"}, flags{prompt: "fix it"}, true, 0},
		{"yes", []string{"--yes"}, flags{yes: true}, true, 0},
		{"version", []string{"--version"}, flags{showVersion: true}, true, 0},
		{"help stops without an error", []string{"-h"}, flags{}, false, 0},
		{"unknown flag is an error", []string{"--nope"}, flags{}, false, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			got, code, ok := parseFlags(tc.args, &errOut)
			if ok != tc.wantOK || code != tc.code {
				t.Fatalf("parseFlags() = (_, %d, %v), want (_, %d, %v)", code, ok, tc.code, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("parseFlags() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseFlagsUsageNamesTheFlags(t *testing.T) {
	var errOut bytes.Buffer
	parseFlags([]string{"-h"}, &errOut)

	for _, want := range []string{"usage: metron", "-p", "-yes", "-version"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("usage = %q, missing %q", errOut.String(), want)
		}
	}
}

func TestRunMainPrintsTheVersion(t *testing.T) {
	var out, errOut bytes.Buffer

	if code := runMain([]string{"--version"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("runMain(--version) = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "metron ") {
		t.Fatalf("stdout = %q, want the version printed", out.String())
	}
}

func TestRunMainRejectsUnknownFlags(t *testing.T) {
	var out, errOut bytes.Buffer

	if code := runMain([]string{"--nope"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("runMain(--nope) = %d, want 2", code)
	}
}

// oneShotServer scripts a fake Ollama and points the environment at it.
func oneShotServer(t *testing.T, script []string) {
	t.Helper()
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if turn >= len(script) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(script[turn]))
		turn++
	}))
	t.Cleanup(srv.Close)
	t.Setenv("METRON_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OLLAMA_HOST", srv.URL+"/api/chat")
	t.Setenv("OLLAMA_MODEL", "one-shot-model")
}

func TestRunMainOneShotPrintsOnlyTheAnswerOnStdout(t *testing.T) {
	t.Chdir(t.TempDir())
	oneShotServer(t, []string{
		`{"message":{"role":"assistant","content":"greet.go defines Greet."},"done":true}`,
	})

	var out, errOut bytes.Buffer
	code := runMain([]string{"-p", "where is Greet?"}, strings.NewReader(""), &out, &errOut)

	if code != 0 {
		t.Fatalf("runMain(-p) = %d, want 0", code)
	}
	if out.String() != "greet.go defines Greet.\n" {
		t.Fatalf("stdout = %q, want only the answer", out.String())
	}
}

func TestRunMainOneShotReportsFailuresOnStderr(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("METRON_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1/api/chat")
	t.Setenv("OLLAMA_MODEL", "unreachable")

	var out, errOut bytes.Buffer
	code := runMain([]string{"-p", "anything"}, strings.NewReader(""), &out, &errOut)

	if code != 1 {
		t.Fatalf("runMain(-p) = %d, want 1 when the model is unreachable", code)
	}
	if !strings.Contains(errOut.String(), "error:") {
		t.Fatalf("stderr = %q, want the failure reported", errOut.String())
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want it empty on failure", out.String())
	}
}

func TestRunMainOneShotRefusesPatchesWithoutYes(t *testing.T) {
	dir := gitDir(t)
	diff := "--- a/x.txt\n+++ b/x.txt\n@@ -1 +1 @@\n-a\n+b\n"
	oneShotServer(t, []string{
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"apply_patch","arguments":` +
			mustJSONMain(t, map[string]any{"diff": diff}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"I would change x.txt."},"done":true}`,
	})

	var out, errOut bytes.Buffer
	code := runMain([]string{"-p", "change x"}, strings.NewReader(""), &out, &errOut)

	if code != 0 {
		t.Fatalf("runMain(-p) = %d, want 0", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("x.txt exists, want unattended runs to fail closed without --yes")
	}
	if !strings.Contains(errOut.String(), "[executing: apply_patch]") {
		t.Fatalf("stderr = %q, want progress kept off stdout in one-shot mode", errOut.String())
	}
}

func TestRunMainOneShotAppliesPatchesWithYes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeGitRepoForTest(t)
	diff := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-alpha\n+omega\n"
	oneShotServer(t, []string{
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"apply_patch","arguments":` +
			mustJSONMain(t, map[string]any{"diff": diff}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"Changed target.txt."},"done":true}`,
	})

	var out, errOut bytes.Buffer
	code := runMain([]string{"-p", "change it", "--yes"}, strings.NewReader(""), &out, &errOut)

	if code != 0 {
		t.Fatalf("runMain(-p --yes) = %d, want 0", code)
	}
	b, err := os.ReadFile(filepath.Join(dir, "target.txt"))
	if err != nil || string(b) != "omega\n" {
		t.Fatalf("target.txt = %q (err %v), want the patch applied", b, err)
	}
}

// writeGitRepoForTest makes the working directory a repo with one tracked file.
func writeGitRepoForTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if err := os.WriteFile("target.txt", []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "target.txt"},
		{"commit", "-qm", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestRunDoesNotReprintAStreamedReply(t *testing.T) {
	var out bytes.Buffer
	bot := &fakeStepper{reply: "already streamed"}

	run(context.Background(), replFor("hi\n"), &out, cfgFor("m"), "", testEnv(t), bot, true)

	// The client's sink wrote the text; run must not write it again.
	if strings.Contains(out.String(), "already streamed") {
		t.Fatalf("output = %q, want no second copy of a streamed reply", out.String())
	}
}

func TestRunMainStreamsIntoStdoutByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	oneShotServer(t, []string{
		`{"message":{"role":"assistant","content":"streamed "},"done":false}` + "\n" +
			`{"message":{"role":"assistant","content":"answer"},"done":true}` + "\n",
	})

	var out, errOut bytes.Buffer
	if code := runMain(nil, strings.NewReader("hi\nexit\n"), &out, &errOut); code != 0 {
		t.Fatalf("runMain() = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "streamed answer") {
		t.Fatalf("stdout = %q, want the streamed chunks joined", out.String())
	}
	if strings.Count(out.String(), "streamed answer") != 1 {
		t.Fatalf("stdout = %q, want the reply printed exactly once", out.String())
	}
}

func TestCommandConfigReportsTheAdvertisedTools(t *testing.T) {
	var out bytes.Buffer
	bot := &fakeStepper{advertised: []string{"view_slice", "apply_patch"}, schemaBytes: 400}

	command(&out, "/config", config.Defaults(), "", testEnv(t), bot)

	// The schemas are sent with every request, so their cost is a standing one
	// and worth showing next to the budgets it competes with.
	for _, want := range []string{"view_slice, apply_patch", "400 schema bytes", "~100 tokens"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("/config output = %q, missing %q", out.String(), want)
		}
	}
}

func TestCommandConfigSaysWhenNoToolsAreAdvertised(t *testing.T) {
	var out bytes.Buffer

	command(&out, "/config", config.Defaults(), "", testEnv(t), &fakeStepper{})

	if !strings.Contains(out.String(), "tools: none advertised") {
		t.Fatalf("/config output = %q, want the empty tool set stated", out.String())
	}
}
