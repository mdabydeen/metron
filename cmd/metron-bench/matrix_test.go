package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMatrix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "matrix.json")
	writeFile(t, path, `{"endpoint":"http://x/api/chat","models":["m"],"edit_formats":["diff"],"repetitions":2}`, 0o644)
	m, err := loadMatrix(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Repetitions != 2 || m.Models[0] != "m" {
		t.Fatalf("unexpected matrix: %+v", m)
	}
}

func TestLoadMatrixRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body, want string
	}{
		{"unreadable", "", "read matrix"},
		{"malformed", `{`, "parse matrix"},
		{"unknown key", `{"endpoint":"e","models":["m"],"edit_formats":["diff"],"repetitions":1,"nope":1}`, "parse matrix"},
		{"empty endpoint", `{"endpoint":"","models":["m"],"edit_formats":["diff"],"repetitions":1}`, "endpoint must not be empty"},
		{"no models", `{"endpoint":"e","models":[],"edit_formats":["diff"],"repetitions":1}`, "models must not be empty"},
		{"no formats", `{"endpoint":"e","models":["m"],"edit_formats":[],"repetitions":1}`, "edit_formats must not be empty"},
		{"bad format", `{"endpoint":"e","models":["m"],"edit_formats":["telepathy"],"repetitions":1}`, `unknown edit format "telepathy"`},
		{"no reps", `{"endpoint":"e","models":["m"],"edit_formats":["diff"],"repetitions":0}`, "repetitions must be > 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if tc.name != "unreadable" {
				writeFile(t, path, tc.body, 0o644)
			}
			_, err := loadMatrix(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTagsURL(t *testing.T) {
	if got := tagsURL("http://localhost:11434/api/chat"); got != "http://localhost:11434/api/tags" {
		t.Fatalf("got %q", got)
	}
	if got := tagsURL("http://localhost:11434/"); got != "http://localhost:11434/api/tags" {
		t.Fatalf("got %q", got)
	}
}
