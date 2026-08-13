#!/bin/bash
set -Eeuo pipefail

umask 077

workspace="${TRACE_WEAVE_WORKSPACE:-/workspace/trace-weave}"
verifier_root="/logs/verifier"
reward_path="${verifier_root}/reward.txt"
log_path="${verifier_root}/trace-weave-tests.log"
reward=0
scratch=""
reward_identity=""
log_identity=""

cleanup() {
  if [[ -n "${scratch}" && "${scratch}" == */.traceweave-verifier.* && -d "${scratch}" && ! -L "${scratch}" ]]; then
    rm -rf -- "${scratch}"
  fi
}

finish() {
  finish_status=$?
  trap - EXIT
  set +e
  cleanup
  if [[ -f "${reward_path}" && ! -L "${reward_path}" &&
        "$(stat -Lc '%d:%i:%u:%g:%h' "${reward_path}" 2>/dev/null)" == "${reward_identity}" ]]; then
    chmod 0644 "${reward_path}" >/dev/null 2>&1 || true
    printf '%s\n' "${reward}" >"${reward_path}" || true
  fi
  exit "${finish_status}"
}

fail() {
  printf 'trace-weave verifier integrity failure: %s\n' "$1" >&2
  exit 1
}

[[ -d /logs && ! -L /logs ]] || {
  printf 'trace-weave verifier integrity failure: /logs is missing or is not a real directory\n' >&2
  exit 1
}
if [[ -L "${verifier_root}" || ! -d "${verifier_root}" ]]; then
  printf 'trace-weave verifier integrity failure: verifier output root is not a real directory\n' >&2
  exit 1
fi
rm -f -- "${reward_path}" "${log_path}"
: >"${reward_path}"
: >"${log_path}"
chmod 0644 "${reward_path}" "${log_path}" >/dev/null 2>&1 || true
printf '0\n' >"${reward_path}"
reward_identity="$(stat -Lc '%d:%i:%u:%g:%h' "${reward_path}")"
log_identity="$(stat -Lc '%d:%i:%u:%g:%h' "${log_path}")"
trap finish EXIT
exec > >(tee -a "${log_path}") 2>&1

[[ -d "${workspace}" && ! -L "${workspace}" ]] || fail "workspace is missing or is a symlink"
[[ -d /tests && ! -L /tests ]] || fail "verifier tests are missing or are a symlink"
[[ -f /tests/verifier.go && ! -L /tests/verifier.go ]] || fail "trusted verifier source is missing or is not regular"
[[ -f /tests/verifier.sha256 && ! -L /tests/verifier.sha256 ]] || fail "trusted verifier digest is missing or is not regular"

# The root shell protects trusted assets, inspects the submitted tree, and
# compiles both programs. The independent semantic verifier is executed by the
# image-started UID 1001 runner, so no runtime ownership or identity-changing
# syscall is required from this capability-free verifier process.

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
  ["Dockerfile"]="224ea3e3ca2a1976b205ceb652e7cb1fa3b3abf78802f00e8ee3e0849e9b79e4"
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

scratch_parent="/var/lib/traceweave-verifier"
[[ -d "${scratch_parent}" && ! -L "${scratch_parent}" ]] || fail "image verifier scratch root is missing or unsafe"
[[ "$(stat -c '%u:%g:%a' "${scratch_parent}" 2>/dev/null || true)" == "0:0:711" ]] || fail "image verifier scratch root has unsafe ownership or mode"
scratch="$(/usr/bin/mktemp -d "${scratch_parent}/.traceweave-verifier.XXXXXXXXXXXX")"
[[ -n "${scratch}" && -d "${scratch}" && ! -L "${scratch}" ]] || fail "cannot create verifier scratch directory"
chmod 0700 "${scratch}"
free_kib="$(df -Pk -- "${scratch}" 2>/dev/null | awk 'NR == 2 {print $4}')"
[[ "${free_kib}" =~ ^[0-9]+$ ]] && (( free_kib >= 131072 )) || fail "verifier scratch has less than 128 MiB free"
exec_probe="${scratch}/.exec-probe"
install -m 0500 /bin/true "${exec_probe}"
"${exec_probe}" || fail "verifier scratch filesystem is mounted noexec"
rm -f -- "${exec_probe}"
printf '[verifier] scratch parent: %s\n' "$(dirname -- "${scratch}")"
source_root="${scratch}/source"
binary_root="${scratch}/binaries"
trusted_runtime_root="${scratch}/trusted-runtime"
trusted_root="${scratch}/trusted"
install -d -m 0755 "${source_root}"
install -d -m 0711 "${binary_root}"
install -d -m 0700 "${trusted_root}" "${trusted_runtime_root}"
install -d -m 0700 \
  "${trusted_runtime_root}/home" "${trusted_runtime_root}/tmp" \
  "${trusted_runtime_root}/go-cache" "${trusted_runtime_root}/go-mod-cache"

