#!/bin/bash
set -Eeuo pipefail

umask 077

workspace="${TRACE_WEAVE_WORKSPACE:-/workspace/trace-weave}"
verifier_root="/logs/verifier"
reward_path="${verifier_root}/reward.txt"
log_path="${verifier_root}/trace-weave-tests.log"
reward=0
scratch=""
unit_group_pid=""

cleanup_unit_group() {
  if [[ -n "${unit_group_pid}" ]]; then
    kill -KILL -- "-${unit_group_pid}" >/dev/null 2>&1 || true
    wait "${unit_group_pid}" >/dev/null 2>&1 || true
    unit_group_pid=""
  fi
}

cleanup() {
  cleanup_unit_group
  if [[ -n "${scratch}" && "${scratch}" == */.traceweave-verifier.* && -d "${scratch}" && ! -L "${scratch}" ]]; then
    rm -rf -- "${scratch}"
  fi
}

finish() {
  status=$?
  trap - EXIT
  set +e
  cleanup
  printf '%s\n' "${reward}" >"${reward_path}"
  chmod 0644 "${reward_path}"
  exit "${status}"
}

fail() {
  printf 'trace-weave verifier integrity failure: %s\n' "$1" >&2
  exit 1
}

if [[ -L /logs || ( -e /logs && ! -d /logs ) ]]; then
  printf 'trace-weave verifier integrity failure: /logs is not a real directory\n' >&2
  exit 1
fi
mkdir -p /logs
if [[ -L "${verifier_root}" || ( -e "${verifier_root}" && ! -d "${verifier_root}" ) ]]; then
  printf 'trace-weave verifier integrity failure: verifier output root is not a real directory\n' >&2
  exit 1
fi
mkdir -p "${verifier_root}"
chown 0:0 /logs "${verifier_root}" >/dev/null 2>&1 || true
chmod 0755 /logs
chmod 0755 "${verifier_root}"
rm -f -- "${reward_path}" "${log_path}"
install -o 0 -g 0 -m 0644 /dev/null "${reward_path}"
install -o 0 -g 0 -m 0644 /dev/null "${log_path}"
printf '0\n' >"${reward_path}"
trap finish EXIT
exec > >(tee -a "${log_path}") 2>&1

# The candidate runs as UID 1000. Terminate any process it left behind before
# verifier assets are used, then ensure no such process survives this boundary.
pkill -KILL -u 1000 2>/dev/null || true
for _ in 1 2 3 4 5; do
  if ! pgrep -u 1000 >/dev/null 2>&1; then
    break
  fi
  /usr/bin/sleep 0.1
done
if pgrep -u 1000 >/dev/null 2>&1; then
  fail "candidate-owned process survived the agent/verifier boundary"
fi

[[ -d "${workspace}" && ! -L "${workspace}" ]] || fail "workspace is missing or is a symlink"
[[ -d /tests && ! -L /tests ]] || fail "verifier tests are missing or are a symlink"
[[ -f /tests/verifier.go && ! -L /tests/verifier.go ]] || fail "trusted verifier source is missing or is not regular"

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
  ["Dockerfile"]="7df5dc93fc9db99af211fe01bd91c93fface0c71d55c1914c21d240d942e98f9"
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

