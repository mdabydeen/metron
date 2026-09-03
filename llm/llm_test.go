package llm

import (
	"testing"
	"time"
)

func TestUsageAccumulatesAcrossCalls(t *testing.T) {
	// A Step makes several model calls; what it cost is their sum, and that
	// number is the one this whole program exists to keep small.
	var total Usage
	total.Add(Usage{PromptTokens: 100, GenTokens: 10})
	total.Add(Usage{PromptTokens: 250, GenTokens: 40})

	if total.PromptTokens != 350 || total.GenTokens != 50 {
		t.Fatalf("Usage = %+v, want the calls summed", total)
	}
}

func TestDefaultOptionsMatchTheDocumentedDefaults(t *testing.T) {
	got := DefaultOptions()

	if got.Temperature != 0.1 || got.TopP != 0.95 || got.NumCtx != 16384 {
		t.Fatalf("DefaultOptions() = %+v, want the documented sampling defaults", got)
	}
	// The timeout bounds silence, not total generation: a large local model
	// legitimately spends minutes on one reply.
	if got.Timeout != 180*time.Second {
		t.Fatalf("DefaultOptions().Timeout = %v, want 180s", got.Timeout)
	}
	if got.Stream || got.Sink != nil || got.APIKey != "" {
		t.Fatalf("DefaultOptions() = %+v, want streaming and auth off by default", got)
	}
}
