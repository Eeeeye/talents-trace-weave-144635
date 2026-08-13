#!/bin/bash
set -Eeuo pipefail

umask 077

workspace="${TRACE_WEAVE_WORKSPACE:-/workspace/trace-weave}"
logs_root="/logs"
verifier_root="/logs/verifier"
reward_path="${verifier_root}/reward.txt"
log_path="${verifier_root}/trace-weave-tests.log"
reward=0
scratch=""
reward_identity=""
log_identity=""
logs_root_identity=""
logs_root_owner=""
logs_root_mode=""
verifier_root_identity=""
verifier_root_owner=""
verifier_root_mode=""

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
  logs_root_ok=0
  verifier_root_ok=0
  if [[ -d "${logs_root}" && ! -L "${logs_root}" &&
        "$(stat -Lc '%d:%i:%h' "${logs_root}" 2>/dev/null)" == "${logs_root_identity}" ]]; then
    logs_root_ok=1
  fi
  if (( logs_root_ok == 1 )) && [[ -d "${verifier_root}" && ! -L "${verifier_root}" &&
        "$(stat -Lc '%d:%i:%h' "${verifier_root}" 2>/dev/null)" == "${verifier_root_identity}" ]]; then
    verifier_root_ok=1
  fi
  if (( verifier_root_ok == 1 )); then
    if [[ -f "${reward_path}" && ! -L "${reward_path}" &&
          "$(stat -Lc '%d:%i:%u:%g:%h' "${reward_path}" 2>/dev/null)" == "${reward_identity}" ]]; then
      chmod 0644 "${reward_path}" >/dev/null 2>&1 || true
      printf '%s\n' "${reward}" >"${reward_path}" || true
    fi
    if [[ -f "${log_path}" && ! -L "${log_path}" &&
          "$(stat -Lc '%d:%i:%u:%g:%h' "${log_path}" 2>/dev/null)" == "${log_identity}" ]]; then
      chmod 0644 "${log_path}" >/dev/null 2>&1 || true
    fi
    # The directory is sealed only while candidate children exist. Restore its
    # upload-time ownership and mode so the Harbor host can collect artifacts.
    chown "${verifier_root_owner}" "${verifier_root}" >/dev/null 2>&1 || true
    chmod "${verifier_root_mode}" "${verifier_root}" >/dev/null 2>&1 || true
  fi
  if (( logs_root_ok == 1 )); then
    chown "${logs_root_owner}" "${logs_root}" >/dev/null 2>&1 || true
    chmod "${logs_root_mode}" "${logs_root}" >/dev/null 2>&1 || true
  fi
  exit "${finish_status}"
}

fail() {
  printf 'trace-weave verifier integrity failure: %s\n' "$1" >&2
  exit 1
}

[[ -d "${logs_root}" && ! -L "${logs_root}" ]] || {
  printf 'trace-weave verifier integrity failure: /logs is missing or is not a real directory\n' >&2
  exit 1
}
if [[ -L "${verifier_root}" || ! -d "${verifier_root}" ]]; then
  printf 'trace-weave verifier integrity failure: verifier output root is not a real directory\n' >&2
  exit 1
fi
logs_root_identity="$(stat -Lc '%d:%i:%h' "${logs_root}")"
logs_root_owner="$(stat -Lc '%u:%g' "${logs_root}")"
logs_root_mode="$(stat -Lc '%a' "${logs_root}")"
verifier_root_identity="$(stat -Lc '%d:%i:%h' "${verifier_root}")"
verifier_root_owner="$(stat -Lc '%u:%g' "${verifier_root}")"
verifier_root_mode="$(stat -Lc '%a' "${verifier_root}")"
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

# The verifier shell protects trusted assets, snapshots the submitted tree,
# and compiles both programs. Before launching candidate children, the
# semantic verifier enters a Landlock domain when available. On workers that
# block Landlock it instead uses a private chroot, or (when chroot is also
# blocked) requires these trusted trees to have been removed before it
# permanently drops to UID/GID 1000. If no safe isolation path is available,
# grading fails.

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

# Platform workers can reuse an image built from an earlier reviewed revision
# even though /tests and /solution are mounted from the current task. Accept
# only Dockerfiles that actually appeared in this activity's reviewed history;
# arbitrary candidate edits remain outside the permitted edit surface.
declare -A allowed_dockerfile_hashes=(
  ["224ea3e3ca2a1976b205ceb652e7cb1fa3b3abf78802f00e8ee3e0849e9b79e4"]=1
  ["4845a1ff569a614b77f8969adc19e19f9874d290f14538a4d14e6d115d0aa5aa"]=1
  ["7df5dc93fc9db99af211fe01bd91c93fface0c71d55c1914c21d240d942e98f9"]=1
  ["92f85d3f11a38945f146c666baa4a24ae40d1d6df274f359e79551a084af7a1b"]=1
  ["be015b111b86cc7cf69b9eef70aaa2908bd230af5ed7a5b9615d145e32ef627b"]=1
  ["cd233c030c70e595db1282ebe5f10d74b8908aae71c3c0c0debd769f34f74140"]=1
  ["d227357e3705e20b16b423515c568e73cc600c895cf863a478d8582078692877"]=1
  ["dde1ea961cff55befc9354000f9ad84f62c57516216304365512dd3b975cf0a3"]=1
  ["df3cd96d39ebd3f770e1c4df7f3509d6b08bd9aca52222accc1bfe4b5ff0a099"]=1
  ["e602fa04ff36afe9b94c208898b3c87209876292a09abf7eb1ada779294e5228"]=1
  ["f2ac225fb589381d03492ae084d82ca102e93cdab9fc3825114c79c2f70ea790"]=1
  ["f3fccba61717942eeb043375dd5cf2062637e54aa0f6a4e2917ecac8cd202d50"]=1
)
dockerfile_path="${workspace}/Dockerfile"
[[ -f "${dockerfile_path}" && ! -L "${dockerfile_path}" ]] || fail "protected file is missing or not regular: Dockerfile"
dockerfile_hash="$(sha256sum -- "${dockerfile_path}" | awk '{print $1}')"
[[ -n "${allowed_dockerfile_hashes[${dockerfile_hash}]:-}" ]] || fail "protected file changed: Dockerfile"

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

