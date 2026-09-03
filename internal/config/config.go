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

	"github.com/mdabydeen/metron/tools"
)

// Config is the full set of tunable settings. Every field maps to a value that
// was previously hard-coded, so a stock config file reproduces the old
// behaviour exactly.
type Config struct {
	// Connection
	// Provider selects the wire format. "openai" reaches llama.cpp's server,
	// LM Studio, vLLM, OpenRouter and Ollama's own compatibility endpoint, so
	// one setting covers most of what people actually run.
	// Profile is a named set of budgets to start from. Individual settings in
	// the same file still win, so a profile is a baseline rather than a lock.
	Profile string `json:"profile"`

	Provider string `json:"provider"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	// APIKeyEnv names the environment variable holding the key, rather than
	// holding the key. A secret in a config file is one `cat` from being
	// pasted into an issue.
	APIKeyEnv string `json:"api_key_env"`
	// TimeoutSeconds bounds silence, not total generation time: a streamed
	// reply that keeps arriving is never cut off, however long it takes.
	TimeoutSeconds int  `json:"timeout_seconds"`
	Stream         bool `json:"stream"`

	// Sampling
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	NumCtx      int     `json:"num_ctx"`

	// SystemPromptExtra is appended to the generated system prompt.
	//
	// Different models need different nudges to stay inside the tool contract,
	// and those nudges are folklore until something measures them. Keeping them
	// in config rather than baked into the binary means metron ships no
	// unverified claims about particular models, and `make bench` can settle
	// which ones earn the tokens they cost on every request.
	SystemPromptExtra string `json:"system_prompt_extra"`

	// Agent loop
	MaxTurns           int `json:"max_turns"`
	CompactThreshold   int `json:"compact_threshold_bytes"`
	MaxHistoryMessages int `json:"max_history_messages"`
	// RepoMapTokens budgets a structural summary of the project injected once
	// per session, so the model starts with a picture instead of guessing
	// filenames. Zero disables it, which is the default until the benchmark
	// shows the turns saved outweigh the tokens spent on every request.
	RepoMapTokens int `json:"repo_map_tokens"`
	// MaxPromptTokens caps what one turn may spend on prompt tokens across all
	// its round-trips. Zero means no ceiling. Every other budget bounds one
	// tool; this bounds the turn.
	MaxPromptTokens int `json:"max_prompt_tokens"`

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

	// EditFormat picks how the model expresses a change: "diff" (apply_patch,
	// a unified diff) or "search_replace" (edit_file, an anchored quote).
	// Unified diffs need exact line numbers, which is the thing small models
	// most reliably get wrong; search_replace asks them to quote lines they
	// have already read instead.
	EditFormat string `json:"edit_format"`

	CommandTimeoutSeconds int `json:"command_timeout_seconds"`
	MaxCommandOutputBytes int `json:"max_command_output_bytes"`

	// Sessions
	// SaveSessions writes each conversation to .metron/sessions as it goes, so
	// it survives exiting. The directory ignores itself, so it never shows up
	// as untracked in the project it lives in.
	//
	// Off by default. A transcript contains every tool result the model saw,
	// which is every file it read -- persisting that to disk indefinitely is a
	// change in exposure the operator should choose, not inherit.
	SaveSessions bool `json:"save_sessions"`

	// Safety
	AutoApprovePatches bool `json:"auto_approve_patches"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Profile:            ProfileStandard,
		Provider:           ProviderOllama,
		Endpoint:           "http://localhost:11434/api/chat",
		APIKeyEnv:          "",
		Model:              "qwen2.5-coder:32b",
		TimeoutSeconds:     180,
		Stream:             true,
		Temperature:        0.1,
		TopP:               0.95,
		NumCtx:             16384,
		SystemPromptExtra:  "",
		MaxTurns:           10,
		CompactThreshold:   400,
		MaxHistoryMessages: 60,
		RepoMapTokens:      0,
		MaxPromptTokens:    0,
		MaxSliceLines:      120,
		MaxLineChars:       500,
		SearchMaxMatches:   10,
		SearchMaxPerFile:   2,
		ListMaxEntries:     60,
		DisabledTools:      []string{},

		EditFormat:            tools.FormatDiff,
		AllowedCommands:       []string{},
		CommandTimeoutSeconds: 120,
		MaxCommandOutputBytes: 4000,

		SaveSessions:       false,
		AutoApprovePatches: false,
	}
}

