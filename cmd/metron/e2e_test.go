package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdabydeen/metron/agent"
	"github.com/mdabydeen/metron/internal/config"
	"github.com/mdabydeen/metron/llm"
	"github.com/mdabydeen/metron/ollama"
	"github.com/mdabydeen/metron/tools"
)

// TestEndToEndPatchSession wires the real HTTP client, agent loop and tools
// together against a scripted Ollama server, and checks that a full
// locate -> read -> patch session edits the file on disk.
func TestEndToEndPatchSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	t.Chdir(dir)
	source := "package main\n\nfunc Greet() string {\n\treturn \"hello\"\n}\n"
	if err := os.WriteFile("greet.go", []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	// A pre-built .tags file keeps the test independent of the host's ctags --
	// but find_symbol is only advertised when a *Universal* ctags is on PATH, so
	// a shim answering --version is prepended. It is never invoked for anything
	// else, since .tags already exists. The rest of PATH is kept, because git is
	// used for real here.
	if err := os.WriteFile(".tags", []byte(
		"Greet\tgreet.go\t/^func Greet$/;\"\tkind:func\tline:3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shimBin := filepath.Join(dir, "shim-bin")
	if err := os.MkdirAll(shimBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimBin, "ctags"),
		[]byte("#!/bin/sh\necho 'Universal Ctags 6.1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "greet.go"},
		{"-c", "commit.gpgsign=false", "commit", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	diff := "--- a/greet.go\n+++ b/greet.go\n@@ -1,5 +1,5 @@\n" +
		" package main\n \n func Greet() string {\n-\treturn \"hello\"\n+\treturn \"hola\"\n }\n"

	script := []string{
		`{"message":{"role":"assistant","content":"","tool_calls":[
			{"function":{"name":"find_symbol","arguments":{"symbol":"Greet"}}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[
			{"function":{"name":"view_slice","arguments":{"path":"greet.go","start":1,"end":5}}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[
			{"function":{"name":"apply_patch","arguments":` +
			mustJSON(t, map[string]any{"diff": diff}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"Greet now returns hola."},"done":true}`,
	}

	var turn int
	var toolResults []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
		}
		if turn >= len(script) {
			t.Errorf("model called %d times, script has %d replies", turn+1, len(script))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(script[turn]))
		turn++
	}))
	defer srv.Close()

	bot := agent.New(ollama.NewClient(srv.URL+"/api/chat", "e2e-model", llm.DefaultOptions()), agent.DefaultOptions())
	var out bytes.Buffer
	cfg := config.Defaults()
	cfg.Model = "e2e-model"
	run(context.Background(), bufio.NewScanner(strings.NewReader("make Greet say hola\nexit\n")), &out, cfg, "", testEnv(t), bot, testRecorder(t), false)

	if turn != len(script) {
		t.Fatalf("model was called %d times, want %d", turn, len(script))
	}
	if !strings.Contains(out.String(), "Greet now returns hola.") {
		t.Fatalf("REPL output = %q, want the final answer", out.String())
	}

	b, err := os.ReadFile("greet.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `return "hola"`) {
		t.Fatalf("greet.go = %q, want the patch applied on disk", b)
	}

	// The tool results the model actually saw, in order.
	joined := strings.Join(toolResults, "\n")
	for _, want := range []string{"Greet [func] -> greet.go:3", `    4 | 	return "hello"`, "successfully applied"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tool results %q missing %q", joined, want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEndToEndRunCommandSession drives the real client, agent and tools through
// a session that edits a file and then runs a command to check the edit --
// the loop metron could not close before run_command existed.
func TestEndToEndRunCommandSession(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("answer.txt", []byte("41\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@e.com"}, {"config", "user.name", "t"},
		{"add", "answer.txt"}, {"-c", "commit.gpgsign=false", "commit", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	diff := "--- a/answer.txt\n+++ b/answer.txt\n@@ -1 +1 @@\n-41\n+42\n"
	script := []string{
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"apply_patch","arguments":` +
			mustJSON(t, map[string]any{"diff": diff}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"run_command","arguments":` +
			mustJSON(t, map[string]any{"command": "cat answer.txt"}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"answer.txt now reads 42."},"done":true}`,
	}
	var turn int
	var toolResults []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
		}
		if turn >= len(script) {
			t.Errorf("model called %d times, script has %d", turn+1, len(script))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(script[turn]))
		turn++
	}))
	defer srv.Close()

	env := tools.NewEnv(tools.DefaultBudgets())
	env.Allowed = tools.ParseAllowlist([]string{"cat"})
	opts := agent.DefaultOptions()
	opts.Env = env
	bot := agent.New(ollama.NewClient(srv.URL+"/api/chat", "m", llm.DefaultOptions()), opts)

	var out bytes.Buffer
	cfg := config.Defaults()
	run(context.Background(), bufio.NewScanner(strings.NewReader("fix the answer\nexit\n")),
		&out, cfg, "", env, bot, testRecorder(t), false)

	if turn != len(script) {
		t.Fatalf("model called %d times, want %d", turn, len(script))
	}
	joined := strings.Join(toolResults, "\n")
	if !strings.Contains(joined, "successfully applied") {
		t.Fatalf("tool results %q missing the patch result", joined)
	}
	// The point of the whole PR: the agent saw the effect of its own edit.
	if !strings.Contains(joined, "exit status 0") || !strings.Contains(joined, "42") {
		t.Fatalf("tool results %q missing the verified command output", joined)
	}
}

