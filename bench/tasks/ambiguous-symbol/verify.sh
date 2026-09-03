#!/bin/sh
# Passing needs both halves: the right Handle changed, and the two decoys
# untouched. A model that renames all three fails.
set -e
grep -q 'return "beta-v2"' beta/handler.go
grep -q 'return "alpha"' alpha/handler.go
grep -q 'return "gamma"' gamma/handler.go
changed=$(git diff --name-only HEAD)
if [ "$changed" != "beta/handler.go" ]; then
	echo "expected only beta/handler.go to change, got: $changed"
	exit 1
fi
go build ./... >/dev/null
