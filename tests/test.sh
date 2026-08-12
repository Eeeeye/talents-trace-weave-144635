#!/bin/bash
set -Eeuo pipefail

workspace="${TRACE_WEAVE_WORKSPACE:-/workspace/trace-weave}"
verifier_root="/logs/verifier"
reward_path="${verifier_root}/reward.txt"
log_path="${verifier_root}/trace-weave-tests.log"
scratch=""
case_root=""

mkdir -p /logs "${verifier_root}"
chown 0:0 /logs "${verifier_root}"
chmod 0755 /logs
chmod 0755 "${verifier_root}"
rm -f -- "${reward_path}" "${log_path}"
install -o 0 -g 0 -m 0644 /dev/null "${reward_path}"
install -o 0 -g 0 -m 0644 /dev/null "${log_path}"
printf '0\n' >"${reward_path}"
exec > >(tee -a "${log_path}") 2>&1

cleanup() {
  if [[ -n "${case_root}" && -d "${case_root}" ]]; then
    rm -rf -- "${case_root}"
  fi
  if [[ -n "${scratch}" && -d "${scratch}" ]]; then
    rm -rf -- "${scratch}"
  fi
}
trap cleanup EXIT

fail() {
  printf 'trace-weave verifier integrity failure: %s\n' "$1" >&2
  exit 1
}

[[ -d "${workspace}" && ! -L "${workspace}" ]] || fail "workspace is missing or is a symlink"
[[ -d /tests && ! -L /tests ]] || fail "verifier tests are missing or are a symlink"

while IFS= read -r -d '' entry; do
  name="$(basename -- "${entry}")"
  case "${name}" in
    .dockerignore|.gitignore|Dockerfile|LICENSE|Makefile|README.md|bin|cmd|configs|go.mod|internal|scripts)
      ;;
    *)
      fail "path outside the allowed edit surface: ${name}"
      ;;
  esac
done < <(find "${workspace}" -mindepth 1 -maxdepth 1 -print0)

if [[ -e "${workspace}/go.sum" ]]; then
  fail "go.sum is outside the allowed edit surface"
fi
while IFS= read -r -d '' test_path; do
  relative="${test_path#"${workspace}/"}"
  case "${relative}" in
    internal/checkpoint/checkpoint_test.go|\
    internal/config/config_test.go|\
    internal/format/format_test.go|\
    internal/generator/generator_test.go|\
    internal/manifest/manifest_test.go|\
    internal/model/model_test.go|\
    internal/runner/runner_test.go)
      ;;
    *)
      fail "new or relocated Go test is outside the allowed edit surface: ${relative}"
      ;;
  esac
done < <(find "${workspace}" -xdev -type f -name '*_test.go' -print0)

declare -A protected_hashes=(
  [".dockerignore"]="f3f977655f1f084c9172e0b910866418d469e3d61c13eb9b0c508ab5164e4f00"
  [".gitignore"]="2c5e6dd3904895964b3ba38bf98e42027ec449b1b3c5ee194731c196e2f367f3"
  ["Dockerfile"]="cd233c030c70e595db1282ebe5f10d74b8908aae71c3c0c0debd769f34f74140"
  ["LICENSE"]="52f28a21801fdf1614167b3fdceac61a3bacc67544c553c8b63582d6b416bd5f"
  ["README.md"]="0733493db1e9790d77fd882b74d7d7ed49a4d40989baf56e9af9c0043acc2357"
  ["go.mod"]="50806758d0ee4a0f527562c57daf90f4a5e0bbe8a22e5469a372a5ef110e2c50"
  ["configs/dev.json"]="581c5c89e10f5e79b9ec733a60d640e61a76300cf1e2d623fd004084423bd014"
  ["internal/checkpoint/checkpoint_test.go"]="42b65ca6e651182a9fcf6fc431d99e125a95b23b922bc2669c93ff8781e4bc22"
  ["internal/config/config_test.go"]="7591b5886858ec71daefcb9e165c4595a45d5432663a7c4a4af998d5afad50d7"
  ["internal/format/format_test.go"]="3794aeba7ed4d5174714481dffbb2ca8dbfe5b873cd77fd79746fb8c288e3a65"
  ["internal/generator/generator_test.go"]="3116d1ff90cb3a32c58ac98ad8c42b4108562491e1d53b6f855fcf39573268df"
  ["internal/manifest/manifest_test.go"]="f4ae766380bb0115ff567523416daedd6ac9329b1eeaf9b91193c2067091b7e9"
  ["internal/model/model_test.go"]="eeebfe0f293e82ba349f41559bce40d5fce256c0ec5b2cee3b692ef14872239c"
  ["internal/runner/runner_test.go"]="4416e237f321710d1512f893966bc9922c8ba7d726394d07b1d9e0a0e7320e4c"
)

