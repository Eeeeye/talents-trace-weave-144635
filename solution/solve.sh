#!/bin/bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
reference_root="${script_dir}/files"

workspace="${TRACE_WEAVE_WORKSPACE:-/workspace/trace-weave}"
if [[ ! -d "${workspace}" && -f "${PWD}/go.mod" ]]; then
  workspace="$(pwd -P)"
fi

declare -a repaired=(
  "internal/format/record.go"
  "internal/merge/merge.go"
  "internal/output/writer.go"
  "internal/runner/runner.go"
  "internal/source/source.go"
)

for relative in "${repaired[@]}"; do
  source_path="${reference_root}/${relative}"
  destination="${workspace}/${relative}"
  if [[ ! -f "${source_path}" || ! -f "${destination}" ]]; then
    printf 'missing reference or workspace file: %s\n' "${relative}" >&2
    exit 1
  fi
  cp -- "${source_path}" "${destination}"
done

cd "${workspace}"
/usr/local/go/bin/gofmt -w "${repaired[@]}"