// Budget profiles. Eleven numbers is too many to tune blind, so these are the
// starting points for the three situations people actually have.
//
// They are reasoned, not measured. metron ships a benchmark precisely so that
// claims like these can be checked -- run `make bench` against your own model
// and adjust. Saying so is better than presenting a guess as a finding.
const (
	ProfileTight    = "tight"    // a 7B on a laptop with a small context window
	ProfileStandard = "standard" // the built-in defaults
	ProfileRoomy    = "roomy"    // a 32B with room to spare
)

// Profiles lists the valid profile settings.
var Profiles = []string{ProfileTight, ProfileStandard, ProfileRoomy}

// applyProfile returns cfg with one profile's budgets applied over it.
func applyProfile(cfg Config, name string) Config {
	switch name {
	case ProfileTight:
		cfg.NumCtx = 8192
		cfg.MaxSliceLines = 60
		cfg.MaxLineChars = 300
		cfg.SearchMaxMatches = 6
		cfg.SearchMaxPerFile = 1
		cfg.ListMaxEntries = 40
		cfg.MaxHistoryMessages = 30
		cfg.CompactThreshold = 300
		cfg.MaxTurns = 8
		cfg.RepoMapTokens = 0
		cfg.MaxPromptTokens = 6000
	case ProfileRoomy:
		cfg.NumCtx = 32768
		cfg.MaxSliceLines = 200
		cfg.MaxLineChars = 800
		cfg.SearchMaxMatches = 20
		cfg.SearchMaxPerFile = 3
		cfg.ListMaxEntries = 100
		cfg.MaxHistoryMessages = 80
		cfg.MaxTurns = 12
		cfg.RepoMapTokens = 600
	}
	return cfg
}

// The wire formats metron can speak.
const (
	ProviderOllama = "ollama"
	ProviderOpenAI = "openai"
)

// Providers lists the valid provider settings.
var Providers = []string{ProviderOllama, ProviderOpenAI}

// APIKey reads the key from the environment variable the config names, or ""
// when none is configured.
func (c Config) APIKey() string {
	if c.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.APIKeyEnv)
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

// privileged names the settings that grant authority rather than tune
// behaviour. They are the difference between "metron edits when you say so"
// and "metron edits and executes without asking".
//
// A project's own .metron.json is the highest-priority config file, which means
// it ships inside whatever repository you point metron at. A cloned repository
// that could set these would disable the approval prompt and enable
// run_command before the first turn -- turning `git clone && metron` into
// arbitrary code execution, with every mitigation this program documents
// switched off by a file the operator never read.
//
// So these are honoured only from a config the *operator* chose: the user-level
// file, or one named explicitly by METRON_CONFIG. A project file that sets them
// is reported and ignored, never silently obeyed.
var privileged = []string{"auto_approve_patches", "allowed_commands", "save_sessions"}

