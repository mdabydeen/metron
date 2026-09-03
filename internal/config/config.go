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
	"slices"
	"strings"

	"github.com/mdabydeen/metron/internal/tools"
)

// Config is the full set of tunable settings. Every field maps to a value that
// was previously hard-coded, so a stock config file reproduces the old
// behaviour exactly.
type Config struct {
	// Connection
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
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

	// Tools
	// DisabledTools names tools to withhold from the model. Their schemas are
	// then not sent at all, which is a saving on every request rather than only
	// on the turns that would have used them.
	DisabledTools []string `json:"disabled_tools"`

	// AllowedCommands is what run_command may execute, as argv prefixes:
	// "go test" permits `go test ./...` but not `go tool`. Empty -- the default
	// -- withdraws the tool entirely, so letting a model run anything at all is
	// a decision an operator makes rather than one they forget to unmake.
	AllowedCommands []string `json:"allowed_commands"`

	CommandTimeoutSeconds int `json:"command_timeout_seconds"`
	MaxCommandOutputBytes int `json:"max_command_output_bytes"`

	// Safety
	AutoApprovePatches bool `json:"auto_approve_patches"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Endpoint:           "http://localhost:11434/api/chat",
		Model:              "qwen2.5-coder:32b",
		TimeoutSeconds:     180,
		Stream:             true,
		Temperature:        0.1,
		TopP:               0.95,
		NumCtx:             16384,
		MaxTurns:           10,
		CompactThreshold:   400,
		MaxHistoryMessages: 60,
		MaxSliceLines:      120,
		MaxLineChars:       500,
		SearchMaxMatches:   10,
		SearchMaxPerFile:   2,
		ListMaxEntries:     60,
		DisabledTools:      []string{},

		AllowedCommands:       []string{},
		CommandTimeoutSeconds: 120,
		MaxCommandOutputBytes: 4000,

		AutoApprovePatches: false,
	}
}

// ProjectFile is the config file metron looks for in the working directory.
const ProjectFile = ".metron.json"

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
		{"command_timeout_seconds", c.CommandTimeoutSeconds},
		{"max_command_output_bytes", c.MaxCommandOutputBytes},
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
	// A typo here would silently leave a tool enabled, which is the opposite of
	// what the operator asked for -- so it is an error, like an unknown key.
	for _, name := range c.DisabledTools {
		if !slices.Contains(tools.ToolNames, name) {
			problems = append(problems, fmt.Sprintf("disabled_tools: unknown tool %q (known: %s)",
				name, strings.Join(tools.ToolNames, ", ")))
		}
	}
	// A blank entry would match every argv prefix of length zero -- that is, all
	// of them -- turning a typo into "run anything".
	for i, cmd := range c.AllowedCommands {
		if strings.TrimSpace(cmd) == "" {
			problems = append(problems, fmt.Sprintf("allowed_commands[%d] must not be empty", i))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}
