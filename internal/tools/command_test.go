package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// allowingEnv is an Env permitting exactly the given commands, rooted in a
// scratch directory.
func allowingEnv(t *testing.T, allowed ...string) Env {
	t.Helper()
	workdir(t)
	env := NewEnv(DefaultBudgets())
	env.Allowed = ParseAllowlist(allowed)
	return env
}

func TestRunCommandRunsAnAllowedCommand(t *testing.T) {
	env := allowingEnv(t, "echo")

	got := env.RunCommand(context.Background(), "echo hello")
	for _, want := range []string{"$ echo hello", "exit status 0", "hello"} {
		if !strings.Contains(got, want) {
			t.Errorf("RunCommand() = %q, missing %q", got, want)
		}
	}
}

func TestRunCommandReportsNonZeroExitAsData(t *testing.T) {
	env := allowingEnv(t, "false")

	// A failing command is the answer to a question, not a malfunction: the
	// model asked whether the tests pass, and the answer is no.
	out := env.RunCommand(context.Background(), "false")
	if !strings.Contains(out, "exit status 1") {
		t.Fatalf("RunCommand() = %q, want the exit status reported", out)
	}
}

func TestRunCommandRefusesWhatIsNotAllowed(t *testing.T) {
	env := allowingEnv(t, "go test")

	got := env.RunCommand(context.Background(), "rm -rf /")
	for _, want := range []string{"not permitted", `"go test"`, "Do not retry"} {
		if !strings.Contains(got, want) {
			t.Errorf("RunCommand() = %q, missing %q", got, want)
		}
	}
}

// TestRunCommandDoesNotInterpretShellMetacharacters is the security property
// the whole design rests on: the command string never reaches a shell, so a
// chained command is not two commands, it is one command with odd arguments.
func TestRunCommandDoesNotInterpretShellMetacharacters(t *testing.T) {
	env := allowingEnv(t, "echo")
	marker := env.Root + "/pwned"

	for _, attempt := range []string{
		"echo hi; touch " + marker,
		"echo hi && touch " + marker,
		"echo hi | tee " + marker,
		"echo hi > " + marker,
	} {
		got := env.RunCommand(context.Background(), attempt)
		// The metacharacters arrive as literal arguments to echo, so they are
		// echoed back rather than acted on.
		if !strings.Contains(got, "exit status 0") {
			t.Errorf("RunCommand(%q) = %q, want echo to have run with literal args", attempt, got)
		}
		if fileExists(marker) {
			t.Fatalf("RunCommand(%q) created %s: the string reached a shell", attempt, marker)
		}
	}
}

