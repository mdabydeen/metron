#!/bin/sh
# The check lives here, not in the seed, so the model cannot read the
# assertion it has to satisfy -- or satisfy it by editing the assertion.
set -e
cat > zz_verify_test.go <<'EOF'
package bench

import "testing"

func TestLengthGuardsNil(t *testing.T) {
	if got := Length(nil); got != 0 {
		t.Fatalf("Length(nil) = %d, want 0", got)
	}
	if got := Length(&Node{Value: "abc"}); got != 3 {
		t.Fatalf("Length(abc) = %d, want 3", got)
	}
}
EOF
go test ./... >/dev/null
