package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Matrix is the set of cells to measure: every model crossed with every edit
// format, repeated Repetitions times.
type Matrix struct {
	// Endpoint is the Ollama chat endpoint. The runner derives /api/tags from
	// it to decide which models are installed.
	Endpoint string `json:"endpoint"`

	Models      []string `json:"models"`
	EditFormats []string `json:"edit_formats"`

	// Repetitions is how many times each cell runs. Local models are
	// nondeterministic, so a single run reports luck rather than capability.
	Repetitions int `json:"repetitions"`
}

// loadMatrix reads and validates a matrix file. Unknown keys are an error for
// the same reason they are in internal/config: a typo that silently changes
// what got measured is worse than a startup failure.
func loadMatrix(path string) (Matrix, error) {
	var m Matrix
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read matrix: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("parse matrix %s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return m, fmt.Errorf("matrix %s: %w", path, err)
	}
	return m, nil
}

func (m Matrix) validate() error {
	var problems []string
	if strings.TrimSpace(m.Endpoint) == "" {
		problems = append(problems, "endpoint must not be empty")
	}
	if len(m.Models) == 0 {
		problems = append(problems, "models must not be empty")
	}
	if len(m.EditFormats) == 0 {
		problems = append(problems, "edit_formats must not be empty")
	}
	for _, f := range m.EditFormats {
		if f != "diff" && f != "search_replace" {
			problems = append(problems, fmt.Sprintf("unknown edit format %q", f))
		}
	}
	if m.Repetitions <= 0 {
		problems = append(problems, fmt.Sprintf("repetitions must be > 0 (got %d)", m.Repetitions))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// tagsURL turns the configured chat endpoint into the model-listing endpoint.
// Ollama serves both from the same host, so there is no second thing for an
// operator to configure and get wrong.
func tagsURL(endpoint string) string {
	if i := strings.Index(endpoint, "/api/"); i >= 0 {
		return endpoint[:i] + "/api/tags"
	}
	return strings.TrimSuffix(endpoint, "/") + "/api/tags"
}