# Remote workers do not promise that /opt is writable, that /tmp is executable,
# or that /logs has enough quota for build artifacts. Probe several
# root-controlled parents and use the first one that is writable, executable,
# and has enough free space for a cold Go cache. mktemp creates the name
# atomically after all candidate-owned processes have been killed.
workspace_parent="$(dirname -- "${workspace}")"
for scratch_parent in /opt "${workspace_parent}" /var/tmp /tmp "${verifier_root}"; do
  [[ -d "${scratch_parent}" && ! -L "${scratch_parent}" ]] || continue
  [[ "$(stat -c '%u' "${scratch_parent}" 2>/dev/null || true)" == "0" ]] || continue
  scratch_parent_mode="$(stat -c '%a' "${scratch_parent}" 2>/dev/null || true)"
  [[ "${scratch_parent_mode}" =~ ^[0-7]+$ ]] || continue
  # A writable parent is safe only when outsiders cannot rename entries, or
  # when the sticky bit gives root-owned entries that protection (as on /tmp).
  if (( (8#${scratch_parent_mode} & 0022) != 0 && (8#${scratch_parent_mode} & 01000) == 0 )); then
    continue
  fi

  candidate_scratch=""
  if ! candidate_scratch="$(mktemp -d "${scratch_parent}/.traceweave-verifier.XXXXXXXXXXXX" 2>/dev/null)"; then
    continue
  fi
  if ! chown 0:0 "${candidate_scratch}" || ! chmod 0700 "${candidate_scratch}"; then
    rmdir -- "${candidate_scratch}" >/dev/null 2>&1 || true
    continue
  fi

  free_kib="$(df -Pk -- "${candidate_scratch}" 2>/dev/null | awk 'NR == 2 {print $4}')"
  if [[ ! "${free_kib}" =~ ^[0-9]+$ ]] || (( free_kib < 131072 )); then
    rmdir -- "${candidate_scratch}" >/dev/null 2>&1 || true
    continue
  fi

  exec_probe="${candidate_scratch}/.exec-probe"
  if ! install -o 0 -g 0 -m 0500 /bin/true "${exec_probe}" 2>/dev/null || ! "${exec_probe}"; then
    rm -f -- "${exec_probe}" >/dev/null 2>&1 || true
    rmdir -- "${candidate_scratch}" >/dev/null 2>&1 || true
    continue
  fi
  rm -f -- "${exec_probe}"
  scratch="${candidate_scratch}"
  break
done

[[ -n "${scratch}" ]] || fail "no writable executable scratch filesystem with at least 128 MiB free"
printf '[verifier] scratch parent: %s\n' "$(dirname -- "${scratch}")"
# Candidate builds and commands run below a dropped UID and must traverse this
# directory, but cannot list or modify it.
chmod 0711 "${scratch}"
source_root="${scratch}/source"
binary_root="${scratch}/binaries"
runtime_root="${scratch}/runtime"
trusted_root="${scratch}/trusted"
install -d -o 0 -g 0 -m 0755 "${source_root}"
install -d -o 65532 -g 65532 -m 0700 "${binary_root}"
install -d -o 0 -g 0 -m 0711 "${runtime_root}"
install -d -o 0 -g 0 -m 0700 \
  "${trusted_root}" "${runtime_root}/trusted-home" "${runtime_root}/trusted-tmp" \
  "${runtime_root}/go-cache" "${runtime_root}/go-mod-cache"

# Copy and compile the trusted verifier before touching candidate-derived build
# commands. The source mount may be read-only, so it is never chmod'ed or
# chown'ed as a prerequisite. A fresh cache removes any dependency on
# /root/.cache/go-build; compiling the verifier also warms the standard-library
# entries reused by the restricted candidate build.
install -o 0 -g 0 -m 0600 /tests/verifier.go "${trusted_root}/verifier.go"
(
  cd "${trusted_root}"
  /usr/bin/env -i \
    HOME="${runtime_root}/trusted-home" \
    PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    TMPDIR="${runtime_root}/trusted-tmp" \
    GOPROXY=off \
    GOSUMDB=off \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly \
    GOENV=off \
    CGO_ENABLED=0 \
    GOCACHE="${runtime_root}/go-cache" \
    GOMODCACHE="${runtime_root}/go-mod-cache" \
    /usr/bin/timeout --signal=KILL 180s \
      /usr/local/go/bin/go build -trimpath -buildvcs=false -o "${scratch}/verifier" "${trusted_root}/verifier.go"
)
chown 0:0 "${scratch}/verifier"
chmod 0500 "${scratch}/verifier"
rm -f -- "${trusted_root}/verifier.go"

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
  "${runtime_root}/home" "${runtime_root}/tmp"
chown -R 65532:65532 "${runtime_root}/go-cache"
chown -R 65532:65532 "${runtime_root}/go-mod-cache"

# The verifier is already compiled and runs before candidate unit tests. Hiding
# its original source is defense in depth only; some backends mount /tests
# read-only, so permission tightening must be best-effort.
chown -R 0:0 /tests >/dev/null 2>&1 || true
chmod -R u=rwX,go= /tests >/dev/null 2>&1 || true

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
      CGO_ENABLED=0 \
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
chown 0:0 "${binary_root}"
chmod 0511 "${binary_root}"

# Keep generated fixtures in the same root-controlled overlay scratch. They
# must not consume either the shared /tmp quota or the /logs artifact quota.
# The EXIT trap removes the complete scratch tree.
case_root="${scratch}/cases"
install -d -o 0 -g 0 -m 0711 "${case_root}"
printf '[verifier] independent byte-level integration checks\n'
"${scratch}/verifier" \
  "${binary_root}/traceweave" \
  "${binary_root}/tracegen" \
  "${binary_root}/traceinspect" \
  "${case_root}"

# Candidate unit tests are fixed protected files, but they are still executable
# candidate-tree code. Run them only after the independent checks and in their
# own process group so no descendant can survive the verifier boundary.
printf '[verifier] protected candidate unit tests\n'
unit_log="${scratch}/unit-tests.log"
set +e
(
  cd "${source_root}"
  exec /usr/bin/setsid /usr/bin/setpriv \
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
      CGO_ENABLED=0 \
      GOCACHE="${runtime_root}/go-cache" \
      GOMODCACHE="${runtime_root}/go-mod-cache" \
      /usr/bin/timeout --signal=KILL 120s \
        /usr/local/go/bin/go test -buildvcs=false -count=1 ./...
) >"${unit_log}" 2>&1 &
unit_group_pid=$!
wait "${unit_group_pid}"
unit_status=$?
set -e
cleanup_unit_group
cat "${unit_log}"
(( unit_status == 0 )) || exit "${unit_status}"

reward=1
printf '[verifier] reward=1\n'