// TestEndToEndSearchReplaceSession drives a full session in the search/replace
// edit format. The point of the format is that the model never has to produce a
// line number or a hunk header -- it quotes lines it has already read -- so the
// scripted reply here contains neither.
func TestEndToEndSearchReplaceSession(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	source := "package main\n\nfunc Greet() string {\n\treturn \"hello\"\n}\n"
	if err := os.WriteFile("greet.go", []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	script := []string{
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"view_slice",
			"arguments":{"path":"greet.go","start":1,"end":5}}}]},"done":true}`,
		`{"message":{"role":"assistant","tool_calls":[{"function":{"name":"edit_file","arguments":` +
			mustJSON(t, map[string]any{
				"path":    "greet.go",
				"search":  "\treturn \"hello\"",
				"replace": "\treturn \"hola\"",
			}) + `}}]},"done":true}`,
		`{"message":{"role":"assistant","content":"Greet now returns hola."},"done":true}`,
	}

	var turn int
	var toolResults []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults = append(toolResults, m.Content)
			}
		}
		if turn >= len(script) {
			t.Errorf("model called %d times, script has %d", turn+1, len(script))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(script[turn]))
		turn++
	}))
	defer srv.Close()

	env := tools.NewEnv(tools.DefaultBudgets())
	env.EditFormat = tools.FormatSearchReplace
	opts := agent.DefaultOptions()
	opts.Env = env
	bot := agent.New(ollama.NewClient(srv.URL+"/api/chat", "m", llm.DefaultOptions()), opts)

	var out bytes.Buffer
	cfg := config.Defaults()
	cfg.EditFormat = tools.FormatSearchReplace
	run(context.Background(), bufio.NewScanner(strings.NewReader("make Greet say hola\nexit\n")),
		&out, cfg, "", env, bot, testRecorder(t), false)

	if turn != len(script) {
		t.Fatalf("model called %d times, want %d", turn, len(script))
	}
	b, err := os.ReadFile("greet.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `return "hola"`) {
		t.Fatalf("greet.go = %q, want the edit applied on disk", b)
	}
	// git is never invoked in this format, so the whole edit path works in a
	// directory that is not a repository at all.
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatal("expected no git repository in this test")
	}
	if !strings.Contains(strings.Join(toolResults, "\n"), "Edited greet.go") {
		t.Fatalf("tool results %q missing the edit confirmation", toolResults)
	}
}
