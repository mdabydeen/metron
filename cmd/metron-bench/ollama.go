package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// installedModels asks Ollama which models are pulled. A cell whose model is
// not installed is skipped rather than failed -- the same posture the
// -tags=live tests take, because "you have not pulled that model" is a fact
// about the machine, not a regression in metron.
func installedModels(ctx context.Context, endpoint string) (map[string]bool, error) {
	url := tagsURL(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("model list: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model list %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list %s: HTTP %d", url, resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("model list %s: %w", url, err)
	}
	installed := make(map[string]bool, len(body.Models))
	for _, m := range body.Models {
		if m.Name != "" {
			installed[m.Name] = true
		}
		if m.Model != "" {
			installed[m.Model] = true
		}
	}
	return installed, nil
}