// TestAllowsMatchesWholeTokens covers the ways a prefix check can be talked
// around if it is done on the joined string instead of on argv.
func TestAllowsMatchesWholeTokens(t *testing.T) {
	env := Env{Allowed: ParseAllowlist([]string{"go test", "make"})}

	tests := []struct {
		command string
		want    bool
	}{
		{"go test ./...", true},
		{"go test", true},
		{"make", true},
		{"make build", true},
		{"go build ./...", false},  // different subcommand
		{"go tool compile", false}, // different subcommand
		{"gotcha test", false},     // not a token boundary in a joined match
		{"go --work test", false},  // flag inserted before the subcommand
		{"go", false},              // shorter than the prefix
		{"makebelieve", false},     // prefix of the token, not the token
		{"./go test", false},       // different program, same tail
		{"env go test", false},     // wrapper in front
	}
	for _, tc := range tests {
		if got := env.Allows(strings.Fields(tc.command)); got != tc.want {
			t.Errorf("Allows(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRunCommandRejectsAnEmptyCommand(t *testing.T) {
	env := allowingEnv(t, "go test")

	got := env.RunCommand(context.Background(), "   ")
	if !strings.Contains(got, "No command given") {
		t.Fatalf("RunCommand() = %q, want the empty command explained", got)
	}
}

func TestRunCommandTimesOutAndSaysSo(t *testing.T) {
	env := allowingEnv(t, "sleep")
	env.Budgets.CommandTimeout = time.Second // the floor; sleep 30 will not finish

	start := time.Now()
	got := env.RunCommand(context.Background(), "sleep 30")
	elapsed := time.Since(start)

	if !strings.Contains(got, "timed out") {
		t.Fatalf("RunCommand() = %q, want the timeout stated", got)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("RunCommand() took %s, want it killed at the deadline", elapsed)
	}
}

func TestRunCommandReportsAMissingBinary(t *testing.T) {
	env := allowingEnv(t, "definitely-not-installed")
	shimDir(t, nil)

	got := env.RunCommand(context.Background(), "definitely-not-installed --version")
	if !strings.Contains(got, "not installed") || !strings.Contains(got, "Do not retry") {
		t.Fatalf("RunCommand() = %q, want a model-actionable missing-binary message", got)
	}
}

func TestClipOutputKeepsBothEnds(t *testing.T) {
	// A compiler names the offending file at the top and summarises at the
	// bottom; keeping only the head would lose the summary.
	body := "HEAD" + strings.Repeat("x", 500) + "TAIL"

	got := clipOutput(body, 100)

	if !strings.HasPrefix(got, "HEAD") {
		t.Errorf("clipOutput() = %q, want the head kept", got)
	}
	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("clipOutput() = %q, want the tail kept", got)
	}
	if !strings.Contains(got, "bytes elided") {
		t.Errorf("clipOutput() = %q, want the cut declared", got)
	}
}

func TestClipOutputLeavesSmallOutputAlone(t *testing.T) {
	if got := clipOutput("short", 100); got != "short" {
		t.Fatalf("clipOutput() = %q, want it untouched under budget", got)
	}
	if got := clipOutput("anything", 0); got != "anything" {
		t.Fatalf("clipOutput() with no budget = %q, want it untouched", got)
	}
}

func TestClipOutputNeverSplitsARune(t *testing.T) {
	// Multi-byte throughout, so a byte-wise cut lands mid-rune every time.
	body := strings.Repeat("日本語", 200)

	got := clipOutput(body, 101)

	if !utf8ValidString(got) {
		t.Fatalf("clipOutput() produced invalid UTF-8: %q", got)
	}
}

func TestAllowedPhraseNamesWhatMayRun(t *testing.T) {
	env := Env{Allowed: ParseAllowlist([]string{"go test", "make"})}
	if got := env.AllowedPhrase(); !strings.Contains(got, `"go test"`) || !strings.Contains(got, `"make"`) {
		t.Fatalf("AllowedPhrase() = %q, want both commands named", got)
	}

	empty := Env{}
	if got := empty.AllowedPhrase(); !strings.Contains(got, "No commands are permitted") {
		t.Fatalf("AllowedPhrase() = %q, want the empty case stated plainly", got)
	}
}

func TestParseAllowlistDropsBlankEntries(t *testing.T) {
	// A blank entry would parse to a zero-length prefix, which every argv
	// begins with -- silently permitting everything.
	got := ParseAllowlist([]string{"go test", "   ", ""})

	if len(got) != 1 {
		t.Fatalf("ParseAllowlist() = %v, want blanks dropped", got)
	}
	if (Env{Allowed: got}).Allows([]string{"rm", "-rf", "/"}) {
		t.Fatal("a blank allowlist entry permitted an arbitrary command")
	}
}

// TestRunCommandKillsGrandchildren is the reason the command runs in its own
// process group. `go test` builds a binary and then runs it; killing only the
// process metron started leaves the real work going past the deadline, and --
// because the survivor holds the output pipe open -- the tool does not even
// return promptly. Disabling setProcessGroup turns this test red.
func TestRunCommandKillsGrandchildren(t *testing.T) {
	env := allowingEnv(t, "sh")
	env.Budgets.CommandTimeout = time.Second
	marker := filepath.Join(env.Root, "grandchild-survived")
	script := filepath.Join(env.Root, "spawn.sh")

	writeFile(t, script, "#!/bin/sh\n( sleep 2; touch '"+marker+"' ) &\nsleep 20\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	env.RunCommand(context.Background(), "sh "+script)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("RunCommand() took %s: a survivor held the output pipe open", elapsed)
	}

	// Wait past the point where the grandchild would have written the marker.
	time.Sleep(3 * time.Second)
	if fileExists(marker) {
		t.Fatal("the grandchild outlived the timeout: only the direct child was killed")
	}
}

func TestRunCommandAppliesATimeoutFloor(t *testing.T) {
	env := allowingEnv(t, "true")
	env.Budgets.CommandTimeout = 0 // a zero-valued Budgets, not a configured one

	// Without a floor this would be a zero deadline, and every command would
	// report a timeout it never had a chance to avoid.
	if got := env.RunCommand(context.Background(), "true"); !strings.Contains(got, "exit status 0") {
		t.Fatalf("RunCommand() = %q, want the floor to give the command time to run", got)
	}
}

func TestRunCommandReportsAFailureToStart(t *testing.T) {
	env := allowingEnv(t, "./notexec")
	// A file that exists but is not executable. The leading "./" matters: a name
	// containing a separator skips PATH lookup, so this fails in exec itself
	// rather than as the *exec.Error that missingBinary claims for a missing
	// binary. It is neither a non-zero exit nor a missing program.
	path := filepath.Join(env.Root, "notexec")
	writeFile(t, path, "#!/bin/sh\ntrue\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	got := env.RunCommand(context.Background(), "./notexec")

	if !strings.Contains(got, "failed to start") {
		t.Fatalf("RunCommand() = %q, want the start failure reported", got)
	}
}

func TestKillProcessGroupToleratesAnUnstartedCommand(t *testing.T) {
	// Cancel can fire before the process exists; killing pid 0's group would
	// signal every process metron can reach.
	if err := killProcessGroup(exec.Command("true")); err != nil {
		t.Fatalf("killProcessGroup() = %v, want nil for an unstarted command", err)
	}
}
