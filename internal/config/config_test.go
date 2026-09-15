package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mdabydeen/metron/internal/tools"
)

// isolate removes every source of configuration so a test starts from a known
// state: no project file, no user file, no environment overrides.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("METRON_CONFIG", "")
	t.Setenv("METRON_CONFIG_DIR", dir)
	t.Setenv("METRON_ALLOW_PROJECT_COMMANDS", "")
	t.Setenv("OLLAMA_HOST", "")
	t.Setenv("OLLAMA_MODEL", "")
	return dir
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("Defaults() is invalid: %v", err)
	}
}

func TestLoadWithoutAnyConfigFile(t *testing.T) {
	isolate(t)

	res, err := Load()
	cfg, path := res.Config, res.Path
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty when no file exists", path)
	}
	if !reflect.DeepEqual(cfg, Defaults()) {
		t.Fatalf("Load() = %+v, want the defaults", cfg)
	}
}

func TestLoadReadsProjectFile(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"model":"gemma4:12b-mlx","max_slice_lines":40}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	cfg, path := res.Config, res.Path
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if path != ProjectFile {
		t.Fatalf("path = %q, want %q", path, ProjectFile)
	}
	if res.Source != SourceProject {
		t.Fatalf("source = %v, want SourceProject", res.Source)
	}
	if cfg.Model != "gemma4:12b-mlx" || cfg.MaxSliceLines != 40 {
		t.Fatalf("Load() = %+v, want the file's values", cfg)
	}
	// Unspecified fields keep their defaults.
	if cfg.Endpoint != Defaults().Endpoint || cfg.MaxTurns != Defaults().MaxTurns {
		t.Fatalf("Load() = %+v, want unspecified fields defaulted", cfg)
	}
}

