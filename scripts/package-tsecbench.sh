#!/usr/bin/env bash
set -euo pipefail

image="${1:-chronos-tsecbench:submission}"
output="${2:-dist/chronos-tsecbench-amd64.tar.gz}"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to build the TSecBench submission image" >&2
  exit 1
fi

mkdir -p "$(dirname "$output")"
echo "Building $image for linux/amd64 (this does not start an evaluation)..."
docker buildx build --platform linux/amd64 --load -f Dockerfile.tsecbench -t "$image" .

echo "Exporting $output..."
docker save "$image" | gzip -9 >"$output"
shasum -a 256 "$output" >"$output.sha256"

size_bytes="$(wc -c <"$output" | tr -d ' ')"
max_bytes=$((3 * 1024 * 1024 * 1024))
if (( size_bytes > max_bytes )); then
  echo "submission archive exceeds the 3 GB platform limit" >&2
  exit 1
fi

echo "Prepared $output ($size_bytes bytes)"
cat "$output.sha256"
echo "No container was run and no evaluation was started."
