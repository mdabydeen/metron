#!/bin/sh
# The answer has to be written into the source, so what is graded is a
# source line rather than a sentence the model produced about itself.
set -e
go build ./... >/dev/null
awk '/^func compute\(/ { print prev } { prev = $0 }' pipeline.go > zz_above.txt
grep -qx '// called by RunPipeline' zz_above.txt