func TestLoadFromReadsProjectRootOutsideCurrentDirectory(t *testing.T) {
	dir := isolate(t)
	project := filepath.Join(dir, "project")
	nested := filepath.Join(project, "pkg", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ProjectFile), []byte(`{"model":"from-root"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	res, err := LoadFrom(project)
	cfg, path := res.Config, res.Path
	if err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	wantPath := filepath.Join(project, ProjectFile)
	if path != wantPath || cfg.Model != "from-root" {
		t.Fatalf("LoadFrom() = model %q from %q, want from-root from %q", cfg.Model, path, wantPath)
	}
}

func TestLoadFallsBackToUserFile(t *testing.T) {
	dir := isolate(t)
	userDir := filepath.Join(dir, ".metron")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(userDir, "config.json")
	if err := os.WriteFile(userFile, []byte(`{"model":"from-user-file"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	cfg, path := res.Config, res.Path
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if path != userFile {
		t.Fatalf("path = %q, want %q", path, userFile)
	}
	if res.Source != SourceUser {
		t.Fatalf("source = %v, want SourceUser", res.Source)
	}
	if cfg.Model != "from-user-file" {
		t.Fatalf("model = %q, want the user file's value", cfg.Model)
	}
}

func TestLoadReadsHomeConfig(t *testing.T) {
	isolate(t)
	home := t.TempDir()
	t.Setenv("METRON_CONFIG_DIR", "")
	t.Setenv("HOME", home)
	userFile := filepath.Join(home, ".metron", "config.json")
	if err := os.MkdirAll(filepath.Dir(userFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userFile, []byte(`{"model":"from-home"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Path != userFile || res.Source != SourceUser || res.Config.Model != "from-home" {
		t.Fatalf("Load() = model %q from %q (source %v), want the home config as SourceUser", res.Config.Model, res.Path, res.Source)
	}
}

func TestProjectFileWinsOverUserFile(t *testing.T) {
	dir := isolate(t)
	userDir := filepath.Join(dir, ".metron")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(`{"model":"user","auto_approve_patches":true,"allowed_commands":["go test"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectFile, []byte(`{"model":"project"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	cfg, path := res.Config, res.Path
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Model != "project" || path != ProjectFile {
		t.Fatalf("Load() = %q from %q, want the project file to win", cfg.Model, path)
	}
	if cfg.AutoApprovePatches || len(cfg.AllowedCommands) != 0 {
		t.Fatalf("Load() = %+v, want user-file privileges excluded when the project file wins", cfg)
	}
}

func TestMetronConfigOverridesTheSearch(t *testing.T) {
	dir := isolate(t)
	explicit := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(explicit, []byte(`{"model":"explicit"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A project file exists but must be ignored.
	if err := os.WriteFile(ProjectFile, []byte(`{"model":"project"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METRON_CONFIG", explicit)

	res, err := Load()
	cfg, path := res.Config, res.Path
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Model != "explicit" || path != explicit {
		t.Fatalf("Load() = %q from %q, want the explicit file", cfg.Model, path)
	}
	if res.Source != SourceExplicit {
		t.Fatalf("source = %v, want SourceExplicit", res.Source)
	}
}

func TestEnvironmentOverridesTheFile(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"model":"from-file","endpoint":"http://file/api/chat"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLLAMA_MODEL", "from-env")
	t.Setenv("OLLAMA_HOST", "http://env/api/chat")

	res, err := Load()
	cfg := res.Config
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Source != SourceProject || res.Path != ProjectFile {
		t.Fatalf("source = %v, path = %q, want the project source and path despite environment overrides", res.Source, res.Path)
	}
	if cfg.Model != "from-env" || cfg.Endpoint != "http://env/api/chat" {
		t.Fatalf("Load() = %+v, want the environment to win", cfg)
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"model":`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("Load() error = %v, want a parse error", err)
	}
	if res.Path != ProjectFile || res.Source != SourceProject {
		t.Fatalf("error result = path %q, source %v, want the failing project source", res.Path, res.Source)
	}
}

func TestLoadReadErrorRetainsSourceContext(t *testing.T) {
	isolate(t)
	if err := os.Mkdir(ProjectFile, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load() error = %v, want a read error", err)
	}
	if res.Path != ProjectFile || res.Source != SourceProject {
		t.Fatalf("error result = path %q, source %v, want the failing project source", res.Path, res.Source)
	}
	if !reflect.DeepEqual(res.Config, Defaults()) {
		t.Fatalf("error result config = %+v, want the defaults retained", res.Config)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"modle":"typo"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("Load() error = %v, want the unknown field named", err)
	}
}

func TestLoadRejectsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ProjectFile, 0o644) })

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("Load() error = %v, want a read error", err)
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"max_turns":0}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err == nil || !strings.Contains(err.Error(), "max_turns must be > 0") {
		t.Fatalf("Load() error = %v, want validation to reject it", err)
	}
	if res.Config.MaxTurns != 0 || res.Path != ProjectFile || res.Source != SourceProject {
		t.Fatalf("error result = %+v from %q (source %v), want invalid project config preserved", res.Config, res.Path, res.Source)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"empty endpoint", func(c *Config) { c.Endpoint = "  " }, "endpoint must not be empty"},
		{"empty model", func(c *Config) { c.Model = "" }, "model must not be empty"},
		{"zero timeout", func(c *Config) { c.TimeoutSeconds = 0 }, "timeout_seconds must be > 0"},
		{"negative num_ctx", func(c *Config) { c.NumCtx = -1 }, "num_ctx must be > 0"},
		{"zero output budget", func(c *Config) { c.MaxOutputTokens = 0 }, "max_output_tokens must be > 0"},
		{"zero max turns", func(c *Config) { c.MaxTurns = 0 }, "max_turns must be > 0"},
		{"zero compaction", func(c *Config) { c.CompactThreshold = 0 }, "compact_threshold_bytes must be > 0"},
		{"zero history budget", func(c *Config) { c.MaxHistoryMessages = 0 }, "max_history_messages must be > 0"},
		{"zero slice budget", func(c *Config) { c.MaxSliceLines = 0 }, "max_slice_lines must be > 0"},
		{"zero line budget", func(c *Config) { c.MaxLineChars = 0 }, "max_line_chars must be > 0"},
		{"zero matches", func(c *Config) { c.SearchMaxMatches = 0 }, "search_max_matches must be > 0"},
		{"zero per file", func(c *Config) { c.SearchMaxPerFile = 0 }, "search_max_per_file must be > 0"},
		{"negative temperature", func(c *Config) { c.Temperature = -0.5 }, "temperature must be >= 0"},
		{"zero top_p", func(c *Config) { c.TopP = 0 }, "top_p must be in (0, 1]"},
		{"top_p above one", func(c *Config) { c.TopP = 1.5 }, "top_p must be in (0, 1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	cfg := Defaults()
	cfg.Model = ""
	cfg.MaxTurns = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "model") || !strings.Contains(err.Error(), "max_turns") {
		t.Fatalf("Validate() = %v, want both problems listed", err)
	}
}

func TestSearchOrder(t *testing.T) {
	t.Run("explicit path short-circuits", func(t *testing.T) {
		t.Setenv("METRON_CONFIG", "/somewhere/custom.json")
		if got := Search(); len(got) != 1 || got[0] != "/somewhere/custom.json" {
			t.Fatalf("Search() = %v, want only the explicit path", got)
		}
	})
	t.Run("project file then metron directory", func(t *testing.T) {
		t.Setenv("METRON_CONFIG", "")
		t.Setenv("METRON_CONFIG_DIR", "/xdg")
		want := []string{ProjectFile, filepath.Join("/xdg", ".metron", "config.json")}
		got := Search()
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("Search() = %v, want %v", got, want)
		}
	})
	t.Run("explicit project root then XDG", func(t *testing.T) {
		t.Setenv("METRON_CONFIG", "")
		t.Setenv("METRON_CONFIG_DIR", "/xdg")
		want := []string{filepath.Join("/repo", ProjectFile), filepath.Join("/xdg", ".metron", "config.json")}
		got := SearchFrom("/repo")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SearchFrom() = %v, want %v", got, want)
		}
	})
	t.Run("falls back to the home directory", func(t *testing.T) {
		t.Setenv("METRON_CONFIG", "")
		t.Setenv("METRON_CONFIG_DIR", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".metron", "config.json")
		got := Search()
		if len(got) != 2 || got[1] != want {
			t.Fatalf("Search() = %v, want the second entry to be %q", got, want)
		}
	})
	t.Run("uses specified directory", func(t *testing.T) {
		t.Setenv("METRON_CONFIG", "")
		t.Setenv("METRON_CONFIG_DIR", "/opt/metron-config")
		want := filepath.Join("/opt/metron-config", ".metron", "config.json")
		got := Search()
		if len(got) != 2 || got[1] != want {
			t.Fatalf("Search() = %v, want second entry %q", got, want)
		}
	})
}

func TestValidateRejectsUnknownDisabledTools(t *testing.T) {
	cfg := Defaults()
	cfg.DisabledTools = []string{"view_slice", "vieuw_slice"}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want a typo in disabled_tools rejected")
	}
	// A silently-ignored typo would leave the tool enabled, which is the
	// opposite of what the operator asked for.
	if !strings.Contains(err.Error(), "vieuw_slice") {
		t.Fatalf("Validate() error = %v, want it to name the unknown tool", err)
	}
	if !strings.Contains(err.Error(), "view_slice") {
		t.Fatalf("Validate() error = %v, want it to list the known tools", err)
	}
}

func TestValidateAcceptsEveryKnownTool(t *testing.T) {
	cfg := Defaults()
	cfg.DisabledTools = append([]string{}, tools.ToolNames...)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want every known tool name accepted", err)
	}
}

func TestValidateRejectsABlankAllowedCommand(t *testing.T) {
	cfg := Defaults()
	cfg.AllowedCommands = []string{"go test", "  "}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want a blank allowed_commands entry rejected")
	}
	// A blank entry parses to a zero-length prefix, which every argv begins
	// with -- so the typo would permit everything rather than nothing.
	if !strings.Contains(err.Error(), "allowed_commands[1]") {
		t.Fatalf("Validate() error = %v, want it to name the offending entry", err)
	}
}

