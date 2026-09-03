package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mdabydeen/metron/internal/ollama"
)

// fakeChatter replays a scripted sequence of model replies and records the
// history it was handed on each call.
type fakeChatter struct {
	replies []ollama.Message
	usage   ollama.Usage
	err     error
	calls   int
	seen    [][]ollama.Message
	tools   []ollama.Tool
}

func (f *fakeChatter) Chat(ctx context.Context, messages []ollama.Message, tools []ollama.Tool) (*ollama.Reply, error) {
	f.calls++
	f.seen = append(f.seen, append([]ollama.Message(nil), messages...))
	f.tools = tools
	if f.err != nil {
		return nil, f.err
	}
	if len(f.replies) == 0 {
		return &ollama.Reply{Message: ollama.Message{Role: "assistant", Content: "done"}, Usage: f.usage}, nil
	}
	next := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	return &ollama.Reply{Message: next, Usage: f.usage}, nil
}

func toolCall(name string, args map[string]any) ollama.Message {
	var tc ollama.ToolCall
	tc.Function.Name = name
	tc.Function.Arguments = args
	return ollama.Message{Role: "assistant", ToolCalls: []ollama.ToolCall{tc}}
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

func TestNewSeedsSystemPrompt(t *testing.T) {
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
	fake := &fakeChatter{replies: []ollama.Message{{Role: "assistant", Content: "hello"}}}
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
	fake := &fakeChatter{replies: []ollama.Message{
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

	fake := &fakeChatter{replies: []ollama.Message{multi, {Role: "assistant", Content: "ok"}}}
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
	fake := &fakeChatter{replies: []ollama.Message{loopForever}}
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
	a := New(&fakeChatter{}, DefaultOptions())

	tests := []struct {
		name string
		call ollama.Message
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
			got := a.dispatch(tc.call.ToolCalls[0])
			if !strings.Contains(got, tc.want) {
				t.Fatalf("dispatch() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestDispatchFindSymbolReportsToolError(t *testing.T) {
	isolate(t) // no .tags and no ctags binary
	a := New(&fakeChatter{}, DefaultOptions())

	got := a.dispatch(toolCall("find_symbol", map[string]any{"symbol": "Alpha"}).ToolCalls[0])
	if !strings.HasPrefix(got, "Error:") {
		t.Fatalf("dispatch() = %q, want the ctags failure reported as text", got)
	}
}

func TestDispatchAppliesPatch(t *testing.T) {
	t.Chdir(t.TempDir())
	writeGitRepo(t)
	a := New(&fakeChatter{}, DefaultOptions())

	diff := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-alpha\n+omega\n"
	got := a.dispatch(toolCall("apply_patch", map[string]any{"diff": diff}).ToolCalls[0])
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

	got := a.dispatch(toolCall("list_files", map[string]any{"pattern": "*.go"}).ToolCalls[0])
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

	got := a.dispatch(toolCall("search_text", map[string]any{"pattern": "needle"}).ToolCalls[0])
	if got != "./a.go:1:needle" {
		t.Fatalf("dispatch() = %q, want the ripgrep output", got)
	}
}

func TestCompactContextRedactsOnlyLargeSlices(t *testing.T) {
	big := strings.Repeat("    1 | filler line\n", 40) // > 400 chars, contains " | "
	a := &Agent{opts: DefaultOptions(), messages: []ollama.Message{
		{Role: "tool", Content: big},
		{Role: "tool", Content: strings.Repeat("no delimiter here ", 40)},
		{Role: "tool", Content: "    1 | short slice"},
		{Role: "assistant", Content: big},
	}}

	a.compactContext()

	if a.messages[0].Content != "[File slice redacted after turn completion]" {
		t.Errorf("large slice = %q, want it redacted", a.messages[0].Content)
	}
	if strings.HasPrefix(a.messages[1].Content, "[File slice") {
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
	fake := &fakeChatter{replies: []ollama.Message{
		toolCall("view_slice", map[string]any{"path": "big.go", "start": 1, "end": 60}),
		{Role: "assistant", Content: "summarised"},
	}}
	a := New(fake, DefaultOptions())

	if _, err := a.Step(context.Background(), "read it"); err != nil {
		t.Fatal(err)
	}
	if a.messages[3].Content != "[File slice redacted after turn completion]" {
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
	if a.messages[0].Content != systemPrompt {
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
	a := New(&fakeChatter{}, opts)

	a.dispatch(toolCall("find_symbol", map[string]any{"symbol": "Greet"}).ToolCalls[0])

	if !strings.Contains(progress.String(), "[executing: find_symbol]") {
		t.Fatalf("progress = %q, want the tool execution notice", progress.String())
	}
}

func TestDispatchDiscardsProgressWhenNoWriterIsSet(t *testing.T) {
	isolate(t)
	a := New(&fakeChatter{}, DefaultOptions())

	// The nil-writer path must not panic and must not reach for stdout.
	if got := a.dispatch(toolCall("find_symbol", map[string]any{"symbol": "X"}).ToolCalls[0]); got == "" {
		t.Fatal("dispatch() = \"\", want the tool result regardless of progress writer")
	}
}

func TestDispatchAsksBeforeApplyingAPatch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	var seen string
	opts := DefaultOptions()
	opts.Approve = func(diff string) bool {
		seen = diff
		return false
	}
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(toolCall("apply_patch", map[string]any{"diff": "--- a/x\n+++ b/x\n"}).ToolCalls[0])

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
	opts.Approve = func(string) bool {
		approved = true
		return true
	}
	a := New(&fakeChatter{}, opts)

	diff := "--- a/target.txt\n+++ b/target.txt\n@@ -1 +1 @@\n-alpha\n+omega\n"
	got := a.dispatch(toolCall("apply_patch", map[string]any{"diff": diff}).ToolCalls[0])

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
	client := &fakeChatter{replies: []ollama.Message{
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
		a.messages = append(a.messages, ollama.Message{Role: "user", Content: strconv.Itoa(i)})
	}

	a.trimHistory()

	if len(a.messages) != 5 {
		t.Fatalf("history = %d messages, want the system prompt plus 4", len(a.messages))
	}
	if a.messages[0].Role != "system" {
		t.Fatalf("history[0].Role = %q, want the system prompt kept", a.messages[0].Role)
	}
	if a.messages[1].Content != "6" || a.messages[4].Content != "9" {
		t.Fatalf("history = %v, want the newest messages retained", a.messages[1:])
	}
}

func TestTrimHistoryNeverLeavesAnOrphanToolResult(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxHistoryMessages = 3
	a := New(&fakeChatter{}, opts)
	a.messages = append(a.messages,
		ollama.Message{Role: "user", Content: "old"},
		toolCall("find_symbol", map[string]any{"symbol": "X"}),
		ollama.Message{Role: "tool", ToolName: "find_symbol", Content: "r1"},
		ollama.Message{Role: "tool", ToolName: "find_symbol", Content: "r2"},
		ollama.Message{Role: "assistant", Content: "answer"},
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
	a.messages = append(a.messages, ollama.Message{Role: "user", Content: "hi"})

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
		a.messages = append(a.messages, ollama.Message{Role: "user", Content: "x"})
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
	a.messages = []ollama.Message{{Role: "system", Content: "abc"}, {Role: "user", Content: "de"}}

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
	opts.MaxSliceLines = 1
	a := New(&fakeChatter{}, opts)

	got := a.dispatch(toolCall("view_slice", map[string]any{"path": "sample.go", "start": 1, "end": 4}).ToolCalls[0])
	if !strings.Contains(got, "<= 1 lines") {
		t.Fatalf("dispatch() = %q, want the configured slice budget enforced", got)
	}
}

func TestOptionsBoundMaxTurns(t *testing.T) {
	isolate(t)
	opts := DefaultOptions()
	opts.MaxTurns = 2
	fake := &fakeChatter{replies: []ollama.Message{toolCall("search_text", map[string]any{"pattern": "x"})}}

	if _, err := New(fake, opts).Step(context.Background(), "spin"); err == nil {
		t.Fatal("Step() = nil error, want max turns exceeded")
	}
	if fake.calls != 2 {
		t.Fatalf("model calls = %d, want the configured limit of 2", fake.calls)
	}
}

func TestOptionsBoundCompactionThreshold(t *testing.T) {
	a := &Agent{opts: Options{CompactThreshold: 10}, messages: []ollama.Message{
		{Role: "tool", Content: "    1 | just over ten characters"},
	}}

	a.compactContext()

	if a.messages[0].Content != "[File slice redacted after turn completion]" {
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
	opts.SearchMaxMatches, opts.SearchMaxPerFile = 3, 1
	a := New(&fakeChatter{}, opts)

	a.dispatch(toolCall("search_text", map[string]any{"pattern": "needle"}).ToolCalls[0])

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
		usage: ollama.Usage{PromptTokens: 100, GenTokens: 10},
		replies: []ollama.Message{
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
	client := &fakeChatter{usage: ollama.Usage{PromptTokens: 7, GenTokens: 3}}
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
