package tools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Tool names, as they are advertised to the model. They are declared here, in
// the package that implements them, because three separate things key off them:
// the schemas the agent sends, the dependency check below, and the operator's
// disabled_tools setting.
const (
	ToolListFiles  = "list_files"
	ToolFindSymbol = "find_symbol"
	ToolSearchText = "search_text"
	ToolViewSlice  = "view_slice"
	ToolApplyPatch = "apply_patch"
	ToolRunCommand = "run_command"
	ToolEditFile   = "edit_file"
)

// ToolNames lists every tool metron can expose, in the order the model sees
// them: discover, locate, read, then change.
var ToolNames = []string{ToolListFiles, ToolFindSymbol, ToolSearchText, ToolViewSlice,
	ToolApplyPatch, ToolEditFile, ToolRunCommand}

// Edit formats. Exactly one is offered at a time: they do the same job, and
// advertising both would pay for two schemas on every request and invite the
// model to pick the wrong one.
const (
	FormatDiff          = "diff"
	FormatSearchReplace = "search_replace"
)

// EditFormats lists the valid edit_format settings.
var EditFormats = []string{FormatDiff, FormatSearchReplace}

// Dependency describes one external binary and the tools that stop working
// without it. ripgrep backs two of them, which is why this is a list.
type Dependency struct {
	Binary string
	Tools  []string
	Hint   string
}

// ctagsHint is named rather than inlined because two places print it: the
// dependency table below, and the degraded-index note in Preflight.
const ctagsHint = "install Universal Ctags (brew install universal-ctags)"

// dependencies is the full set of external programs metron shells out to.
// view_slice is absent on purpose: it reads files directly, so it is the one
// tool that is always available.
var dependencies = []Dependency{
	{Binary: "rg", Tools: []string{ToolListFiles, ToolSearchText}, Hint: "install ripgrep (brew install ripgrep)"},
	{Binary: "ctags", Tools: []string{ToolFindSymbol}, Hint: ctagsHint},
	{Binary: "git", Tools: []string{ToolApplyPatch}, Hint: "install git"},
}

// problem is one unusable dependency: what is wrong, and which tools it costs.
type problem struct {
	dep    Dependency
	reason string
}

// problems evaluates every dependency once. Preflight and UnavailableTools both
// read from it, so the warning an operator sees and the tool set the model is
// offered can never disagree.
func (e Env) problems() []problem {
	var found []problem
	for _, dep := range dependencies {
		var reason string
		switch {
		case dep.Binary == "ctags":
			// A broken ctags costs find_symbol nothing when metron can index
			// the project itself, which on a Go project it can. Reporting it
			// here would withdraw a tool that works.
			if reason = ctagsFault(); reason == "" || e.canIndexSymbols() {
				continue
			}
		case !onPath(dep.Binary):
			reason = fmt.Sprintf("%s not found on PATH", dep.Binary)
		case dep.Binary == "git" && !e.insideWorkTree():
			reason = "the project is not a git repository"
		default:
			continue
		}
		found = append(found, problem{dep: dep, reason: reason})
	}
	return found
}

// canIndexSymbols reports whether find_symbol can answer without a working
// ctags: from an index a previous run left behind, or from Go source the
// built-in parser reads directly.
func (e Env) canIndexSymbols() bool {
	return e.tagsExist() || e.hasGoSources()
}

// Preflight returns one human-readable warning per problem found. An empty
// result means every tool is ready. Nothing here is fatal: a missing binary
// only costs the tools that need it, and those are no longer offered to the
// model at all.
func (e Env) Preflight() []string {
	var warnings []string
	for _, p := range e.problems() {
		hint := p.dep.Hint
		if p.reason == "the project is not a git repository" {
			hint = "start metron inside a git repository"
		}
		warnings = append(warnings, fmt.Sprintf("%s - %s (%s)",
			p.reason, unavailablePhrase(p.dep.Tools), hint))
	}
	// Not a problem: find_symbol still runs. It is still worth saying, because
	// the operator is one index short of what they think they have -- the
	// built-in one reads Go and nothing else, so on a mixed repository the
	// answer silently narrows.
	if fault := ctagsFault(); fault != "" && e.canIndexSymbols() {
		warnings = append(warnings, fmt.Sprintf(
			"%s - find_symbol is falling back to metron's built-in index, which covers Go only (%s)",
			fault, ctagsHint))
	}
	return warnings
}

// UnavailableTools maps each tool that cannot run here to the reason, so the
// agent can leave it out of what it advertises and explain itself if the model
// asks for it anyway.
func (e Env) UnavailableTools() map[string]string {
	out := make(map[string]string)
	for _, p := range e.problems() {
		for _, tool := range p.dep.Tools {
			out[tool] = fmt.Sprintf("%s (%s)", p.reason, p.dep.Hint)
		}
	}
	// run_command has no binary behind it; what it needs is permission. An
	// empty allowlist is the default, so this is the usual state rather than a
	// fault -- which is why it is reported here, where it withdraws the tool,
	// and not by Preflight, which is for things that are wrong.
	if len(e.Allowed) == 0 {
		out[ToolRunCommand] = "no commands are permitted (set allowed_commands in .metron.json)"
	}
	// The two edit tools are alternatives, so whichever is not selected is
	// withdrawn. The reason names the one that is available, which is the only
	// thing the model needs to know.
	if e.EditFormat == FormatSearchReplace {
		out[ToolApplyPatch] = "edit_format is \"search_replace\"; use edit_file instead"
	} else {
		out[ToolEditFile] = "edit_format is \"diff\"; use apply_patch instead"
	}
	return out
}

// unavailablePhrase renders a tool list with the verb agreeing, since a missing
// ripgrep costs two tools and a missing ctags costs one.
func unavailablePhrase(tools []string) string {
	verb := "is"
	if len(tools) > 1 {
		verb = "are"
	}
	return fmt.Sprintf("%s %s unavailable", strings.Join(tools, ", "), verb)
}

func onPath(binary string) bool {
	_, err := exec.LookPath(binary)
	return err == nil
}

// ctagsFault reports why the ctags on PATH cannot build metron's index, or ""
// if it can. The BSD ctags shipped with macOS is the case this exists for: it
// is present, it is on PATH, and it rejects the field flags EnsureTags relies
// on -- which is why a plain LookPath is not the test.
func ctagsFault() string {
	if !onPath("ctags") {
		return "ctags not found on PATH"
	}
	out, err := exec.Command("ctags", "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Universal Ctags") {
		return "ctags on PATH is not Universal Ctags"
	}
	return ""
}

// insideWorkTree reports whether the project is inside a git repository. Having
// the git binary is not enough: apply_patch shells out to `git apply`, which
// outside a repository fails with a message the model then wastes turns trying
// to fix as if it were a bad diff.
func (e Env) insideWorkTree() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = e.Root
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// RebuildTags discards any existing ctags index and builds a fresh one. The
// index is otherwise generated once and never invalidated, so this is how an
// operator picks up renames and new files mid-session.
func (e Env) RebuildTags() error {
	if err := os.Remove(e.tagsFile()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale index: %w", err)
	}
	// With no usable ctags the answers come from the built-in Go index, which
	// is rebuilt on every lookup and so has nothing to invalidate. Dropping the
	// stale file above was the whole job; reporting the absent ctags as a
	// failure would be reporting a tool that works as broken.
	if ctagsFault() != "" && e.hasGoSources() {
		return nil
	}
	if err := e.EnsureTags(); err != nil {
		return fmt.Errorf("rebuild ctags index: %w", err)
	}
	return nil
}