func TestValidateRejectsNonPositiveCommandBudgets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Config)
	}{
		{"command_timeout_seconds", func(c *Config) { c.CommandTimeoutSeconds = 0 }},
		{"max_command_output_bytes", func(c *Config) { c.MaxCommandOutputBytes = -1 }},
	} {
		cfg := Defaults()
		tc.apply(&cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("Validate() error = %v, want %s rejected", err, tc.name)
		}
	}
}

// The tests below pin the trust boundary: a project file is untrusted data and
// may not raise the operator's privileges, while the operator's own settings --
// user file or METRON_CONFIG -- may.

func TestProjectFileCannotRaisePrivileges(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"auto_approve_patches":true,"allowed_commands":["go test","make"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Source != SourceProject {
		t.Fatalf("Source = %v, want SourceProject", res.Source)
	}
	if res.Config.AutoApprovePatches {
		t.Fatal("AutoApprovePatches survived a project file, want it stripped")
	}
	if len(res.Config.AllowedCommands) != 0 {
		t.Fatalf("AllowedCommands = %v, want it stripped from a project file", res.Config.AllowedCommands)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{
		"requested auto_approve_patches", "ignored",
		"requested allowed_commands", "METRON_ALLOW_PROJECT_COMMANDS",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %q, want it to mention %q", joined, want)
		}
	}
}

func TestProjectSafeSettingsAreKept(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"model":"gemma4:12b","max_slice_lines":40}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Config.Model != "gemma4:12b" || res.Config.MaxSliceLines != 40 {
		t.Fatalf("safe project settings = %+v, want them kept", res.Config)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for a project file that raises no privilege", res.Warnings)
	}
}

