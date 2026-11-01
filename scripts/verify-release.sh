#!/bin/sh
# Checks the assets scripts/build-release.sh wrote for TAG:
#   - all four shiftgate-<os>-<arch> binaries exist and nothing else is listed,
#   - SHA256SUMS has exactly those four lines and every digest matches,
#   - the binary for this machine prints TAG from `shiftgate version`.
#
# usage: scripts/verify-release.sh TAG OUTPUT_DIR
set -eu

tool=shiftgate
version_args="version"

if [ "$#" -ne 2 ]; then
	echo "usage: $0 TAG OUTPUT_DIR" >&2
	exit 2
fi
tag=$1
output=$2

fail() {
	echo "verify-release: $*" >&2
	exit 1
}

for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
	[ -x "$output/$tool-$target" ] || fail "missing $tool-$target"
done
[ -f "$output/SHA256SUMS" ] || fail "missing SHA256SUMS"
listed=$(awk '{print $2}' "$output/SHA256SUMS" | sort | tr '\n' ' ')
want="$tool-darwin-amd64 $tool-darwin-arm64 $tool-linux-amd64 $tool-linux-arm64 "
[ "$listed" = "$want" ] || fail "SHA256SUMS lists '$listed', want '$want'"
(
	cd "$output"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -c SHA256SUMS >/dev/null
	else
		shasum -a 256 -c SHA256SUMS >/dev/null
	fi
) || fail "SHA256SUMS does not match the assets"

native="$output/$tool-$(go env GOOS)-$(go env GOARCH)"
# shellcheck disable=SC2086 # version_args is a fixed word list
out=$("$native" $version_args 2>&1) || fail "$native $version_args exited non-zero: $out"
# The version matches when one whitespace, "(", ")", "=" or ","-separated
# token equals the tag; fleet-update applies the same rule after install.
printf '%s\n' "$out" | tr -s ' \t()=,' '\n' | grep -Fxq -- "$tag" ||
	fail "'$tool $version_args' printed '$out', want the token '$tag'"
printf '%s %s: %s\n' "$tool" "$version_args" "$out"
