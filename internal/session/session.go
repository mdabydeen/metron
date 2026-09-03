// Package session persists a conversation to disk so it survives exiting.
//
// The format is JSONL: the first line is metadata, every line after it is one
// message. That keeps a transcript greppable and tailable with ordinary tools,
// which matters because these files are also the benchmark's raw output and the
// first thing anyone will want when reporting a bug.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mdabydeen/metron/internal/ollama"
)

// Dir is the directory, relative to the project root, that metron keeps its own
// state in.
const Dir = ".metron"

// Meta is the first line of a transcript: what this conversation was, and what
// the tree looked like when it was recorded.
type Meta struct {
	ID      string    `json:"id"`
	Started time.Time `json:"started"`
	Model   string    `json:"model"`
	// GitHead is the commit the project was on when the session was saved.
	// Resuming against a different one is allowed but warned about: the
	// conversation refers to line numbers and file contents that have moved.
	GitHead string `json:"git_head,omitempty"`
	Dirty   bool   `json:"dirty,omitempty"`
}

// NewID returns a sortable, human-readable session identifier.
func NewID(now time.Time) string {
	return now.UTC().Format("20060102-150405")
}

// Store reads and writes transcripts under a project root.
type Store struct {
	Root string
}

// dir is where transcripts live.
func (s Store) dir() string {
	return filepath.Join(s.Root, Dir, "sessions")
}

// idPattern is what a session id may look like: the timestamp NewID produces,
// and nothing else. Path joins the id straight onto a directory, so without
// this "--resume ../../../../etc/foo" would read outside the store.
var idPattern = regexp.MustCompile(`^\d{8}-\d{6}$`)

// ValidID reports whether an id is well formed.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Path is the transcript file for one session id.
func (s Store) Path(id string) string {
	return filepath.Join(s.dir(), id+".jsonl")
}

// ensureDir creates the state directory, and makes it invisible to git.
//
// The .gitignore holding "*" is deliberate: metron writes into a directory it
// does not own, and a tool that leaves untracked files in someone's repository
// every time it runs is a tool people stop running. Ignoring itself needs no
// cooperation from the project's own .gitignore.
func (s Store) ensureDir() error {
	dir := s.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	marker := filepath.Join(s.Root, Dir, ".gitignore")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	return os.WriteFile(marker, []byte("*\n"), 0o644)
}

// Save writes the whole transcript, replacing any previous copy.
//
// Rewriting rather than appending is what keeps the file consistent with the
// agent's actual history: compaction rewrites old messages in place and
// trimming drops them, so an append-only log would accumulate a history the
// agent no longer has. The write goes through a temporary file and a rename, so
// a crash mid-save leaves the previous transcript rather than half of a new one.
func (s Store) Save(meta Meta, messages []ollama.Message) error {
	if err := s.ensureDir(); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	// Serialise into memory first, so the only error left below is the one that
	// actually happens in practice: not being able to write the file. The
	// discarded error is not carelessness -- bytes.Buffer.Write never fails, and
	// writeTranscript's own failure paths are exercised directly in its test,
	// against a writer that does.
	var buf bytes.Buffer
	_ = writeTranscript(&buf, meta, messages)

	// 0600, not 0644: a transcript holds every tool result the model saw, which
	// is every file it read. That is the operator's to see, not the machine's.
	final := s.Path(meta.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	// Rename last, so a crash mid-save leaves the previous transcript intact
	// rather than half of a new one.
	return os.Rename(tmp, final)
}

// writeTranscript serialises one session: metadata first, then a line per
// message. It takes a writer rather than the file so the failure paths are
// reachable in a test without having to fill a disk.
func writeTranscript(w io.Writer, meta Meta, messages []ollama.Message) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("write session metadata: %w", err)
	}
	for _, m := range messages {
		if err := enc.Encode(m); err != nil {
			return fmt.Errorf("write session message: %w", err)
		}
	}
	return nil
}

// Load reads a transcript back.
func (s Store) Load(id string) (Meta, []ollama.Message, error) {
	if !ValidID(id) {
		return Meta{}, nil, fmt.Errorf("%q is not a session id; ids look like 20260903-142530", id)
	}
	f, err := os.Open(s.Path(id))
	if err != nil {
		return Meta{}, nil, fmt.Errorf("open session %s: %w", id, err)
	}
	defer f.Close()

	var (
		meta     Meta
		messages []ollama.Message
	)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)
	for first := true; scanner.Scan(); first = false {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if first {
			if err := json.Unmarshal(line, &meta); err != nil {
				return Meta{}, nil, fmt.Errorf("parse session metadata: %w", err)
			}
			continue
		}
		var m ollama.Message
		if err := json.Unmarshal(line, &m); err != nil {
			return Meta{}, nil, fmt.Errorf("parse session message: %w", err)
		}
		messages = append(messages, m)
	}
	if err := scanner.Err(); err != nil {
		return Meta{}, nil, fmt.Errorf("read session %s: %w", id, err)
	}
	if meta.ID == "" {
		return Meta{}, nil, fmt.Errorf("session %s has no metadata", id)
	}
	return meta, messages, nil
}

// maxLine bounds a single transcript line. A tool result can be large before
// compaction runs, and the scanner default of 64KB would reject it.
const maxLine = 4 << 20

// List returns the saved session ids, newest first. The ids are timestamps, so
// a reverse lexical sort is a reverse chronological one.
func (s Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session directory: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".jsonl") {
			ids = append(ids, strings.TrimSuffix(name, ".jsonl"))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// Latest returns the most recent session id, or "" when there is none.
func (s Store) Latest() (string, error) {
	ids, err := s.List()
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

// Head records the commit the project is on, and whether the tree is dirty.
// Both are best-effort: outside a git repository there is simply nothing to
// stamp, and that is not an error.
func Head(root string) (rev string, dirty bool) {
	run := func(args ...string) (string, bool) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	rev, ok := run("rev-parse", "HEAD")
	if !ok {
		return "", false
	}
	status, ok := run("status", "--porcelain")
	return rev, ok && status != ""
}

// DriftWarning reports why a resumed session may not line up with the tree, or
// "" when it does. A transcript is full of line numbers and quoted code; if the
// tree has moved underneath it, the model will confidently act on things that
// are no longer true, and the operator should be the one to decide whether that
// matters.
func DriftWarning(meta Meta, root string) string {
	if meta.GitHead == "" {
		return ""
	}
	rev, dirty := Head(root)
	switch {
	case rev == "":
		return "the session was recorded in a git repository and this is not one"
	case rev != meta.GitHead:
		return fmt.Sprintf("the project has moved from %s to %s since this session was saved",
			short(meta.GitHead), short(rev))
	case dirty && !meta.Dirty:
		return "the working tree has uncommitted changes that it did not have when this session was saved"
	default:
		return ""
	}
}

func short(rev string) string {
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}
