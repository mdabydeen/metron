package session

import (
	"os"
	"path/filepath"
	"testing"

	"metron/internal/ollama"
)

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".metron", "session.json")
	msgs := []ollama.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}

	if err := Save(path, msgs); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("Load() = %d messages, want %d", len(got), len(msgs))
	}
	for i := range msgs {
		if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, got[i], msgs[i])
		}
	}
}

func TestSaveCreatesTheContainingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "session.json")

	if err := Save(path, []ollama.Message{{Role: "system", Content: "x"}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() error = %v, want the created directory to hold a readable file", err)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("Load() error = nil, want a missing session file reported")
	}
}

func TestLoadReportsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := Save(path, nil); err != nil {
		t.Fatal(err)
	}
	// Overwrite with garbage after Save proves the directory exists.
	if err := writeGarbage(path); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() error = nil, want malformed JSON reported")
	}
}

func TestSaveReportsAnUnwritableDirectory(t *testing.T) {
	if isRoot() {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	if err := chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chmod(dir, 0o755) })

	err := Save(filepath.Join(dir, "sub", "session.json"), nil)
	if err == nil {
		t.Fatal("Save() error = nil, want the unwritable directory reported")
	}
}

func TestSaveReportsAnUnmarshalableMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	tc := ollama.ToolCall{}
	tc.Function.Arguments = map[string]any{"bad": make(chan int)}
	msgs := []ollama.Message{{Role: "assistant", ToolCalls: []ollama.ToolCall{tc}}}

	err := Save(path, msgs)

	if err == nil {
		t.Fatal("Save() error = nil, want the marshal failure reported")
	}
}

func TestSaveReportsAnUnwritableFile(t *testing.T) {
	dir := t.TempDir()
	// A directory at the target path: MkdirAll(dirname) succeeds trivially,
	// but WriteFile onto a directory fails.
	path := filepath.Join(dir, "session.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err := Save(path, []ollama.Message{{Role: "system", Content: "x"}})

	if err == nil {
		t.Fatal("Save() error = nil, want writing onto a directory to fail")
	}
}