// Load resolves the configuration: defaults, overlaid with the first config
// file found, overlaid with the environment. The returned path is the file
// that was used, or "" if none was found.
//
// Settings in `privileged` are dropped from a project-level file; the returned
// warnings say which, so the operator can see what was refused.
//
// A file that exists but cannot be read or parsed is an error rather than a
// silent fallback -- a typo in a config file should not quietly change how the
// agent behaves.
func Load() (Config, string, []string, error) {
	cfg := Defaults()

	var (
		used     string
		warnings []string
	)
	for _, path := range Search() {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cfg, path, nil, fmt.Errorf("read config %s: %w", path, err)
		}
		// One parse serves both jobs below. Doing them separately meant two
		// json.Unmarshal calls over the same bytes, where the second could
		// never fail if the first had not -- a branch no test could reach.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return cfg, path, nil, fmt.Errorf("parse config %s: %w", path, err)
		}

		// A profile is a baseline the same file may then override, so it has
		// to be applied before the file is decoded over it.
		if name, err := profileFrom(raw); err != nil {
			return cfg, path, nil, err
		} else if name != "" {
			cfg = applyProfile(cfg, name)
		}

		// A project file is untrusted input: it arrives with the code, not from
		// the operator. Note which authority-granting keys it sets so they can
		// be undone after decoding.
		var granted []string
		if isProjectFile(path) {
			granted = privilegedIn(raw)
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, path, nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		if len(granted) > 0 {
			d := Defaults()
			cfg.AutoApprovePatches = d.AutoApprovePatches
			cfg.AllowedCommands = d.AllowedCommands
			cfg.SaveSessions = d.SaveSessions
			warnings = append(warnings, fmt.Sprintf(
				"%s tried to set %s; ignored, because a config file that ships with a "+
					"repository must not be able to grant itself permissions. Move it to "+
					"your user config or point METRON_CONFIG at it if you meant it",
				path, strings.Join(granted, ", ")))
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
		return cfg, used, warnings, err
	}
	return cfg, used, warnings, nil
}

// isProjectFile reports whether a config path is the one that travels with a
// repository, as opposed to one the operator placed or named themselves.
func isProjectFile(path string) bool {
	if os.Getenv("METRON_CONFIG") != "" {
		return false // named explicitly by the operator
	}
	return path == ProjectFile
}

// privilegedIn reports which authority-granting keys a raw config sets.
//
// Presence is read from the raw JSON rather than from the decoded struct
// because a decoded false is indistinguishable from an absent key -- and "the
// project tried to set this and it was ignored" is exactly the thing worth
// saying out loud.
func privilegedIn(raw map[string]json.RawMessage) []string {
	var found []string
	for _, key := range privileged {
		if _, present := raw[key]; present {
			found = append(found, key)
		}
	}
	return found
}

// profileFrom reads and validates the profile a raw config names.
func profileFrom(raw map[string]json.RawMessage) (string, error) {
	value, present := raw["profile"]
	if !present {
		return "", nil
	}
	var name string
	if err := json.Unmarshal(value, &name); err != nil {
		return "", fmt.Errorf("invalid config: profile must be a string")
	}
	if name != "" && !slices.Contains(Profiles, name) {
		return "", fmt.Errorf("invalid config: profile must be one of %s (got %q)",
			strings.Join(Profiles, ", "), name)
	}
	return name, nil
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
	if c.MaxPromptTokens < 0 {
		problems = append(problems, fmt.Sprintf("max_prompt_tokens must be >= 0 (got %d)", c.MaxPromptTokens))
	}
	if c.RepoMapTokens < 0 {
		problems = append(problems, fmt.Sprintf("repo_map_tokens must be >= 0 (got %d)", c.RepoMapTokens))
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
	if !slices.Contains(Profiles, c.Profile) {
		problems = append(problems, fmt.Sprintf("profile must be one of %s (got %q)",
			strings.Join(Profiles, ", "), c.Profile))
	}
	if !slices.Contains(Providers, c.Provider) {
		problems = append(problems, fmt.Sprintf("provider must be one of %s (got %q)",
			strings.Join(Providers, ", "), c.Provider))
	}
	if !slices.Contains(tools.EditFormats, c.EditFormat) {
		problems = append(problems, fmt.Sprintf("edit_format must be one of %s (got %q)",
			strings.Join(tools.EditFormats, ", "), c.EditFormat))
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
