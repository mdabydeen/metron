#!/bin/sh
set -e
cat > zz_verify_test.go <<'EOF'
package bench

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetriesField(t *testing.T) {
	if got := Default().Retries; got != 3 {
		t.Fatalf("Default().Retries = %d, want 3", got)
	}
	data, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"retries":3`) {
		t.Fatalf("marshalled config %s lacks the retries tag", data)
	}
	if got := Default().Host; got != "localhost" {
		t.Fatalf("Default().Host = %q, want localhost", got)
	}
}
EOF
go test ./... >/dev/null