# Prefer /var/tmp because the chroot-free portable fallback must remain able to
# traverse the scratch ancestry after it permanently drops to UID/GID 1000.
# The random scratch directory itself is later mode 0711, so candidate
# children can reach only the explicitly transferred cases tree, not list or
# alter verifier binaries and sources.
for scratch_parent in /var/tmp /var/lib/traceweave-verifier /workspace /tmp; do
  [[ -d "${scratch_parent}" && ! -L "${scratch_parent}" ]] || continue
  candidate_scratch="$(/usr/bin/mktemp -d "${scratch_parent}/.traceweave-verifier.XXXXXXXXXXXX" 2>/dev/null || true)"
  [[ -n "${candidate_scratch}" && -d "${candidate_scratch}" && ! -L "${candidate_scratch}" ]] || continue
  chmod 0700 "${candidate_scratch}" || {
    rmdir -- "${candidate_scratch}" >/dev/null 2>&1 || true
    continue
  }
  free_kib="$(df -Pk -- "${candidate_scratch}" 2>/dev/null | awk 'NR == 2 {print $4}')"
  if [[ ! "${free_kib}" =~ ^[0-9]+$ ]] || (( free_kib < 131072 )); then
    rmdir -- "${candidate_scratch}" >/dev/null 2>&1 || true
    continue
  fi
  exec_probe="${candidate_scratch}/.exec-probe"
  if ! install -m 0500 /bin/true "${exec_probe}" 2>/dev/null || ! "${exec_probe}"; then
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
chmod 0711 "${scratch}" "${binary_root}"

# Keep generated fixtures in the same root-controlled overlay scratch. They
# must not consume either the shared /tmp quota or the /logs artifact quota.
# The EXIT trap removes the complete scratch tree.
case_root="${scratch}/cases"
install -d -m 0700 "${case_root}"
# Prepare the only candidate-writable tree while the trusted shell still has
# ownership controls. This also lets the Landlock path run on cap-drop workers;
# the fallback independently re-verifies the ownership before accepting it.
if chown 1000:1000 "${case_root}" 2>/dev/null; then
  chmod 0700 "${case_root}" || fail "cannot seal candidate case root"
fi
# Seal verifier output with Unix permissions when the worker grants ownership
# controls. Capability-free workers can still use Landlock; the semantic
# verifier checks the effective isolation before it launches any candidate.
if chown 0:0 "${logs_root}" "${verifier_root}" "${reward_path}" "${log_path}" 2>/dev/null &&
   chmod 0700 "${logs_root}" "${verifier_root}" 2>/dev/null &&
   chmod 0600 "${reward_path}" "${log_path}" 2>/dev/null; then
  printf '[verifier] verifier output sealed with root ownership\n'
else
  printf '[verifier] ownership seal unavailable; requiring kernel sandbox\n'
fi

# Harbor uploads tests and the Oracle solution after the image starts. Most
# remote workers materialize those uploads as ordinary directories, so remove
# them after the digest-checked verifier and candidate binaries are compiled.
# A local Docker backend may expose either path as a mount; never recurse into
# a mount because it may refer to host data. In that case Landlock or chroot
# must hide it, and the verifier's post-drop probes fail closed otherwise.
seal_trusted_tree() {
  trusted_path="$1"
  [[ -e "${trusted_path}" || -L "${trusted_path}" ]] || return 0
  [[ -d "${trusted_path}" && ! -L "${trusted_path}" ]] || fail "trusted path is not a real directory: ${trusted_path}"
  if mountpoint -q -- "${trusted_path}"; then
    printf '[verifier] trusted mount retained for kernel sandbox: %s\n' "${trusted_path}"
    return 0
  fi
  rm -rf --one-file-system -- "${trusted_path}"
  [[ ! -e "${trusted_path}" && ! -L "${trusted_path}" ]] || fail "could not remove trusted path: ${trusted_path}"
  printf '[verifier] removed trusted upload before candidate execution: %s\n' "${trusted_path}"
}

seal_trusted_tree /solution
seal_trusted_tree /tests
printf '[verifier] independent byte-level integration checks\n'
/usr/bin/timeout --signal=KILL 360s \
  "${scratch}/verifier" \
  "${binary_root}/traceweave" \
  "${binary_root}/tracegen" \
  "${binary_root}/traceinspect" \
  "${case_root}"

[[ "$(stat -Lc '%d:%i:%u:%g:%h' "${reward_path}" 2>/dev/null)" == "${reward_identity}" ]] || fail "candidate replaced verifier reward path"
[[ "$(stat -Lc '%d:%i:%u:%g:%h' "${log_path}" 2>/dev/null)" == "${log_identity}" ]] || fail "candidate replaced verifier log path"
reward=1
printf '[verifier] reward=1\n'
