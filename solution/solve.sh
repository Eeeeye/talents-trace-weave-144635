#!/bin/bash
set -Eeuo pipefail

workspace="${TRACE_WEAVE_WORKSPACE:-/workspace/trace-weave}"

declare -a repaired=(
  "internal/format/record.go"
  "internal/merge/merge.go"
  "internal/output/writer.go"
  "internal/runner/runner.go"
  "internal/source/source.go"
)

for relative in "${repaired[@]}"; do
  source_path="/solution/files/${relative}"
  destination="${workspace}/${relative}"
  if [[ ! -f "${source_path}" || ! -f "${destination}" ]]; then
    printf 'missing reference or workspace file: %s\n' "${relative}" >&2
    exit 1
  fi
  cp -- "${source_path}" "${destination}"
done

cd "${workspace}"
gofmt -w "${repaired[@]}"
