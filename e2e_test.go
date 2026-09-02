package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"metron/internal/agent"
	"metron/internal/config"
	"metron/internal/ollama"
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
	// A pre-built .tags file keeps the test independent of the host's ctags.
	if err := os.WriteFile(".tags", []byte(
		"Greet\tgreet.go\t/^func Greet$/;\"\tkind:func\tline:3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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

	bot := agent.New(ollama.NewClient(srv.URL+"/api/chat", "e2e-model", ollama.DefaultOptions()), agent.DefaultOptions())
	var out bytes.Buffer
	cfg := config.Defaults()
	cfg.Model = "e2e-model"
	run(context.Background(), strings.NewReader("make Greet say hola\nexit\n"), &out, cfg, "", bot)

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
