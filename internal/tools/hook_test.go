package tools

import (
	"strings"
	"testing"
)

func TestRunPreToolHookAllowsOnExitZero(t *testing.T) {
	allowed, reason, err := RunPreToolHook("exit 0", "search_text", map[string]any{"pattern": "x"})
	if err != nil {
		t.Fatalf("RunPreToolHook() error = %v", err)
	}
	if !allowed {
		t.Fatalf("allowed = false, reason = %q, want exit 0 to allow", reason)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty on allow", reason)
	}
}

func TestRunPreToolHookDeniesOnNonZeroExitWithStderrAsReason(t *testing.T) {
	allowed, reason, err := RunPreToolHook(`echo "not allowed" >&2; exit 1`, "apply_patch", nil)
	if err != nil {
		t.Fatalf("RunPreToolHook() error = %v", err)
	}
	if allowed {
		t.Fatal("allowed = true, want a non-zero exit to deny")
	}
	if reason != "not allowed" {
		t.Fatalf("reason = %q, want the command's stderr", reason)
	}
}

func TestRunPreToolHookGivesADefaultReasonWhenStderrIsEmpty(t *testing.T) {
	allowed, reason, err := RunPreToolHook("exit 1", "find_symbol", nil)
	if err != nil {
		t.Fatalf("RunPreToolHook() error = %v", err)
	}
	if allowed {
		t.Fatal("allowed = true, want denial")
	}
	if !strings.Contains(reason, "find_symbol") {
		t.Fatalf("reason = %q, want it to name the tool when stderr is silent", reason)
	}
}

func TestRunPreToolHookPassesToolAndArgsOnStdin(t *testing.T) {
	allowed, reason, err := RunPreToolHook(
		`input=$(cat); case "$input" in *'"tool":"view_slice"'*'"path":"a.go"'*) exit 0;; *) echo "got: $input" >&2; exit 1;; esac`,
		"view_slice", map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatalf("RunPreToolHook() error = %v", err)
	}
	if !allowed {
		t.Fatalf("allowed = false, reason = %q, want the hook to see the tool name and args on stdin", reason)
	}
}

func TestRunPreToolHookReportsAnUnrunnableCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no `sh` on PATH
	_, _, err := RunPreToolHook("exit 0", "list_files", nil)
	if err == nil {
		t.Fatal("RunPreToolHook() error = nil, want a failure when sh cannot be found")
	}
}

func TestRunPreToolHookReportsAnUnmarshalableArgument(t *testing.T) {
	_, _, err := RunPreToolHook("exit 0", "search_text", map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("RunPreToolHook() error = nil, want the marshal failure reported")
	}
}