for relative in "${!protected_hashes[@]}"; do
  path="${workspace}/${relative}"
  [[ -f "${path}" && ! -L "${path}" ]] || fail "protected file is missing or not regular: ${relative}"
  observed="$(sha256sum -- "${path}" | awk '{print $1}')"
  [[ "${observed}" == "${protected_hashes[${relative}]}" ]] || fail "protected file changed: ${relative}"
done

if find "${workspace}" -xdev \( -type l -o -type b -o -type c -o -type p -o -type s \) -print -quit | grep -q .; then
  fail "workspace contains a symlink or special file"
fi
if find "${workspace}" -xdev -type f -links +1 -print -quit | grep -q .; then
  fail "workspace contains a multiply-linked regular file"
fi
file_count="$(find "${workspace}" -xdev -type f | wc -l)"
byte_count="$(find "${workspace}" -xdev -type f -printf '%s\n' | awk '{sum += $1} END {print sum + 0}')"
(( file_count <= 800 )) || fail "workspace contains too many files: ${file_count}"
(( byte_count <= 30000000 )) || fail "workspace is unexpectedly large: ${byte_count} bytes"

scratch="$(mktemp -d "${verifier_root}/.traceweave-verifier.XXXXXX")"
chown 0:0 "${scratch}"
# Candidate builds run below a dropped UID and must traverse this parent, but
# cannot list it. Verifier sources remain separately protected at /tests.
chmod 0711 "${scratch}"
source_root="${scratch}/source"
binary_root="${scratch}/binaries"
runtime_root="${scratch}/runtime"
install -d -o 0 -g 0 -m 0755 "${source_root}"
install -d -o 65532 -g 65532 -m 0700 "${binary_root}" "${runtime_root}"

tar \
  --exclude='./.git' \
  --exclude='./bin' \
  --exclude='./.incident-work' \
  --exclude='./.reference-work' \
  --exclude='./demo' \
  -C "${workspace}" -cf - . | tar -C "${source_root}" -xf -
chown -R 0:0 "${source_root}"
find "${source_root}" -type d -exec chmod 0755 {} +
find "${source_root}" -type f -exec chmod 0644 {} +

install -d -o 65532 -g 65532 -m 0700 \
  "${runtime_root}/home" "${runtime_root}/tmp" "${runtime_root}/go-cache" "${runtime_root}/go-mod-cache"

# Hide verifier sources before running any command derived from the candidate
# tree, including module inspection and compilation.
chown -R 0:0 /tests
find /tests -type d -exec chmod 0700 {} +
find /tests -type f -exec chmod 0600 {} +

run_as_builder() {
  /usr/bin/setpriv \
    --reuid=65532 \
    --regid=65532 \
    --clear-groups \
    --no-new-privs \
    --bounding-set=-all \
    --inh-caps=-all \
    --ambient-caps=-all \
    /usr/bin/env -i \
      HOME="${runtime_root}/home" \
      USER=traceweave-builder \
      LOGNAME=traceweave-builder \
      PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
      TMPDIR="${runtime_root}/tmp" \
      GOPROXY=off \
      GOSUMDB=off \
      GOTOOLCHAIN=local \
      GOFLAGS=-mod=readonly \
      GOENV=off \
      GOCACHE="${runtime_root}/go-cache" \
      GOMODCACHE="${runtime_root}/go-mod-cache" \
      /usr/bin/timeout --signal=KILL 120s "$@"
}

printf '[verifier] offline dependency and build checks\n'
module_lines="$(cd "${source_root}" && run_as_builder /usr/local/go/bin/go list -m all | sed '1d' | wc -l)"
[[ "${module_lines}" == "0" ]] || fail "candidate introduced third-party Go modules"

for command in tracegen traceweave traceinspect; do
  package="./cmd/${command}"
  (cd "${source_root}" && run_as_builder /usr/local/go/bin/go build \
    -trimpath -buildvcs=false -o "${binary_root}/${command}" "${package}")
done
chown 0:0 "${binary_root}/tracegen" "${binary_root}/traceweave" "${binary_root}/traceinspect"
chmod 0555 "${binary_root}/tracegen" "${binary_root}/traceweave" "${binary_root}/traceinspect"

(cd "${source_root}" && run_as_builder /usr/local/go/bin/go test -buildvcs=false ./...)
(cd "${source_root}" && run_as_builder /usr/local/go/bin/go test -buildvcs=false -race ./...)

printf '[verifier] compile independent byte-level verifier\n'
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go build -trimpath -buildvcs=false -o "${scratch}/verifier" /tests/verifier.go
chown 0:0 "${scratch}/verifier"
chmod 0500 "${scratch}/verifier"

case_root="$(mktemp -d /tmp/traceweave-cases.XXXXXX)"
chown 0:0 "${case_root}"
chmod 0711 "${case_root}"
"${scratch}/verifier" \
  "${binary_root}/traceweave" \
  "${binary_root}/tracegen" \
  "${binary_root}/traceinspect" \
  "${case_root}"

printf '1\n' >"${reward_path}"
printf '[verifier] reward=1\n'
