#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
version=${TOXIPROXY_VERSION:-2.12.0}
out=${TOXIPROXY_OUTPUT:-"$root/bin/toxiproxy-server"}
if [[ ${1:-} == --output ]]; then
    [[ $# -eq 2 ]] || { printf 'Usage: %s [--output PATH]\n' "$0" >&2; exit 2; }
    out=$2
elif [[ $# -ne 0 ]]; then
    printf 'Usage: %s [--output PATH]\n' "$0" >&2
    exit 2
fi
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    printf 'TOXIPROXY_VERSION must be a stable semantic version\n' >&2
    exit 2
fi
command -v go >/dev/null || { printf 'Go is required to build official Toxiproxy %s\n' "$version" >&2; exit 1; }
workspace=$(mktemp -d "${TMPDIR:-/tmp}/toxiproxy-build.XXXXXX")
trap 'rm -rf "$workspace"' EXIT
mkdir -p "$(dirname "$out")"
out="$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
(cd "$workspace" && GOWORK=off go mod download -json "github.com/Shopify/toxiproxy/v2@v$version") > "$workspace/module.json"
source=$(jq -er '.Dir' "$workspace/module.json")
(cd "$source" && GOWORK=off CGO_ENABLED=0 go build -mod=readonly -ldflags "-X github.com/Shopify/toxiproxy/v2.Version=$version" -o "$out" ./cmd/server)
printf 'Built official Shopify Toxiproxy %s: %s\n' "$version" "$out"
