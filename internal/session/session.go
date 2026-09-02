// Package session persists a conversation to disk so `--continue` can resume
// it. Scope is deliberately small: one default session file, not named
// multi-session management.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"metron/internal/ollama"
)

// Path is the default session file, gitignored alongside .metron.json and
// .tags -- per-repository, not shared across projects.
const Path = ".metron/session.json"

// Save writes msgs to path as JSON, creating the containing directory if
// needed.
func Save(path string, msgs []ollama.Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write session %s: %w", path, err)
	}
	return nil
}

// Load reads a previously saved conversation. A missing file is reported as
// an error -- unlike the optional AGENTS.md file, --continue with nothing to
// continue is a mistake the operator should be told about, not silently
// downgraded to a fresh session.
func Load(path string) ([]ollama.Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session %s: %w", path, err)
	}
	var msgs []ollama.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", path, err)
	}
	return msgs, nil
}
