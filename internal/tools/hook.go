package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// RunPreToolHook runs cmdline as a shell command before a tool executes,
// giving an operator a scriptable policy gate over all five tools -- a
// generalisation of apply_patch's interactive approval prompt. The tool name
// and its arguments are marshalled as {"tool":..., "args":...} and piped to
// the command's stdin. Exit 0 allows the call; any other exit code denies it,
// and the command's stderr becomes the reason the model sees, matching the
// codebase's convention of phrasing refusals as text the model can act on
// rather than as opaque errors.
func RunPreToolHook(cmdline, tool string, args map[string]any) (allowed bool, reason string, err error) {
	payload, err := json.Marshal(struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}{tool, args})
	if err != nil {
		return false, "", fmt.Errorf("marshal hook payload: %w", err)
	}

	cmd := exec.Command("sh", "-c", cmdline)
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = fmt.Sprintf("pre_tool_hook denied %s", tool)
			}
			return false, reason, nil
		}
		return false, "", fmt.Errorf("run pre_tool_hook: %w", err)
	}
	return true, "", nil
}
