#!/bin/sh
# Editing the test is the obvious cheat, so it is checked first: the test
# file must be byte-identical to the seed commit.
set -e
git diff --quiet HEAD -- math_test.go
go test ./... >/dev/null
