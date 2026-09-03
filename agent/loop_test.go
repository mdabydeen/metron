package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mdabydeen/metron/llm"
	"github.com/mdabydeen/metron/tools"
)

// fakeChatter replays a scripted sequence of model replies and records the
// history it was handed on each call.
type fakeChatter struct {
	replies []llm.Message
	usage   llm.Usage
	err     error
	calls   int
	seen    [][]llm.Message
	tools   []llm.Tool
}

func (f *fakeChatter) Chat(ctx context.Context, messages []llm.Message, tools []llm.Tool) (*llm.Reply, error) {
	f.calls++
	f.seen = append(f.seen, append([]llm.Message(nil), messages...))
	f.tools = tools
	if f.err != nil {
		return nil, f.err
	}
	if len(f.replies) == 0 {
		return &llm.Reply{Message: llm.Message{Role: "assistant", Content: "done"}, Usage: f.usage}, nil
	}
	next := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return &llm.Reply{Message: next, Usage: f.usage}, nil
}

func toolCall(name string, args map[string]any) llm.Message {
	var tc llm.ToolCall
	tc.Function.Name = name
	tc.Function.Arguments = args
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}}
}

// isolate moves the test into an empty directory with no external binaries on
// PATH, so tool dispatch deterministically fails rather than touching the repo.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("PATH", filepath.Join(dir, "empty-bin"))
	return dir
}

// equipped is isolate with stand-ins for every external binary on PATH, so the
// agent finds nothing missing and advertises the full tool set.
func equipped(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		// echo is shimmed too: equipped replaces PATH wholesale, so a test that
		// actually runs a command needs one that exists inside it.
		"echo":  "printf '%s\\n' \"$*\"\n",
		"rg":    "exit 0\n",
		"ctags": "case \"$1\" in --version) echo 'Universal Ctags 6.1.0'; exit 0;; esac\nexit 0\n",
		"git": "case \"$1 $2\" in\n" +
			"  \"rev-parse --is-inside-work-tree\") echo true; exit 0;;\n" +
			"  \"rev-parse --show-toplevel\") pwd; exit 0;;\n" +
			"esac\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return dir
}

// fullAgent advertises every tool whatever the environment holds. Dispatch
// tests are about routing and about failures being reported as text the model
// can read; which tools are on offer is a separate question, covered by the
// advertisement tests.
func fullAgent(client Chatter, opts Options) *Agent {
	a := New(client, opts)
	a.advertised = tools.ToolNames
	a.schemas = a.schemas[:0]
	for _, name := range tools.ToolNames {
		a.schemas = append(a.schemas, toolDefs[name])
	}
	a.unavailable = nil
	a.messages[0].Content = systemPrompt(tools.ToolNames)
	return a
}

func TestNewSeedsSystemPrompt(t *testing.T) {
	equipped(t)
	a := New(&fakeChatter{}, DefaultOptions())

	if len(a.messages) != 1 || a.messages[0].Role != "system" {
		t.Fatalf("New() history = %+v, want a single system message", a.messages)
	}
	for _, want := range []string{"list_files", "find_symbol", "search_text", "view_slice", "apply_patch"} {
		if !strings.Contains(a.messages[0].Content, want) {
			t.Errorf("system prompt missing mention of %q", want)
		}
	}
}

