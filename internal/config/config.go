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
	// MaxOutputTokens is sent to Ollama as num_predict. Ollama otherwise
	// defaults to unlimited generation, which conflicts with metron's bounded
	// context and tool philosophy.
	MaxOutputTokens int `json:"max_output_tokens"`

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
		MaxOutputTokens:    4096,
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

// ProjectFile is the per-project configuration filename.
const ProjectFile = ".metron.json"

// Search returns the config file paths metron consults, highest priority
// first. METRON_CONFIG_DIR selects the base directory for the user config;
// otherwise the user's home directory is used.
func Search() []string {
	return SearchFrom("")
}

// SearchFrom is Search with an explicit project root. It is used by the CLI
// after it has resolved the enclosing repository, so starting metron in a
// nested package still finds the configuration stored at the repository root.
// An empty projectRoot preserves Search's current-working-directory behaviour.
func SearchFrom(projectRoot string) []string {
	if explicit := os.Getenv("METRON_CONFIG"); explicit != "" {
		return []string{explicit}
	}
	projectFile := ProjectFile
	if projectRoot != "" {
		projectFile = filepath.Join(projectRoot, ProjectFile)
	}
	paths := []string{projectFile}
	dir := os.Getenv("METRON_CONFIG_DIR")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		}
	}
	if dir != "" {
		paths = append(paths, filepath.Join(dir, ".metron", "config.json"))
	}
	return paths
}

// Source names where the effective configuration came from. It is the axis the
// trust boundary turns on: a project's .metron.json is data the repository
// ships, so it must not be able to raise the privileges of the operator's own
// settings, whereas the operator's own file -- or a file METRON_CONFIG names --
// may.
type Source int

const (
	// SourceDefault means no config file was found; the built-in defaults apply.
	SourceDefault Source = iota
	// SourceProject is the repository's .metron.json.
	SourceProject
	// SourceUser is the operator's own configuration, under METRON_CONFIG_DIR or
	// the home directory.
	SourceUser
	// SourceExplicit is the file METRON_CONFIG points at, which the operator chose
	// and so may carry any capability.
	SourceExplicit
)

// Result is what configuration loading resolves: the effective settings, where
// they came from, and the operator-facing warnings that come with that source.
type Result struct {
	Config   Config
	Path     string
	Source   Source
	Warnings []string
}

// sourceForIndex classifies the winning search entry. A METRON_CONFIG file is
// always the sole entry, so an explicit file is trusted whole; otherwise the
// first entry is the project file and the second is the operator's.
func sourceForIndex(explicit bool, index int) Source {
	if explicit {
		return SourceExplicit
	}
	if index == 0 {
		return SourceProject
	}
	return SourceUser
}

// Load resolves the configuration: built-in defaults, overlaid with the first
// config file found, overlaid with the environment. The returned Result says
// where the settings came from and carries the operator-facing warnings that
// source implies.
//
// A file that exists but cannot be read or parsed is an error rather than a
// silent fallback -- a typo in a config file should not quietly change how the
// agent behaves. On error, the Result retains the attempted path, source, and
// any configuration values decoded before the failure for caller diagnostics.
func Load() (Result, error) {
	return LoadFrom("")
}

// LoadFrom resolves configuration like Load, but reads the project file from
// projectRoot. This keeps the package useful to callers that want cwd-based
// lookup while allowing the CLI to use the same repository root as its tools.
func LoadFrom(projectRoot string) (Result, error) {
	cfg := Defaults()
	result := Result{Config: cfg, Source: SourceDefault}

	// The search order is a trust order: the project file comes first so it can
	// customise the project, but that is exactly why it is the one we police.
	explicit := os.Getenv("METRON_CONFIG") != ""
	for index, path := range SearchFrom(projectRoot) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		result.Path = path
		result.Source = sourceForIndex(explicit, index)
		if err != nil {
			return result, fmt.Errorf("read config %s: %w", path, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			result.Config = cfg
			return result, fmt.Errorf("parse config %s: %w", path, err)
		}
		break
	}

	// The environment wins, so a one-off override needs no file edit. These are
	// operator-level and may set anything.
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv("OLLAMA_MODEL"); v != "" {
		cfg.Model = v
	}

	// The trust boundary and the warnings it produces both belong to loading, so
	// they are settled here and handed to the caller rather than the REPL.
	result.Warnings = applyProjectPolicy(&cfg, result.Source)

	if err := cfg.Validate(); err != nil {
		result.Config = cfg
		return result, err
	}
	result.Config = cfg
	return result, nil
}

// applyProjectPolicy enforces the trust boundary and returns the warnings a
// source implies. A project file is the one source that may not raise
// privileges, so its dangerous fields are stripped unless the operator opted in;
// every source is then warned about the capabilities that actually took effect.
func applyProjectPolicy(cfg *Config, source Source) []string {
	var warnings []string

	if source == SourceProject {
		// A project is untrusted data: it may set budgets and the model, but it
		// may not decide whether its own proposed edits run without a human.
		if cfg.AutoApprovePatches {
			cfg.AutoApprovePatches = false
			warnings = append(warnings, "project .metron.json requested auto_approve_patches; ignored -- a project file may not skip patch approval")
		}
		if len(cfg.AllowedCommands) > 0 {
			if optInProjectCommands() {
				warnings = append(warnings, "project .metron.json grants allowed_commands; you opted in with METRON_ALLOW_PROJECT_COMMANDS")
			} else {
				grant := cfg.AllowedCommands
				cfg.AllowedCommands = []string{}
				warnings = append(warnings, fmt.Sprintf("project .metron.json requested allowed_commands %v; ignored -- set METRON_ALLOW_PROJECT_COMMANDS to permit them", grant))
			}
		}
	}

	// What actually took effect, from any source, is still worth naming.
	if cfg.AutoApprovePatches {
		warnings = append(warnings, "auto_approve_patches is enabled: patches apply without approval")
	}
	if len(cfg.AllowedCommands) > 0 {
		warnings = append(warnings, fmt.Sprintf("allowed_commands is non-empty: the model may run %d command prefix(es)", len(cfg.AllowedCommands)))
	}
	return warnings
}

// optInProjectCommands reports whether the operator explicitly chose to let a
// project file grant allowed_commands. The opt-in is an environment variable
// rather than a config field on purpose: a project file could otherwise opt
// itself in, which would defeat the boundary.
func optInProjectCommands() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("METRON_ALLOW_PROJECT_COMMANDS"))) {
	case "1", "true", "yes":
		return true
	}
	return false
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
		{"max_output_tokens", c.MaxOutputTokens},
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
