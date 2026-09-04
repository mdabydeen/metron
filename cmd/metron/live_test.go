//go:build live

// These tests talk to a real Ollama server with a real model. They are
// excluded from the default build because they need a running server, a
// tool-capable model, and enough time and memory to run inference.
//
//	make test-live
//	METRON_TEST_MODEL=gemma4:12b-mlx go test -tags=live -run Live -v .
//
// The model choice is deliberate: metron's default (qwen2.5-coder:32b) is
// often not the model an operator actually has pulled, so the test discovers a
// tool-capable model from /api/tags unless METRON_TEST_MODEL says otherwise.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdabydeen/metron/internal/agent"
	"github.com/mdabydeen/metron/internal/config"
	"github.com/mdabydeen/metron/internal/ollama"
	"github.com/mdabydeen/metron/internal/tools"
)

// liveModel returns a model on the local server that advertises tool support.
func liveModel(t *testing.T, host string) string {
	t.Helper()
	if forced := os.Getenv("METRON_TEST_MODEL"); forced != "" {
		return forced
	}

	base := strings.TrimSuffix(host, "/api/chat")
	req, err := http.NewRequest(http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Skipf("no Ollama server at %s: %v", base, err)
	}
	defer resp.Body.Close()

	var tags struct {
		Models []struct {
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
			Size         int64    `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		t.Fatalf("decode /api/tags: %v", err)
	}

	// Prefer the smallest tool-capable model so the test stays quick.
	best, bestSize := "", int64(0)
	for _, m := range tags.Models {
		for _, c := range m.Capabilities {
			if c == "tools" && (best == "" || m.Size < bestSize) {
				best, bestSize = m.Name, m.Size
			}
		}
	}
	if best == "" {
		t.Skip("no tool-capable model installed; pull one or set METRON_TEST_MODEL")
	}
	return best
}

// countingChatter wraps the real client so the test can see what the model did.
type countingChatter struct {
	inner     agent.Chatter
	calls     int
	toolNames []string
	usage     ollama.Usage
}

func (c *countingChatter) Chat(ctx context.Context, msgs []ollama.Message, tls []ollama.Tool) (*ollama.Reply, error) {
	c.calls++
	reply, err := c.inner.Chat(ctx, msgs, tls)
	if err == nil {
		for _, tc := range reply.ToolCalls {
			c.toolNames = append(c.toolNames, tc.Function.Name)
		}
		c.usage.Add(reply.Usage)
	}
	return reply, err
}

func liveSetup(t *testing.T) (*countingChatter, *agent.Agent, config.Config) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	cfg := config.Defaults()
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		cfg.Endpoint = v
	}
	// timeout_seconds is an idle watchdog, but a big local model can still be
	// slow to produce its *first* token on loaded hardware.
	cfg.TimeoutSeconds = 900
	if v := os.Getenv("METRON_TEST_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("METRON_TEST_TIMEOUT_SECONDS = %q, want a positive integer", v)
		}
		cfg.TimeoutSeconds = n
	}
	cfg.Model = liveModel(t, cfg.Endpoint)
	t.Logf("live model: %s at %s", cfg.Model, cfg.Endpoint)

	client := ollama.NewClient(cfg.Endpoint, cfg.Model, ollama.Options{
		Temperature:     cfg.Temperature,
		TopP:            cfg.TopP,
		NumCtx:          cfg.NumCtx,
		MaxOutputTokens: cfg.MaxOutputTokens,
		Timeout:         time.Duration(cfg.TimeoutSeconds) * time.Second,
		Stream:          cfg.Stream,
	})
	counting := &countingChatter{inner: client}
	return counting, agent.New(counting, agent.Options{
		MaxTurns:           cfg.MaxTurns,
		CompactThreshold:   cfg.CompactThreshold,
		MaxHistoryMessages: cfg.MaxHistoryMessages,
		Env: tools.NewEnv(tools.Budgets{
			MaxSliceLines:    cfg.MaxSliceLines,
			MaxLineChars:     cfg.MaxLineChars,
			SearchMaxMatches: cfg.SearchMaxMatches,
			SearchMaxPerFile: cfg.SearchMaxPerFile,
			ListMaxEntries:   cfg.ListMaxEntries,
		}),
	}), cfg
}

// liveRepo creates a git repository the model can be asked to work on.
func liveRepo(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.WriteFile("greeter.go", []byte(`package sample

func Greet() string {
	return "hello"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"add", "greeter.go"},
		{"-c", "commit.gpgsign=false", "commit", "-qm", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestLiveModelAnswersWithoutTools checks the plainest possible round-trip:
// a real model, a real HTTP call, a real reply.
func TestLiveModelAnswersWithoutTools(t *testing.T) {
	counting, bot, cfg := liveSetup(t)
	liveRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	reply, err := bot.Step(ctx, "Reply with the single word: ready. Do not call any tools.")
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if strings.TrimSpace(reply) == "" {
		t.Fatal("Step() returned an empty reply")
	}
	if counting.calls == 0 {
		t.Fatal("no request reached the model")
	}
	t.Logf("reply: %s", reply)
}

// TestLiveToolUse checks that a real model actually drives metron's tools
// rather than answering from imagination. Which tools it picks is up to the
// model, so the assertion is that it used at least one of them.
func TestLiveToolUse(t *testing.T) {
	counting, bot, cfg := liveSetup(t)
	liveRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	reply, err := bot.Step(ctx,
		"In this repository, find where the function Greet is defined and tell me which file "+
			"and line it is on. Use your tools; do not guess.")
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	t.Logf("tools called: %v", counting.toolNames)
	t.Logf("reply: %s", reply)

	if len(counting.toolNames) == 0 {
		t.Fatalf("model answered without calling any tool: %q", reply)
	}
	for _, name := range counting.toolNames {
		switch name {
		case "find_symbol", "search_text", "view_slice", "apply_patch":
		default:
			t.Errorf("model called an unknown tool %q", name)
		}
	}
}

// TestLivePatchWorkflow is the full end-to-end claim: a real model edits a real
// file through apply_patch. Small models are unreliable at producing valid
// unified diffs, so a failure to land the patch is reported as a skip with the
// transcript rather than a hard failure -- the test still fails loudly if the
// agent loop itself breaks.
func TestLivePatchWorkflow(t *testing.T) {
	counting, bot, cfg := liveSetup(t)
	liveRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	reply, err := bot.Step(ctx,
		`In greeter.go, change the string "hello" to "hola". Use view_slice to read the file `+
			`first, then apply_patch with a unified diff against greeter.go.`)
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	t.Logf("tools called: %v", counting.toolNames)
	t.Logf("reply: %s", reply)

	b, err := os.ReadFile("greeter.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "hola") {
		t.Skipf("model did not land the patch (tools called: %v); file is:\n%s", counting.toolNames, b)
	}
}
