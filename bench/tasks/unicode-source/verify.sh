#!/bin/sh
# Byte-exact: the whole file is compared against the expected result, so a
# run that mangles a multi-byte rune, re-indents, or strips the emoji fails
# even though the Go still compiles.
set -e
cat > zz_expected.txt <<'EOF'
package bench

// Farewell は別れの挨拶。🎌 Multi-byte on purpose.
const Farewell = "さようなら、世界"

// Greet returns the greeting.
func Greet() string {
	return Farewell
}
EOF
if ! cmp -s zz_expected.txt greeting.go; then
	echo "greeting.go does not match the expected bytes:"
	diff zz_expected.txt greeting.go || true
	exit 1
fi
go build ./... >/dev/null
