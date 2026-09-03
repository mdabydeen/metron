#!/bin/sh
set -e
cat > zz_verify_test.go <<'EOF'
package bench

import "testing"

func TestLast(t *testing.T) {
	if got := Last([]int{1, 2, 3}); got != 3 {
		t.Fatalf("Last([1 2 3]) = %d, want 3", got)
	}
	if got := Last([]int{7}); got != 7 {
		t.Fatalf("Last([7]) = %d, want 7", got)
	}
}
EOF
go test ./... >/dev/null
