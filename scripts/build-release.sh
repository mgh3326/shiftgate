#!/bin/sh
# Builds the GitHub Release assets for one tag from the checked-out commit:
#   shiftgate-<os>-<arch> for linux/amd64, linux/arm64, darwin/amd64 and
#   darwin/arm64, plus SHA256SUMS over those four files.
#
# Every binary is CGO-disabled (CGO_ENABLED=0: fully static on Linux; Darwin
# links only the system libSystem), built with -trimpath, and stamps the
# tag with -X main.version so `shiftgate version` prints it.
#
# usage: scripts/build-release.sh TAG OUTPUT_DIR
# Prints the tag on stdout. The release workflow and its pull-request dry run
# both call this script; scripts/verify-release.sh checks what it wrote.
set -eu

tool=shiftgate
pkg=./cmd/shiftgate
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

if [ "$#" -ne 2 ]; then
	echo "usage: $0 TAG OUTPUT_DIR" >&2
	exit 2
fi
tag=$1
output=$2
if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
	echo "build-release: tag '$tag' is not vMAJOR.MINOR.PATCH[-PRERELEASE]" >&2
	exit 1
fi

repo_root=$(CDPATH="" cd -- "$(dirname -- "$0")/.." && pwd)
case "$output" in
/*) ;;
*) output="$(pwd)/$output" ;;
esac
mkdir -p "$output"
commit=$(git -C "$repo_root" rev-parse HEAD)

assets=
for target in $targets; do
	os=${target%/*}
	arch=${target#*/}
	asset="$tool-$os-$arch"
	(
		cd "$repo_root"
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$tag -X main.commit=$commit" \
			-o "$output/$asset" "$pkg"
	)
	assets="$assets $asset"
done

(
	cd "$output"
	# shellcheck disable=SC2086 # asset names contain no whitespace
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum $assets >SHA256SUMS
	else
		shasum -a 256 $assets >SHA256SUMS
	fi
)
printf '%s\n' "$tag"
