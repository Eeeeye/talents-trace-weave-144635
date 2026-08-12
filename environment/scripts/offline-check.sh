#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

export GOPROXY=off
export GOSUMDB=off
export GOTOOLCHAIN=local
export GOFLAGS=-mod=readonly

go test -buildvcs=false ./...
make build

third_party="$(go list -m all | sed '1d')"
if [[ -n "${third_party}" ]]; then
    echo "offline-check: unexpected third-party modules:" >&2
    echo "${third_party}" >&2
    exit 1
fi

echo "offline build and tests passed"
