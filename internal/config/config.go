// Package config loads metron's settings from a JSON file, with defaults and
// environment overrides. Resolution order, lowest priority first:
//
//	built-in defaults  <  config file  <  environment variables
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the full set of tunable settings. Every field maps to a value that
// was previously hard-coded, so a stock config file reproduces the old
// behaviour exactly.
type Config struct {
	// Connection
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	// Provider selects the wire format: "ollama" speaks Ollama's /api/chat,
	// "openai" speaks the OpenAI chat-completions format used by llama.cpp,
	// vLLM, LM Studio and similar local servers. Both are local-only --
	// Endpoint still has to point at a server on your machine or network.
	Provider string `json:"provider"`
	// TimeoutSeconds bounds silence, not total generation time: a streamed
	// reply that keeps arriving is never cut off, however long it takes.
	TimeoutSeconds int  `json:"timeout_seconds"`
	Stream         bool `json:"stream"`

	// Sampling
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	NumCtx      int     `json:"num_ctx"`

	// Agent loop
	MaxTurns           int `json:"max_turns"`
	CompactThreshold   int `json:"compact_threshold_bytes"`
	MaxHistoryMessages int `json:"max_history_messages"`

	// Tool budgets
	MaxSliceLines    int `json:"max_slice_lines"`
	MaxLineChars     int `json:"max_line_chars"`
	SearchMaxMatches int `json:"search_max_matches"`
	SearchMaxPerFile int `json:"search_max_per_file"`
	ListMaxEntries   int `json:"list_max_entries"`
	MaxUndoStack     int `json:"max_undo_stack"`

	// Safety
	AutoApprovePatches bool   `json:"auto_approve_patches"`
	PlanModeDefault    bool   `json:"plan_mode_default"`
	PreToolHook        string `json:"pre_tool_hook"`

	// Project context
	InstructionsFile     string `json:"instructions_file"`
	MaxInstructionsBytes int    `json:"max_instructions_bytes"`
	MaxCommandBytes      int    `json:"max_command_bytes"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Endpoint:             "http://localhost:11434/api/chat",
		Model:                "qwen2.5-coder:32b",
		Provider:             "ollama",
		TimeoutSeconds:       180,
		Stream:               true,
		Temperature:          0.1,
		TopP:                 0.95,
		NumCtx:               16384,
		MaxTurns:             10,
		CompactThreshold:     400,
		MaxHistoryMessages:   60,
		MaxSliceLines:        120,
		MaxLineChars:         500,
		SearchMaxMatches:     10,
		SearchMaxPerFile:     2,
		ListMaxEntries:       60,
		MaxUndoStack:         20,
		AutoApprovePatches:   false,
		PlanModeDefault:      false,
		InstructionsFile:     "AGENTS.md",
		MaxInstructionsBytes: 4096,
		MaxCommandBytes:      4096,
	}
}

// ProjectFile is the config file metron looks for in the working directory.
const ProjectFile = ".metron.json"

// IsProjectFile reports whether path is the project-local config file found
// by searching the working directory, as opposed to the user-level config or
// an explicit $METRON_CONFIG override. A repository's own .metron.json is
// not something the operator necessarily wrote or reviewed -- cloning an
// untrusted repo and running metron in it loads whatever that file contains
// with no prompt -- so callers use this to require extra confirmation before
// honoring security-sensitive keys (pre_tool_hook) sourced from it.
func IsProjectFile(path string) bool {
	return path == ProjectFile
}

// Search returns the config file paths metron consults, highest priority
// first. An explicit path (from METRON_CONFIG) short-circuits the search.
func Search() []string {
	if explicit := os.Getenv("METRON_CONFIG"); explicit != "" {
		return []string{explicit}
	}
	paths := []string{ProjectFile}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".config")
		}
	}
	if dir != "" {
		paths = append(paths, filepath.Join(dir, "metron", "config.json"))
	}
	return paths
}

// Load resolves the configuration: defaults, overlaid with the first config
// file found, overlaid with the environment. The returned path is the file
// that was used, or "" if none was found.
//
// A file that exists but cannot be read or parsed is an error rather than a
// silent fallback -- a typo in a config file should not quietly change how the
// agent behaves.
func Load() (Config, string, error) {
	cfg := Defaults()

	var used string
	for _, path := range Search() {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cfg, path, fmt.Errorf("read config %s: %w", path, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, path, fmt.Errorf("parse config %s: %w", path, err)
		}
		used = path
		break
	}

	// The environment wins, so a one-off override needs no file edit.
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("OLLAMA_MODEL"); v != "" {
		cfg.Model = v
	}

	if err := cfg.Validate(); err != nil {
		return cfg, used, err
	}
	return cfg, used, nil
}

// instructionsTruncatedMarker ends the injected project instructions when they
// are cut off at max_instructions_bytes, so the model can tell a shortened
// file from a complete one -- the same convention view_slice and search_text
// use for their own truncation.
const instructionsTruncatedMarker = "\n[instructions truncated]"

// LoadInstructions reads a project-instructions file (AGENTS.md by default)
// and caps it at maxBytes. A missing file is not an error -- the feature is
// optional, same as Preflight's missing-binary warnings -- it simply means no
// instructions are injected into the system prompt.
func LoadInstructions(path string, maxBytes int) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read instructions %s: %w", path, err)
	}
	text := strings.TrimRight(string(data), "\n")
	if maxBytes > 0 && len(text) > maxBytes {
		text = text[:maxBytes] + instructionsTruncatedMarker
	}
	return text, nil
}

// Validate rejects settings that would make the agent misbehave rather than
// merely perform differently.
func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Endpoint) == "" {
		problems = append(problems, "endpoint must not be empty")
	}
	if strings.TrimSpace(c.Model) == "" {
		problems = append(problems, "model must not be empty")
	}
	if c.Provider != "ollama" && c.Provider != "openai" {
		problems = append(problems, fmt.Sprintf(`provider must be "ollama" or "openai" (got %q)`, c.Provider))
	}
	for _, check := range []struct {
		name string
		val  int
	}{
		{"timeout_seconds", c.TimeoutSeconds},
		{"num_ctx", c.NumCtx},
		{"max_turns", c.MaxTurns},
		{"compact_threshold_bytes", c.CompactThreshold},
		{"max_history_messages", c.MaxHistoryMessages},
		{"max_slice_lines", c.MaxSliceLines},
		{"max_line_chars", c.MaxLineChars},
		{"search_max_matches", c.SearchMaxMatches},
		{"search_max_per_file", c.SearchMaxPerFile},
		{"list_max_entries", c.ListMaxEntries},
		{"max_undo_stack", c.MaxUndoStack},
		{"max_instructions_bytes", c.MaxInstructionsBytes},
		{"max_command_bytes", c.MaxCommandBytes},
	} {
		if check.val <= 0 {
			problems = append(problems, fmt.Sprintf("%s must be > 0 (got %d)", check.name, check.val))
		}
	}
	if c.Temperature < 0 {
		problems = append(problems, fmt.Sprintf("temperature must be >= 0 (got %v)", c.Temperature))
	}
	if c.TopP <= 0 || c.TopP > 1 {
		problems = append(problems, fmt.Sprintf("top_p must be in (0, 1] (got %v)", c.TopP))
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}
