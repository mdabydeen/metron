package tools

import (
	"errors"
	"fmt"
	"os/exec"
)

// missingBinary turns a failed exec into a message the *model* can act on.
// A bare "ripgrep error:" tells the model nothing, so it retries the same
// doomed call until the turn budget runs out; naming the tool as unavailable
// and saying not to retry ends that loop and points at what still works.
func missingBinary(err error, binary, tool, alternatives string) error {
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		return nil
	}
	return fmt.Errorf("%s is not installed, so %s is unavailable. Do not retry it; %s",
		binary, tool, alternatives)
}
