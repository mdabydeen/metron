#!/bin/sh
# This task is the thesis. Passing it is necessary but not sufficient: the
# runner also enforces max_prompt_tokens from task.json, because succeeding
# by pulling 3000 lines into the context is the failure mode metron exists
# to prevent.
set -e
cat > zz_verify_test.go <<'EOF'
package bench

import "testing"

func TestRule0431(t *testing.T) {
	if got := Rule0431(3); got != 21 {
		t.Fatalf("Rule0431(3) = %d, want 21", got)
	}
	// The neighbours must be untouched.
	if got := Rule0430(3); got != 6 {
		t.Fatalf("Rule0430(3) = %d, want 6", got)
	}
	if got := Rule0432(3); got != 6 {
		t.Fatalf("Rule0432(3) = %d, want 6", got)
	}
	if got := Rule0000(3); got != 6 {
		t.Fatalf("Rule0000(3) = %d, want 6", got)
	}
	if got := Rule0498(3); got != 6 {
		t.Fatalf("Rule0498(3) = %d, want 6", got)
	}
}
EOF
go test ./... >/dev/null

# A surgical edit, not a rewrite: at most two lines added and two removed.
churn=$(git diff --numstat HEAD -- big.go | awk '{ print $1 + $2 }')
if [ -z "$churn" ]; then
	echo "big.go was not modified"
	exit 1
fi
if [ "$churn" -gt 4 ]; then
	echo "big.go churn of $churn lines is a rewrite, not an edit"
	exit 1
fi
