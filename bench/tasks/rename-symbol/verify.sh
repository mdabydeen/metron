#!/bin/sh
# Judges the repository, never the answer text.
set -e
grep -q '^func Salutation() string {$' greet.go
grep -q 'Salutation()' service.go
# No trace of the old name may survive, in either file.
if grep -rn 'Greet' --include='*.go' . >/dev/null 2>&1; then
	echo "the old name Greet is still present"
	exit 1
fi
go build ./... >/dev/null