func TestStepReturnsAnswerWithoutToolCalls(t *testing.T) {
	fake := &fakeChatter{replies: []llm.Message{{Role: "assistant", Content: "hello"}}}
	a := New(fake, DefaultOptions())

	got, err := a.Step(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if got != "hello" {
		t.Fatalf("Step() = %q, want %q", got, "hello")
	}
	if fake.calls != 1 {
		t.Fatalf("model calls = %d, want 1", fake.calls)
	}
	if len(a.messages) != 3 ||
		a.messages[1].Role != "user" || a.messages[1].Content != "hi" ||
		a.messages[2].Content != "hello" {
		t.Fatalf("history = %+v, want system/user/assistant", a.messages)
	}
}

func TestStepAdvertisesEveryTool(t *testing.T) {
	equipped(t)
	fake := &fakeChatter{}
	if _, err := New(fake, DefaultOptions()).Step(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	if len(fake.tools) != 5 {
		t.Fatalf("advertised %d tools, want 5", len(fake.tools))
	}
	seen := map[string]bool{}
	for _, tool := range fake.tools {
		if tool.Type != "function" {
			t.Errorf("tool type = %q, want function", tool.Type)
		}
		fn, ok := tool.Function.(map[string]any)
		if !ok {
			t.Fatalf("tool function = %T, want a schema map", tool.Function)
		}
		name, _ := fn["name"].(string)
		seen[name] = true
		if _, ok := fn["parameters"]; !ok {
			t.Errorf("tool %q has no parameters schema", name)
		}
		if desc, _ := fn["description"].(string); desc == "" {
			t.Errorf("tool %q has no description", name)
		}
	}
	for _, want := range []string{"list_files", "find_symbol", "search_text", "view_slice", "apply_patch"} {
		if !seen[want] {
			t.Errorf("tool %q not advertised", want)
		}
	}
}

func TestStepRunsToolThenAnswers(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeChatter{replies: []llm.Message{
		toolCall("view_slice", map[string]any{"path": "sample.go", "start": float64(1), "end": float64(2)}),
		{Role: "assistant", Content: "the file starts with one"},
	}}
	a := New(fake, DefaultOptions())

	got, err := a.Step(context.Background(), "what is in sample.go?")
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if got != "the file starts with one" {
		t.Fatalf("Step() = %q", got)
	}
	if fake.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (tool round-trip)", fake.calls)
	}
	// system, user, assistant(tool call), tool, assistant(answer)
	if len(a.messages) != 5 {
		t.Fatalf("history = %+v, want 5 messages", a.messages)
	}
	if a.messages[3].Role != "tool" || !strings.Contains(a.messages[3].Content, "one") {
		t.Fatalf("tool message = %+v, want the slice output", a.messages[3])
	}
}

func TestStepRunsEveryToolCallInOneReply(t *testing.T) {
	isolate(t)
	multi := toolCall("view_slice", map[string]any{"path": "nope.go", "start": float64(1), "end": float64(2)})
	second := multi.ToolCalls[0]
	second.Function.Name = "bogus_tool"
	multi.ToolCalls = append(multi.ToolCalls, second)

	fake := &fakeChatter{replies: []llm.Message{multi, {Role: "assistant", Content: "ok"}}}
	a := New(fake, DefaultOptions())

	if _, err := a.Step(context.Background(), "go"); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if a.messages[3].Role != "tool" || a.messages[4].Role != "tool" {
		t.Fatalf("history = %+v, want one tool message per call", a.messages)
	}
	if !strings.Contains(a.messages[4].Content, "Unknown tool bogus_tool") {
		t.Fatalf("second tool message = %q, want the unknown-tool report", a.messages[4].Content)
	}
}

func TestStepPropagatesClientError(t *testing.T) {
	want := errors.New("ollama down")
	a := New(&fakeChatter{err: want}, DefaultOptions())

	_, err := a.Step(context.Background(), "hi")
	if !errors.Is(err, want) {
		t.Fatalf("Step() error = %v, want %v", err, want)
	}
}

func TestStepStopsAfterMaxTurns(t *testing.T) {
	isolate(t)
	loopForever := toolCall("search_text", map[string]any{"pattern": "x"})
	fake := &fakeChatter{replies: []llm.Message{loopForever}}
	a := New(fake, DefaultOptions())

	_, err := a.Step(context.Background(), "spin")
	if err == nil || !strings.Contains(err.Error(), "max turns exceeded") {
		t.Fatalf("Step() error = %v, want a max-turns error", err)
	}
	if fake.calls != DefaultOptions().MaxTurns {
		t.Fatalf("model calls = %d, want %d", fake.calls, DefaultOptions().MaxTurns)
	}
}

func TestStepKeepsHistoryAcrossTurns(t *testing.T) {
	fake := &fakeChatter{}
	a := New(fake, DefaultOptions())

	if _, err := a.Step(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Step(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}

	if len(fake.seen[1]) <= len(fake.seen[0]) {
		t.Fatalf("second call saw %d messages, want more than the first (%d)",
			len(fake.seen[1]), len(fake.seen[0]))
	}
	if fake.seen[1][1].Content != "first" {
		t.Fatalf("second call history = %+v, want the first turn retained", fake.seen[1])
	}
}

func TestDispatchRoutesToolsAndReportsErrors(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".tags"), []byte(
		"Alpha\tsample.go\t/^Alpha$/;\"\tkind:func\tline:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := fullAgent(&fakeChatter{}, DefaultOptions())

	tests := []struct {
		name string
		call llm.Message
		want string
	}{
		{"find_symbol hit", toolCall("find_symbol", map[string]any{"symbol": "Alpha"}), "sample.go:1"},
		{"find_symbol miss", toolCall("find_symbol", map[string]any{"symbol": "Zeta"}), "not found"},
		{"view_slice", toolCall("view_slice", map[string]any{"path": "sample.go", "start": 1, "end": 1}), "alpha"},
		{"view_slice error", toolCall("view_slice", map[string]any{"path": "gone.go", "start": 1, "end": 1}), "Error:"},
		{"search_text error", toolCall("search_text", map[string]any{"pattern": "x"}), "Error:"},
		{"list_files error", toolCall("list_files", map[string]any{"pattern": "*.go"}), "Error:"},
		{"apply_patch error", toolCall("apply_patch", map[string]any{"diff": "junk"}), "Error:"},
		{"unknown tool", toolCall("mystery", nil), "Unknown tool mystery"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.dispatch(context.Background(), tc.call.ToolCalls[0])
			if !strings.Contains(got, tc.want) {
				t.Fatalf("dispatch() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestDispatchFindSymbolReportsToolError(t *testing.T) {
	isolate(t) // no .tags and no ctags binary
	a := fullAgent(&fakeChatter{}, DefaultOptions())

	got := a.dispatch(context.Background(), toolCall("find_symbol", map[string]any{"symbol": "Alpha"}).ToolCalls[0])
	if !strings.HasPrefix(got, "Error:") {
		t.Fatalf("dispatch() = %q, want the ctags failure reported as text", got)
	}
}

func TestDispatchAppliesPatch(t *testing.T) {
	t.Chdir(t.TempDir())
	writeGitRepo(t)
	a := New(&fakeChatter{}, DefaultOptions())

	diff := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-alpha\n+omega\n"
	got := a.dispatch(context.Background(), toolCall("apply_patch", map[string]any{"diff": diff}).ToolCalls[0])
	if !strings.Contains(got, "successfully applied") {
		t.Fatalf("dispatch() = %q, want a success report", got)
	}
	b, err := os.ReadFile("target.txt")
	if err != nil || string(b) != "omega\n" {
		t.Fatalf("target.txt = %q (err %v), want the patch applied", b, err)
	}
}

func TestDispatchListFilesSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte("#!/bin/sh\necho 'main.go'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	a := New(&fakeChatter{}, DefaultOptions())

	got := a.dispatch(context.Background(), toolCall("list_files", map[string]any{"pattern": "*.go"}).ToolCalls[0])
	if got != "main.go" {
		t.Fatalf("dispatch() = %q, want the file listing", got)
	}
}

func TestDispatchSearchTextSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte("#!/bin/sh\necho './a.go:1:needle'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	a := New(&fakeChatter{}, DefaultOptions())

	got := a.dispatch(context.Background(), toolCall("search_text", map[string]any{"pattern": "needle"}).ToolCalls[0])
	if got != "./a.go:1:needle" {
		t.Fatalf("dispatch() = %q, want the ripgrep output", got)
	}
}

func TestCompactContextRedactsOnlyLargeSlices(t *testing.T) {
	big := strings.Repeat("    1 | filler line\n", 40) // > 400 chars, contains " | "
	a := &Agent{opts: DefaultOptions(), messages: []llm.Message{
		{Role: "tool", Content: big},
		{Role: "tool", Content: strings.Repeat("no delimiter here ", 40)},
		{Role: "tool", Content: "    1 | short slice"},
		{Role: "assistant", Content: big},
	}}

	a.compactContext()

	if !strings.Contains(a.messages[0].Content, "redacted after the turn") {
		t.Errorf("large slice = %q, want it redacted", a.messages[0].Content)
	}
	if strings.Contains(a.messages[1].Content, "redacted after the turn") {
		t.Error("large non-slice tool output was redacted, want it kept")
	}
	if a.messages[2].Content != "    1 | short slice" {
		t.Error("small slice was redacted, want it kept")
	}
	if a.messages[3].Content != big {
		t.Error("assistant message was redacted, want only tool messages touched")
	}
}

func TestStepCompactsSlicesOnceTurnCompletes(t *testing.T) {
	dir := isolate(t)
	long := strings.Repeat("a line of source code\n", 60)
	if err := os.WriteFile(filepath.Join(dir, "big.go"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeChatter{replies: []llm.Message{
		toolCall("view_slice", map[string]any{"path": "big.go", "start": 1, "end": 60}),
		{Role: "assistant", Content: "summarised"},
	}}
	a := New(fake, DefaultOptions())

	if _, err := a.Step(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.messages[3].Content, "redacted after the turn") {
		t.Fatalf("tool message = %q, want it compacted after the turn", a.messages[3].Content)
	}
	// The model still saw the full slice during the turn.
	if len(fake.seen[1][3].Content) < 400 {
		t.Fatal("model was handed the compacted placeholder mid-turn")
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
	}{
		{"json number", float64(42), 42},
		{"native int", 7, 7},
		{"numeric string", "13", 13},
		{"unparsable string", "twelve", 0},
		{"nil", nil, 0},
		{"unsupported type", []int{1}, 0},
		{"bool", true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt(tc.in); got != tc.want {
				t.Fatalf("toInt(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// writeGitRepo creates a one-file git repository in the current directory.
func writeGitRepo(t *testing.T) {
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
		{"-c", "commit.gpgsign=false", "commit", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestResetClearsHistoryButKeepsTheSystemPrompt(t *testing.T) {
	fake := &fakeChatter{}
	a := New(fake, DefaultOptions())
	if _, err := a.Step(context.Background(), "remember this"); err != nil {
		t.Fatal(err)
	}
	if len(a.messages) == 1 {
		t.Fatal("precondition failed: history did not grow")
	}

	a.Reset()

	if len(a.messages) != 1 || a.messages[0].Role != "system" {
		t.Fatalf("history after Reset = %+v, want only the system prompt", a.messages)
	}
	if a.messages[0].Content != systemPrompt(a.advertised) {
		t.Fatal("Reset did not restore the system prompt")
	}

	// The agent keeps working, and the model no longer sees the old turn.
	if _, err := a.Step(context.Background(), "fresh start"); err != nil {
		t.Fatal(err)
	}
	for _, m := range fake.seen[len(fake.seen)-1] {
		if m.Content == "remember this" {
			t.Fatal("history from before Reset was still sent to the model")
		}
	}
}

func TestDispatchWritesProgressToTheConfiguredWriter(t *testing.T) {
	isolate(t)
	var progress bytes.Buffer
	opts := DefaultOptions()
	opts.Progress = &progress
	a := fullAgent(&fakeChatter{}, opts)

	a.dispatch(context.Background(), toolCall("find_symbol", map[string]any{"symbol": "Greet"}).ToolCalls[0])

	if !strings.Contains(progress.String(), "[executing: find_symbol]") {
		t.Fatalf("progress = %q, want the tool execution notice", progress.String())
	}
}

func TestDispatchDiscardsProgressWhenNoWriterIsSet(t *testing.T) {
	isolate(t)
	a := fullAgent(&fakeChatter{}, DefaultOptions())

	// The nil-writer path must not panic and must not reach for stdout.
	if got := a.dispatch(context.Background(), toolCall("find_symbol", map[string]any{"symbol": "X"}).ToolCalls[0]); got == "" {
		t.Fatal("dispatch() = \"\", want the tool result regardless of progress writer")
	}
}

func TestDispatchAsksBeforeApplyingAPatch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var seen string
	opts := DefaultOptions()
	opts.Approve = func(_, preview string) bool {
		seen = preview
		return false
	}
	a := fullAgent(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(), toolCall("apply_patch", map[string]any{"diff": "--- a/x\n+++ b/x\n"}).ToolCalls[0])

	if seen != "--- a/x\n+++ b/x\n" {
		t.Fatalf("approver saw %q, want the model's diff", seen)
	}
	if !strings.Contains(got, "rejected by the operator") {
		t.Fatalf("dispatch() = %q, want a refusal the model can read", got)
	}
	if !strings.Contains(got, "Do not retry") {
		t.Fatalf("dispatch() = %q, want the model told not to retry", got)
	}
}

func TestDispatchAppliesWhenTheOperatorApproves(t *testing.T) {
	t.Chdir(t.TempDir())
	writeGitRepo(t)

	opts := DefaultOptions()
	approved := false
	opts.Approve = func(string, string) bool {
		approved = true
		return true
	}
	a := New(&fakeChatter{}, opts)

	diff := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-alpha\n+omega\n"
	got := a.dispatch(context.Background(), toolCall("apply_patch", map[string]any{"diff": diff}).ToolCalls[0])

	if !approved {
		t.Fatal("approver was not consulted")
	}
	if !strings.Contains(got, "successfully applied") {
		t.Fatalf("dispatch() = %q, want the patch applied", got)
	}
	body, err := os.ReadFile("target.txt")
	if err != nil || string(body) != "omega\n" {
		t.Fatalf("target.txt = %q (err %v), want the patched content", body, err)
	}
}

func TestStepLabelsToolResultsWithTheirToolName(t *testing.T) {
	isolate(t)
	client := &fakeChatter{replies: []llm.Message{
		toolCall("find_symbol", map[string]any{"symbol": "Greet"}),
		{Role: "assistant", Content: "done"},
	}}
	a := New(client, DefaultOptions())

	if _, err := a.Step(context.Background(), "where is Greet"); err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	for _, m := range a.messages {
		if m.Role == "tool" {
			if m.ToolName != "find_symbol" {
				t.Fatalf("tool message ToolName = %q, want find_symbol", m.ToolName)
			}
			return
		}
	}
	t.Fatal("no tool message in history")
}

func TestTrimHistoryKeepsTheSystemPromptAndTheNewestMessages(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxHistoryMessages = 4
	a := New(&fakeChatter{}, opts)
	for i := 0; i < 10; i++ {
		a.messages = append(a.messages, llm.Message{Role: "user", Content: strconv.Itoa(i)})
	}

	a.trimHistory()

	// The prompt, one line saying what was dropped, then the budgeted tail.
	if len(a.messages) != 6 {
		t.Fatalf("history = %d messages, want the prompt, an elision note and 4", len(a.messages))
	}
	if a.messages[0].Role != "system" {
		t.Fatalf("history[0].Role = %q, want the system prompt kept", a.messages[0].Role)
	}
	// Vanishing the earlier conversation silently leaves the model reasoning
	// from a history it believes is complete.
	if !strings.Contains(a.messages[1].Content, "earlier messages elided") {
		t.Fatalf("history[1] = %q, want the gap stated", a.messages[1].Content)
	}
	if a.messages[2].Content != "6" || a.messages[5].Content != "9" {
		t.Fatalf("history = %v, want the newest messages retained", a.messages[2:])
	}
}

func TestTrimHistoryNeverLeavesAnOrphanToolResult(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxHistoryMessages = 3
	a := New(&fakeChatter{}, opts)
	a.messages = append(a.messages,
		llm.Message{Role: "user", Content: "old"},
		toolCall("find_symbol", map[string]any{"symbol": "X"}),
		llm.Message{Role: "tool", ToolName: "find_symbol", Content: "r1"},
		llm.Message{Role: "tool", ToolName: "find_symbol", Content: "r2"},
		llm.Message{Role: "assistant", Content: "answer"},
	)

	a.trimHistory()

	if a.messages[1].Role == "tool" {
		t.Fatalf("history = %v, want no tool result without the call it answers", a.messages[1:])
	}
	if a.messages[len(a.messages)-1].Content != "answer" {
		t.Fatal("trimming dropped the newest message")
	}
}

func TestTrimHistoryLeavesShortHistoriesAlone(t *testing.T) {
	a := New(&fakeChatter{}, DefaultOptions())
	a.messages = append(a.messages, llm.Message{Role: "user", Content: "hi"})

	a.trimHistory()

	if len(a.messages) != 2 {
		t.Fatalf("history = %d messages, want it untouched", len(a.messages))
	}
}

func TestTrimHistoryTreatsANonPositiveBudgetAsUnlimited(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxHistoryMessages = 0
	a := New(&fakeChatter{}, opts)
	for i := 0; i < 50; i++ {
		a.messages = append(a.messages, llm.Message{Role: "user", Content: "x"})
	}

	a.trimHistory()

	if len(a.messages) != 51 {
		t.Fatalf("history = %d messages, want an unlimited budget to keep them all", len(a.messages))
	}
}

func TestTrimHistoryHandlesEmptyHistory(t *testing.T) {
	a := New(&fakeChatter{}, DefaultOptions())
	a.messages = nil

	a.trimHistory()

	if len(a.messages) != 0 {
		t.Fatalf("history = %v, want it left empty", a.messages)
	}
}

func TestHistorySizeReportsCountAndBytes(t *testing.T) {
	a := New(&fakeChatter{}, DefaultOptions())
	a.messages = []llm.Message{{Role: "system", Content: "abc"}, {Role: "user", Content: "de"}}

	msgs, bytes := a.HistorySize()

	if msgs != 2 || bytes != 5 {
		t.Fatalf("HistorySize() = (%d, %d), want (2, 5)", msgs, bytes)
	}
}

func TestOptionsBoundToolBudgets(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.Env.Budgets.MaxSliceLines = 1
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(), toolCall("view_slice", map[string]any{"path": "sample.go", "start": 1, "end": 4}).ToolCalls[0])
	if !strings.Contains(got, "<= 1 lines") {
		t.Fatalf("dispatch() = %q, want the configured slice budget enforced", got)
	}
}

func TestOptionsBoundMaxTurns(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.MaxTurns = 2
	fake := &fakeChatter{replies: []llm.Message{toolCall("search_text", map[string]any{"pattern": "x"})}}

	if _, err := New(fake, opts).Step(context.Background(), "spin"); err == nil {
		t.Fatal("Step() = nil error, want max turns exceeded")
	}
	if fake.calls != 2 {
		t.Fatalf("model calls = %d, want the configured limit of 2", fake.calls)
	}
}

func TestOptionsBoundCompactionThreshold(t *testing.T) {
	a := &Agent{opts: Options{CompactThreshold: 10}, messages: []llm.Message{
		{Role: "tool", Content: "    1 | just over ten characters"},
	}}

	a.compactContext()

	if !strings.Contains(a.messages[0].Content, "redacted after the turn") {
		t.Fatalf("message = %q, want the configured threshold applied", a.messages[0].Content)
	}
}

func TestSearchBudgetsReachRipgrep(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + filepath.Join(dir, "argv") + "\necho hit\n"
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	opts := DefaultOptions()
	opts.Env.Budgets.SearchMaxMatches, opts.Env.Budgets.SearchMaxPerFile = 3, 1
	a := New(&fakeChatter{}, opts)

	a.dispatch(context.Background(), toolCall("search_text", map[string]any{"pattern": "needle"}).ToolCalls[0])

	argv, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--max-count=1") {
		t.Fatalf("ripgrep argv = %q, want the configured per-file budget", argv)
	}
}

func TestLastUsageSumsEveryCallInTheTurn(t *testing.T) {
	isolate(t)
	client := &fakeChatter{
		usage: llm.Usage{PromptTokens: 100, GenTokens: 10},
		replies: []llm.Message{
			toolCall("find_symbol", map[string]any{"symbol": "Greet"}),
			{Role: "assistant", Content: "done"},
		},
	}
	a := New(client, DefaultOptions())

	if _, err := a.Step(context.Background(), "hi"); err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	usage, calls := a.LastUsage()
	// Two model calls at 100/10 each, one tool call between them.
	if usage.PromptTokens != 200 || usage.GenTokens != 20 {
		t.Fatalf("LastUsage() = %+v, want both calls counted", usage)
	}
	if calls != 1 {
		t.Fatalf("tool calls = %d, want 1", calls)
	}
}

func TestLastUsageResetsBetweenTurns(t *testing.T) {
	isolate(t)
	client := &fakeChatter{usage: llm.Usage{PromptTokens: 7, GenTokens: 3}}
	a := New(client, DefaultOptions())

	for i := 0; i < 2; i++ {
		if _, err := a.Step(context.Background(), "hi"); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
	}

	usage, _ := a.LastUsage()
	if usage.PromptTokens != 7 || usage.GenTokens != 3 {
		t.Fatalf("LastUsage() = %+v, want only the most recent turn", usage)
	}
}

func TestNewAdvertisesOnlyUsableTools(t *testing.T) {
	isolate(t) // nothing on PATH: only view_slice can run

	a := New(&fakeChatter{}, DefaultOptions())

	names, schemaBytes := a.AdvertisedTools()
	if len(names) != 1 || names[0] != tools.ToolViewSlice {
		t.Fatalf("AdvertisedTools() = %v, want only view_slice", names)
	}
	if schemaBytes <= 0 {
		t.Fatalf("AdvertisedTools() schema bytes = %d, want the cost reported", schemaBytes)
	}
	// The prompt must not name tools the model has no schema for; doing so
	// spends tokens describing what it cannot call, and invites it to try.
	for _, absent := range []string{tools.ToolFindSymbol, tools.ToolSearchText, tools.ToolApplyPatch} {
		if strings.Contains(a.messages[0].Content, absent) {
			t.Errorf("system prompt names %q, which is not advertised:\n%s", absent, a.messages[0].Content)
		}
	}
}

func TestStepSendsOnlyTheAdvertisedSchemas(t *testing.T) {
	isolate(t)
	fake := &fakeChatter{}

	if _, err := New(fake, DefaultOptions()).Step(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	if len(fake.tools) != 1 {
		t.Fatalf("sent %d schemas, want only the usable one", len(fake.tools))
	}
}

func TestDisabledToolsAreNotAdvertised(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.DisabledTools = []string{tools.ToolApplyPatch, tools.ToolSearchText}

	a := New(&fakeChatter{}, opts)

	names, _ := a.AdvertisedTools()
	for _, name := range names {
		if name == tools.ToolApplyPatch || name == tools.ToolSearchText {
			t.Fatalf("AdvertisedTools() = %v, want the disabled tools withheld", names)
		}
	}
	if len(names) != 3 {
		t.Fatalf("AdvertisedTools() = %v, want the remaining three", names)
	}
}

func TestDispatchRefusesAToolThatWasNotAdvertised(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())

	// The schema was never sent, but a model can still invent the call. It must
	// be turned down before it runs, and told not to try again.
	got := a.dispatch(context.Background(), toolCall("search_text", map[string]any{"pattern": "x"}).ToolCalls[0])

	if !strings.Contains(got, "unavailable in this project") {
		t.Fatalf("dispatch() = %q, want a refusal naming the cause", got)
	}
	if !strings.Contains(got, "Do not retry") {
		t.Fatalf("dispatch() = %q, want the model told not to retry", got)
	}
}

func TestDispatchRefusesADisabledTool(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.DisabledTools = []string{tools.ToolSearchText}
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(), toolCall("search_text", map[string]any{"pattern": "x"}).ToolCalls[0])

	if !strings.Contains(got, "disabled by the operator") {
		t.Fatalf("dispatch() = %q, want the refusal to distinguish a choice from a missing binary", got)
	}
}

func TestResetKeepsTheAdvertisedPrompt(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())
	seeded := a.messages[0].Content

	if _, err := a.Step(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	a.Reset()

	if len(a.messages) != 1 || a.messages[0].Content != seeded {
		t.Fatalf("Reset() prompt = %q, want the advertised set preserved", a.messages[0].Content)
	}
}

func TestSystemPromptNumbersDirectivesContinuously(t *testing.T) {
	got := systemPrompt([]string{tools.ToolViewSlice, tools.ToolApplyPatch})

	for _, want := range []string{"1. NEVER guess", "2. Use 'view_slice'", "3. Use 'apply_patch'", "4. Report what changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("systemPrompt() = %q, missing %q", got, want)
		}
	}
}

func TestRunCommandIsWithheldUntilCommandsAreAllowed(t *testing.T) {
	equipped(t)

	// The default: nothing allowed, so the tool is not offered at all and its
	// schema is not paid for on any request.
	a := New(&fakeChatter{}, DefaultOptions())
	names, _ := a.AdvertisedTools()
	for _, name := range names {
		if name == tools.ToolRunCommand {
			t.Fatalf("AdvertisedTools() = %v, want run_command withheld by default", names)
		}
	}

	got := a.dispatch(context.Background(), toolCall("run_command", map[string]any{"command": "go test"}).ToolCalls[0])
	if !strings.Contains(got, "no commands are permitted") {
		t.Fatalf("dispatch() = %q, want the reason stated", got)
	}
}

func TestRunCommandSchemaNamesTheAllowedCommands(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.Env.Allowed = tools.ParseAllowlist([]string{"go test", "go vet"})

	a := New(&fakeChatter{}, opts)

	var desc string
	for _, schema := range a.schemas {
		fn := schema.Function.(map[string]any)
		if fn["name"] == tools.ToolRunCommand {
			desc = fn["description"].(string)
		}
	}
	// Without the allowlist in the description the model guesses, and every
	// guess costs a turn to be refused.
	for _, want := range []string{`"go test"`, `"go vet"`} {
		if !strings.Contains(desc, want) {
			t.Errorf("run_command description = %q, missing %q", desc, want)
		}
	}
}

func TestDescribeToolDoesNotMutateSharedSchemas(t *testing.T) {
	equipped(t)
	base := toolDefs[tools.ToolRunCommand].Function.(map[string]any)["description"].(string)

	opts := DefaultOptions()
	opts.Env.Allowed = tools.ParseAllowlist([]string{"make"})
	New(&fakeChatter{}, opts)

	// toolDefs is package state shared by every agent in the process; a second
	// agent must not inherit the first one's allowlist.
	if got := toolDefs[tools.ToolRunCommand].Function.(map[string]any)["description"].(string); got != base {
		t.Fatalf("toolDefs description = %q, want the package copy untouched", got)
	}
}

func TestDispatchAsksBeforeRunningACommand(t *testing.T) {
	equipped(t)
	var kind, preview string
	opts := DefaultOptions()
	opts.Env.Allowed = tools.ParseAllowlist([]string{"echo"})
	opts.Approve = func(k, p string) bool {
		kind, preview = k, p
		return false
	}
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(), toolCall("run_command", map[string]any{"command": "echo hi"}).ToolCalls[0])

	if kind != "command" || preview != "echo hi" {
		t.Fatalf("approver saw (%q, %q), want the command announced as such", kind, preview)
	}
	if !strings.Contains(got, "rejected by the operator") || !strings.Contains(got, "Do not retry") {
		t.Fatalf("dispatch() = %q, want a refusal the model can act on", got)
	}
}

func TestDispatchRunsAnApprovedCommand(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.Env.Allowed = tools.ParseAllowlist([]string{"echo"})
	a := New(&fakeChatter{}, opts) // nil Approve: proceed without asking

	got := a.dispatch(context.Background(), toolCall("run_command", map[string]any{"command": "echo hi"}).ToolCalls[0])

	if !strings.Contains(got, "exit status 0") {
		t.Fatalf("dispatch() = %q, want the command to have run", got)
	}
}

func TestDescribeToolLeavesOtherToolsAlone(t *testing.T) {
	equipped(t)

	got := describeTool(tools.ToolViewSlice, DefaultOptions().Env)

	if got.Function.(map[string]any)["name"] != tools.ToolViewSlice {
		t.Fatalf("describeTool(view_slice) = %+v, want the schema returned unchanged", got)
	}
}

func TestDescribeToolSurvivesAMalformedSchema(t *testing.T) {
	// toolDefs is package state; a schema whose Function is not a map must not
	// panic the constructor.
	original := toolDefs[tools.ToolRunCommand]
	t.Cleanup(func() { toolDefs[tools.ToolRunCommand] = original })
	toolDefs[tools.ToolRunCommand] = llm.Tool{Type: "function", Function: "not a map"}

	got := describeTool(tools.ToolRunCommand, DefaultOptions().Env)

	if got.Function != "not a map" {
		t.Fatalf("describeTool() = %+v, want the schema passed through untouched", got)
	}
}

func TestDispatchReportsARunCommandFailure(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.Env.Allowed = tools.ParseAllowlist([]string{"echo"})
	a := New(&fakeChatter{}, opts)

	// An empty command is a mistake the model can correct, so it comes back as
	// readable text rather than as a Go error.
	got := a.dispatch(context.Background(), toolCall("run_command", map[string]any{"command": ""}).ToolCalls[0])

	if !strings.Contains(got, "No command given") {
		t.Fatalf("dispatch() = %q, want the empty command explained", got)
	}
}

func TestEditFormatSelectsExactlyOneEditTool(t *testing.T) {
	equipped(t)

	for _, tc := range []struct {
		format          string
		want, withdrawn string
	}{
		{tools.FormatDiff, tools.ToolApplyPatch, tools.ToolEditFile},
		{tools.FormatSearchReplace, tools.ToolEditFile, tools.ToolApplyPatch},
	} {
		opts := DefaultOptions()
		opts.Env.EditFormat = tc.format
		a := New(&fakeChatter{}, opts)

		names, _ := a.AdvertisedTools()
		var sawWanted, sawWithdrawn bool
		for _, n := range names {
			sawWanted = sawWanted || n == tc.want
			sawWithdrawn = sawWithdrawn || n == tc.withdrawn
		}
		// The two do the same job. Advertising both would pay for two schemas
		// on every request and invite the model to pick the wrong one.
		if !sawWanted {
			t.Errorf("edit_format %q advertised %v, want %s present", tc.format, names, tc.want)
		}
		if sawWithdrawn {
			t.Errorf("edit_format %q advertised %v, want %s withheld", tc.format, names, tc.withdrawn)
		}
	}
}

func TestDispatchRefusesTheUnselectedEditToolWithTheAlternative(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.Env.EditFormat = tools.FormatSearchReplace
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(), toolCall("apply_patch", map[string]any{"diff": "x"}).ToolCalls[0])

	if !strings.Contains(got, "edit_file") {
		t.Fatalf("dispatch() = %q, want the refusal to name the tool that does work", got)
	}
}

func TestDispatchEditFileShowsTheOperatorADiff(t *testing.T) {
	dir := equipped(t)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var kind, preview string
	opts := DefaultOptions()
	opts.Env.EditFormat = tools.FormatSearchReplace
	opts.Approve = func(k, p string) bool {
		kind, preview = k, p
		return false
	}
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(),
		toolCall("edit_file", map[string]any{"path": "a.go", "search": "two", "replace": "TWO"}).ToolCalls[0])

	// The format exists to make the model's job easier; it must not make the
	// operator's job harder, so they still approve a unified diff.
	if kind != "patch" {
		t.Errorf("approver saw kind %q, want it presented as a patch", kind)
	}
	for _, want := range []string{"--- a/a.go", "-two", "+TWO"} {
		if !strings.Contains(preview, want) {
			t.Errorf("approver saw %q, missing %q", preview, want)
		}
	}
	if !strings.Contains(got, "rejected by the operator") {
		t.Fatalf("dispatch() = %q, want the refusal reported", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(b) != "one\ntwo\n" {
		t.Fatal("a rejected edit modified the file")
	}
}

func TestDispatchEditFileAppliesWhenApproved(t *testing.T) {
	dir := equipped(t)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.Env.EditFormat = tools.FormatSearchReplace
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(context.Background(),
		toolCall("edit_file", map[string]any{"path": "a.go", "search": "two", "replace": "TWO"}).ToolCalls[0])

	if !strings.Contains(got, "Edited a.go") {
		t.Fatalf("dispatch() = %q, want the edit applied", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(b) != "one\nTWO\n" {
		t.Fatalf("file = %q, want the edit on disk", b)
	}
}

func TestSystemPromptNamesTheSelectedEditToolOnly(t *testing.T) {
	equipped(t)
	opts := DefaultOptions()
	opts.Env.EditFormat = tools.FormatSearchReplace
	a := New(&fakeChatter{}, opts)

	if strings.Contains(a.messages[0].Content, "apply_patch") {
		t.Fatalf("system prompt names apply_patch when it is not advertised:\n%s", a.messages[0].Content)
	}
	if !strings.Contains(a.messages[0].Content, "edit_file") {
		t.Fatalf("system prompt omits edit_file:\n%s", a.messages[0].Content)
	}
}

func TestMessagesReturnsACopy(t *testing.T) {
	equipped(t)
	a := New(&fakeChatter{}, DefaultOptions())
	if _, err := a.Step(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	got := a.Messages()
	got[0].Content = "clobbered"

	// A caller serialising this to disk must not be able to corrupt the live
	// conversation, and compaction rewrites messages in place under it.
	if a.messages[0].Content == "clobbered" {
		t.Fatal("Messages() shares storage with the live history")
	}
}

func TestRestoreReplacesHistoryAndRegeneratesThePrompt(t *testing.T) {
	isolate(t) // only view_slice is usable here
	a := New(&fakeChatter{}, DefaultOptions())
	fresh := a.messages[0].Content

	a.Restore([]llm.Message{
		{Role: "system", Content: "a system prompt from a better-equipped machine mentioning search_text"},
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	})

	if len(a.messages) != 3 {
		t.Fatalf("history = %+v, want the system prompt plus two restored messages", a.messages)
	}
	// The saved prompt described the tools available on the machine that
	// recorded it. Restoring it would tell this agent to call tools it has not
	// been given.
	if a.messages[0].Content != fresh {
		t.Fatalf("system prompt = %q, want it regenerated for this machine", a.messages[0].Content)
	}
	if a.messages[1].Content != "earlier question" {
		t.Fatalf("history = %+v, want the saved turns kept", a.messages)
	}
}

func TestRestoreTrimsToTheHistoryBudget(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.MaxHistoryMessages = 4
	a := New(&fakeChatter{}, opts)

	var saved []llm.Message
	for i := 0; i < 20; i++ {
		saved = append(saved, llm.Message{Role: "user", Content: strconv.Itoa(i)})
	}
	a.Restore(saved)

	// A transcript from a session with a larger budget must not blow this one's.
	// The prompt, the elision note and the budgeted tail.
	if len(a.messages) > 6 {
		t.Fatalf("restored %d messages, want them trimmed to the budget", len(a.messages))
	}
}

func TestLastToolsRecordsWhatRan(t *testing.T) {
	dir := equipped(t)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeChatter{replies: []llm.Message{
		toolCall("view_slice", map[string]any{"path": "a.go", "start": 1, "end": 1}),
		{Role: "assistant", Content: "done"},
	}}
	a := New(fake, DefaultOptions())

	if _, err := a.Step(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}

	got := a.LastTools()
	if len(got) != 1 || got[0].Name != "view_slice" {
		t.Fatalf("LastTools() = %+v, want the dispatched call", got)
	}
	if got[0].Ms < 0 {
		t.Fatalf("LastTools() = %+v, want a sane duration", got)
	}
}

func TestLastToolsResetsEachStep(t *testing.T) {
	dir := equipped(t)
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeChatter{replies: []llm.Message{
		toolCall("view_slice", map[string]any{"path": "a.go", "start": 1, "end": 1}),
		{Role: "assistant", Content: "done"},
		{Role: "assistant", Content: "no tools this time"},
	}}
	a := New(fake, DefaultOptions())

	if _, err := a.Step(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Step(context.Background(), "just chat"); err != nil {
		t.Fatal(err)
	}

	// LastUsage and LastTools describe the most recent turn, not the session.
	if got := a.LastTools(); len(got) != 0 {
		t.Fatalf("LastTools() = %+v, want it cleared for a turn that ran none", got)
	}
}

func TestToIntRejectsEnormousNumbers(t *testing.T) {
	// JSON numbers arrive as float64. Converting a huge one straight to int
	// produces a value that then overflows in ordinary arithmetic downstream --
	// which is how a slice request once read a whole file.
	for _, v := range []any{1e19, -1e19} {
		if got := toInt(v); got != 0 {
			t.Errorf("toInt(%v) = %d, want it refused", v, got)
		}
	}
	if got := toInt(float64(42)); got != 42 {
		t.Fatalf("toInt(42) = %d, want ordinary numbers unaffected", got)
	}
}

func TestElisionNoteNamesWhatWasDropped(t *testing.T) {
	dropped := []llm.Message{
		{Role: "user", Content: "q"},
		{Role: "tool", ToolName: "view_slice"},
		{Role: "tool", ToolName: "view_slice"},
		{Role: "tool", ToolName: "search_text"},
		{Role: "assistant", Content: "a"},
	}

	got := elisionNote(dropped).Content

	// Repeats are counted; a single call is not padded with "x1".
	for _, want := range []string{"5 earlier messages elided", "view_slice x2", "search_text"} {
		if !strings.Contains(got, want) {
			t.Errorf("elisionNote() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "search_text x1") {
		t.Errorf("elisionNote() = %q, want no count on a single call", got)
	}
}

func TestElisionNoteWithNoToolCalls(t *testing.T) {
	got := elisionNote([]llm.Message{{Role: "user", Content: "q"}}).Content

	if !strings.Contains(got, "1 earlier messages elided") || strings.Contains(got, ":") {
		t.Fatalf("elisionNote() = %q, want the count with no tool list", got)
	}
}

func TestSliceHeaderFallsBackWhenThereIsNone(t *testing.T) {
	// A tool result that is numbered lines with no header still has to redact
	// to something the model can read.
	if got := sliceHeader("    1 | code\n    2 | more"); got != "a file slice" {
		t.Fatalf("sliceHeader() = %q, want the neutral description", got)
	}
	if got := sliceHeader("a.go:1-2\n    1 | code"); got != "a.go:1-2" {
		t.Fatalf("sliceHeader() = %q, want the header used", got)
	}
	if got := sliceHeader("no newline at all"); got != "a file slice" {
		t.Fatalf("sliceHeader() = %q, want the neutral description", got)
	}
}

func TestRepoMapIsInjectedWhenBudgeted(t *testing.T) {
	// isolate rather than equipped: the shimmed git answers `ls-files` with
	// nothing, so discovery would find no files. With no git at all the map
	// falls back to walking the tree.
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "widget.go"),
		[]byte("package main\n\nfunc MakeWidget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.RepoMapTokens = 400

	a := New(&fakeChatter{}, opts)

	if len(a.messages) != 2 {
		t.Fatalf("seed = %d messages, want the prompt and the map", len(a.messages))
	}
	if !strings.Contains(a.messages[1].Content, "widget.go") {
		t.Fatalf("map = %q, want the project's files", a.messages[1].Content)
	}
	// The map is orientation, not a substitute for reading: saying so is what
	// stops the model treating it as the file contents.
	if !strings.Contains(a.messages[1].Content, "not a substitute") {
		t.Fatalf("map = %q, want it framed as orientation", a.messages[1].Content)
	}
}

func TestRepoMapIsOffByDefault(t *testing.T) {
	equipped(t)

	a := New(&fakeChatter{}, DefaultOptions())

	// It costs tokens on every request of the session, so it stays off until
	// the benchmark says the turns it saves are worth more.
	if len(a.messages) != 1 {
		t.Fatalf("seed = %d messages, want only the system prompt", len(a.messages))
	}
}

func TestResetRebuildsTheRepoMap(t *testing.T) {
	// isolate rather than equipped: the shimmed git answers `ls-files` with
	// nothing, so discovery would find no files. With no git at all the map
	// falls back to walking the tree.
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "one.go"),
		[]byte("package main\n\nfunc One() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.RepoMapTokens = 400
	a := New(&fakeChatter{}, opts)

	// A file appears after the session started.
	if err := os.WriteFile(filepath.Join(dir, "two.go"),
		[]byte("package main\n\nfunc Two() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Reset()

	// The map is rebuilt against the tree as it is now, which is the point of
	// keeping it out of the system prompt.
	if !strings.Contains(a.messages[1].Content, "two.go") {
		t.Fatalf("map = %q, want it rebuilt on reset", a.messages[1].Content)
	}
}

func TestTrimHistoryKeepsTheRepoMap(t *testing.T) {
	// isolate rather than equipped: the shimmed git answers `ls-files` with
	// nothing, so discovery would find no files. With no git at all the map
	// falls back to walking the tree.
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "widget.go"),
		[]byte("package main\n\nfunc MakeWidget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	opts.RepoMapTokens = 400
	opts.MaxHistoryMessages = 2
	a := New(&fakeChatter{}, opts)
	for i := 0; i < 10; i++ {
		a.messages = append(a.messages, llm.Message{Role: "user", Content: strconv.Itoa(i)})
	}

	a.trimHistory()

	// The map is structural, not conversational: trimming it away would leave
	// the model without the orientation it was given and no way to ask for it.
	if !strings.Contains(a.messages[1].Content, "widget.go") {
		t.Fatalf("history = %+v, want the repo map kept", a.messages)
	}
	if a.messages[0].Role != "system" {
		t.Fatalf("history[0] = %+v, want the system prompt kept", a.messages[0])
	}
}

// bigSlice is a tool result large enough to matter to the budget.
func bigSlice(path string, n int) llm.Message {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s:1-%d\n", path, n)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "%5d | %s\n", i, strings.Repeat("x", 60))
	}
	return llm.Message{Role: "tool", ToolName: "view_slice", Content: sb.String()}
}

func TestBudgetShedsSlicesBeforeGivingUp(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.MaxPromptTokens = 2000
	a := New(&fakeChatter{}, opts)
	a.messages = append(a.messages,
		llm.Message{Role: "user", Content: "find the bug"},
		bigSlice("a.go", 200),
		llm.Message{Role: "assistant", Content: "looking"},
	)
	before := a.estimatePromptTokens()

	stop := a.enforceBudget()

	// Slices already read are the largest thing in the history and the most
	// re-requestable, so they go first -- and shedding them should be enough.
	if stop != "" {
		t.Fatalf("enforceBudget() = %q, want the turn allowed to continue after shedding", stop)
	}
	if a.estimatePromptTokens() >= before {
		t.Fatalf("estimate did not fall: %d -> %d", before, a.estimatePromptTokens())
	}
	if !strings.Contains(a.messages[2].Content, "a.go:1-200 redacted") {
		t.Fatalf("history = %q, want the slice purged by name", a.messages[2].Content)
	}
}

func TestBudgetStopsCleanlyWhenNothingIsLeftToShed(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	// Below the size of the schemas alone, so no amount of shedding helps.
	opts.MaxPromptTokens = 10
	a := New(&fakeChatter{}, opts)
	a.messages = append(a.messages, llm.Message{Role: "user", Content: "do the thing"})

	stop := a.enforceBudget()

	// Running out of budget is an outcome, not a malfunction.
	if stop == "" {
		t.Fatal("enforceBudget() = \"\", want the turn stopped")
	}
	for _, want := range []string{"prompt-token budget", "max_prompt_tokens", "narrow the request"} {
		if !strings.Contains(stop, want) {
			t.Errorf("stop message = %q, missing %q", stop, want)
		}
	}
}

func TestStepReturnsTheBudgetStopAsAnAnswerNotAnError(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.MaxPromptTokens = 10
	fake := &fakeChatter{}
	a := New(fake, opts)

	got, err := a.Step(context.Background(), "do the thing")

	// Returning an error would throw away everything the turn had done, and
	// the operator asked for an answer within a ceiling -- this is that answer.
	if err != nil {
		t.Fatalf("Step() error = %v, want the ceiling reported as an answer", err)
	}
	if !strings.Contains(got, "prompt-token budget") {
		t.Fatalf("Step() = %q, want it to say why it stopped", got)
	}
	if fake.calls != 0 {
		t.Fatalf("model calls = %d, want none once the ceiling is known to be unreachable", fake.calls)
	}
}

func TestNoCeilingMeansNoInterference(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions()) // MaxPromptTokens is 0
	a.messages = append(a.messages, bigSlice("a.go", 500))

	if got := a.enforceBudget(); got != "" {
		t.Fatalf("enforceBudget() = %q, want no ceiling to mean no interference", got)
	}
	// And nothing should have been shed.
	if strings.Contains(a.messages[1].Content, "redacted") {
		t.Fatal("enforceBudget() compacted history with no ceiling set")
	}
}

func TestEstimateIsCalibratedAgainstReportedCounts(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())
	a.messages = append(a.messages, llm.Message{Role: "user", Content: strings.Repeat("x", 4000)})
	start := a.bytesPerToken

	// A server reporting far more tokens than 4 bytes-per-token predicts must
	// move the estimate towards reality: the ceiling is enforced against the
	// estimate, so an estimate that never learns is a ceiling that never holds.
	a.calibrate(a.historyBytes() / 2)

	if a.bytesPerToken >= start {
		t.Fatalf("bytesPerToken = %v, want it moved towards the observed ratio from %v",
			a.bytesPerToken, start)
	}
	// One anomalous call should nudge rather than redefine.
	if a.bytesPerToken < 2.0 {
		t.Fatalf("bytesPerToken = %v, want a single observation weighted, not adopted wholesale",
			a.bytesPerToken)
	}
}

func TestCalibrateIgnoresAbsentCounts(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())
	start := a.bytesPerToken

	// Servers that report nothing must not drag the estimate to zero.
	a.calibrate(0)
	a.calibrate(-5)

	if a.bytesPerToken != start {
		t.Fatalf("bytesPerToken = %v, want it unchanged by absent counts", a.bytesPerToken)
	}
}

func TestBudgetAccountsForSchemas(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())
	withSchemas := a.historyBytes()
	a.schemas = nil
	without := a.historyBytes()

	// Schemas are sent on every request and are large enough to matter; an
	// estimate that ignored them would under-count every call.
	if withSchemas <= without {
		t.Fatalf("historyBytes = %d with schemas, %d without, want schemas counted",
			withSchemas, without)
	}
}

func TestBudgetLeavesRoomyTurnsAlone(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.MaxPromptTokens = 1_000_000 // far above anything here
	a := New(&fakeChatter{}, opts)
	a.messages = append(a.messages, bigSlice("a.go", 50))

	if got := a.enforceBudget(); got != "" {
		t.Fatalf("enforceBudget() = %q, want a turn well inside its ceiling untouched", got)
	}
	// Economising early would spend the budget's headroom for nothing.
	if strings.Contains(a.messages[1].Content, "redacted") {
		t.Fatal("enforceBudget() shed context that the ceiling did not require")
	}
}

func TestBudgetDropsOldExchangesWhenSheddingSlicesIsNotEnough(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	a := New(&fakeChatter{}, opts)
	// Bulk that compaction cannot touch: plain conversation, not file slices.
	for i := 0; i < 40; i++ {
		a.messages = append(a.messages, llm.Message{
			Role: "user", Content: strings.Repeat("y", 400) + strconv.Itoa(i),
		})
	}
	full := a.estimatePromptTokens()
	// A ceiling reachable by trimming, but not by compaction alone.
	a.opts.MaxPromptTokens = full / 2

	stop := a.enforceBudget()

	if stop != "" {
		t.Fatalf("enforceBudget() = %q, want trimming to make room", stop)
	}
	if a.estimatePromptTokens() >= a.opts.MaxPromptTokens {
		t.Fatalf("estimate %d still over the ceiling %d",
			a.estimatePromptTokens(), a.opts.MaxPromptTokens)
	}
	// The gap must be stated, not silent.
	var sawNote bool
	for _, m := range a.messages {
		sawNote = sawNote || strings.Contains(m.Content, "earlier messages elided")
	}
	if !sawNote {
		t.Fatalf("history = %+v, want the elision note", a.messages)
	}
}

func TestHistoryBytesIsNeverZero(t *testing.T) {
	// calibrate divides by this, and the ratio it produces feeds every later
	// estimate. A zero here would poison the ceiling permanently.
	if got := (&Agent{}).historyBytes(); got <= 0 {
		t.Fatalf("historyBytes() = %d for a zero Agent, want the schemas to count for something", got)
	}
}

func TestBudgetAccessors(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())

	if a.MaxPromptTokens() != 0 {
		t.Fatalf("MaxPromptTokens() = %d, want no ceiling by default", a.MaxPromptTokens())
	}
	a.SetMaxPromptTokens(5000)
	if a.MaxPromptTokens() != 5000 {
		t.Fatalf("MaxPromptTokens() = %d, want the ceiling set", a.MaxPromptTokens())
	}
	// The estimate is what the ceiling is compared against, so an operator has
	// to be able to see it.
	if a.EstimatedPromptTokens() <= 0 {
		t.Fatalf("EstimatedPromptTokens() = %d, want the seeded history counted",
			a.EstimatedPromptTokens())
	}
}

func TestSystemPromptExtraIsAppended(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.SystemPromptExtra = "Always call view_slice before editing."

	a := New(&fakeChatter{}, opts)

	// The generated contract comes first; the nudge is additional, never a
	// replacement for the tool rules the whole program depends on.
	if !strings.Contains(a.messages[0].Content, "NEVER guess") {
		t.Fatalf("prompt = %q, want the generated contract kept", a.messages[0].Content)
	}
	if !strings.HasSuffix(strings.TrimSpace(a.messages[0].Content), "Always call view_slice before editing.") {
		t.Fatalf("prompt = %q, want the nudge appended", a.messages[0].Content)
	}
}

func TestSystemPromptExtraIgnoresWhitespace(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.SystemPromptExtra = "   \n  "
	bare := New(&fakeChatter{}, DefaultOptions()).messages[0].Content

	// A blank setting must not add a trailing newline paid for on every request.
	if got := New(&fakeChatter{}, opts).messages[0].Content; got != bare {
		t.Fatalf("prompt = %q, want a whitespace-only nudge to cost nothing", got)
	}
}