func TestProjectAllowedCommandsHonourOperatorOptIn(t *testing.T) {
	isolate(t)
	if err := os.WriteFile(ProjectFile, []byte(`{"auto_approve_patches":true,"allowed_commands":["go test"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METRON_ALLOW_PROJECT_COMMANDS", "1")

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(res.Config.AllowedCommands) != 1 || res.Config.AllowedCommands[0] != "go test" {
		t.Fatalf("AllowedCommands = %v, want the operator-opted-in grant kept", res.Config.AllowedCommands)
	}
	if res.Config.AutoApprovePatches {
		t.Fatal("AutoApprovePatches survived the project command opt-in, want it stripped regardless of opt-in")
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "you opted in") {
		t.Fatalf("warnings = %q, want the opt-in announced", joined)
	}
	if !strings.Contains(joined, "allowed_commands is non-empty") {
		t.Fatalf("warnings = %q, want the effective capability named", joined)
	}
}

func TestExplicitSourceMayRaisePrivileges(t *testing.T) {
	dir := isolate(t)
	userDir := filepath.Join(dir, ".metron")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(`{"model":"user","auto_approve_patches":false,"allowed_commands":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProjectFile, []byte(`{"model":"project","auto_approve_patches":false,"allowed_commands":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.json")
	if err := os.WriteFile(explicit, []byte(`{"model":"explicit","auto_approve_patches":true,"allowed_commands":["go test"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METRON_CONFIG", explicit)
	t.Setenv("OLLAMA_MODEL", "environment")
	t.Setenv("OLLAMA_HOST", "http://environment/api/chat")

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Source != SourceExplicit {
		t.Fatalf("Source = %v, want SourceExplicit", res.Source)
	}
	if res.Config.Model != "environment" || res.Config.Endpoint != "http://environment/api/chat" {
		t.Fatalf("explicit source = %+v, want environment values to override the explicit file", res.Config)
	}
	if !res.Config.AutoApprovePatches || len(res.Config.AllowedCommands) != 1 {
		t.Fatalf("explicit source = %+v, want its privileges kept", res.Config)
	}
	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"auto_approve_patches is enabled", "allowed_commands is non-empty"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %q, want the effective capability named: %q", joined, want)
		}
	}
	for _, mustNot := range []string{"ignored", "you opted in"} {
		if strings.Contains(joined, mustNot) {
			t.Errorf("warnings = %q, do not want a project-file notice for an explicit file", joined)
		}
	}
}

func TestUserSourceMayRaisePrivileges(t *testing.T) {
	dir := isolate(t)
	userDir := filepath.Join(dir, ".metron")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.json"), []byte(`{"auto_approve_patches":true,"allowed_commands":["go test"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Source != SourceUser {
		t.Fatalf("Source = %v, want SourceUser", res.Source)
	}
	if !res.Config.AutoApprovePatches || len(res.Config.AllowedCommands) != 1 || res.Config.AllowedCommands[0] != "go test" {
		t.Fatalf("user source = %+v, want both operator-configured capabilities kept", res.Config)
	}
}

func TestDefaultSourceHasNoWarnings(t *testing.T) {
	isolate(t)

	res, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if res.Source != SourceDefault {
		t.Fatalf("Source = %v, want SourceDefault", res.Source)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none for the built-in defaults", res.Warnings)
	}
}

func TestOptInProjectCommandsTruthiness(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"yes", true}, {"TRUE", true}, {" 1 ", true},
		{"", false}, {"0", false}, {"no", false}, {"maybe", false},
	}
	for _, tc := range tests {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("METRON_ALLOW_PROJECT_COMMANDS", tc.val)
			if got := optInProjectCommands(); got != tc.want {
				t.Fatalf("optInProjectCommands(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