# The grading fleet may use either amd64 or arm64 workers. Verify the fixed
# Oracle source and compile it with the image's pinned native Go toolchain so
# the trusted executable always matches the worker architecture.
expected_verifier_sha="$(awk 'NR == 1 {print $1}' /tests/verifier.sha256)"
[[ "${expected_verifier_sha}" =~ ^[0-9a-f]{64}$ ]] || fail "trusted verifier digest has an invalid format"
observed_verifier_sha="$(sha256sum -- /tests/verifier.go | awk '{print $1}')"
[[ "${observed_verifier_sha}" == "${expected_verifier_sha}" ]] || fail "trusted verifier source digest mismatch"
install -m 0600 /tests/verifier.go "${trusted_root}/verifier.go"
(
  cd "${trusted_root}"
  /usr/bin/env -i \
    HOME="${trusted_runtime_root}/home" \
    PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TMPDIR="${trusted_runtime_root}/tmp" \
    GOPROXY=off \
    GOSUMDB=off \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly \
    GOENV=off \
    CGO_ENABLED=0 \
    GOCACHE="${trusted_runtime_root}/go-cache" \
    GOMODCACHE="${trusted_runtime_root}/go-mod-cache" \
    /usr/bin/timeout --signal=KILL 180s \
      /usr/local/go/bin/go build -trimpath -buildvcs=false \
        -o "${scratch}/verifier" "${trusted_root}/verifier.go"
)
chmod 0555 "${scratch}/verifier"
rm -f -- "${trusted_root}/verifier.go"

tar \
  --exclude='./.git' \
  --exclude='./bin' \
  --exclude='./.incident-work' \
  --exclude='./.reference-work' \
  --exclude='./demo' \
  -C "${workspace}" -cf - . | tar --no-same-owner --no-same-permissions -C "${source_root}" -xf -
find "${source_root}" -type d -exec chmod 2750 {} +
find "${source_root}" -type f -exec chmod 0640 {} +

printf '[verifier] offline dependency and build checks\n'
module_lines="$(cd "${source_root}" && /usr/local/go/bin/go list -m all | sed '1d' | wc -l)"
[[ "${module_lines}" == "0" ]] || fail "candidate introduced third-party Go modules"

(
  cd "${source_root}"
  /usr/bin/env -i \
    HOME="${trusted_runtime_root}/home" \
    PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TMPDIR="${trusted_runtime_root}/tmp" \
    GOPROXY=off \
    GOSUMDB=off \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly \
    GOENV=off \
    CGO_ENABLED=0 \
    GOCACHE="${trusted_runtime_root}/go-cache" \
    GOMODCACHE="${trusted_runtime_root}/go-mod-cache" \
    /usr/bin/timeout --signal=KILL 120s \
      /usr/local/go/bin/go build -trimpath -buildvcs=false -o "${binary_root}" ./cmd/...
)
for command in tracegen traceweave traceinspect; do
  [[ -f "${binary_root}/${command}" && ! -L "${binary_root}/${command}" ]] || fail "candidate build did not produce ${command}"
  chmod 0555 "${binary_root}/${command}"
done
chmod 0711 "${scratch}"
chmod 0711 "${binary_root}"

# Keep generated fixtures in the same root-controlled overlay scratch. They
# must not consume either the shared /tmp quota or the /logs artifact quota.
# The EXIT trap removes the complete scratch tree.
case_root="${scratch}/cases"
install -d -m 0707 "${case_root}"
if chmod 0700 /tests 2>/dev/null; then
  printf '[verifier] trusted test mount hidden from candidate uid\n'
else
  printf '[verifier] trusted test mount is read-only; fixed source digest verified\n'
fi
chmod 0444 "${reward_path}" "${log_path}" >/dev/null 2>&1 || true
printf '[verifier] independent byte-level integration checks\n'
/usr/bin/timeout --signal=KILL 360s \
  /usr/local/bin/traceweave-verifier-runner run -- \
    "${scratch}/verifier" \
    "${binary_root}/traceweave" \
    "${binary_root}/tracegen" \
    "${binary_root}/traceinspect" \
    "${case_root}"

[[ "$(stat -Lc '%d:%i:%u:%g:%h' "${reward_path}" 2>/dev/null)" == "${reward_identity}" ]] || fail "candidate replaced verifier reward path"
[[ "$(stat -Lc '%d:%i:%u:%g:%h' "${log_path}" 2>/dev/null)" == "${log_identity}" ]] || fail "candidate replaced verifier log path"
reward=1
chmod 0644 "${reward_path}" "${log_path}" >/dev/null 2>&1 || true
printf '[verifier] reward=1\n'
