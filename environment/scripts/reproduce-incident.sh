#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_root="${repo_root}/.incident-work"
symptoms=0

for binary in tracegen traceweave traceinspect; do
    if [[ ! -x "${repo_root}/bin/${binary}" ]]; then
        echo "reproduce: missing bin/${binary}; run make build" >&2
        exit 70
    fi
done

rm -rf -- "${work_root}"
mkdir -p "${work_root}"

write_config() {
    local label="$1" manifest="$2" output="$3" checkpoint="$4"
    local checkpoint_every="$5" read_chunk="$6" channel_capacity="$7" buffer_bytes="$8"
    local path="${work_root}/${label}.json"
    cat >"${path}" <<EOF
{
  "manifest": "${manifest}",
  "output": "${output}",
  "checkpoint": "${checkpoint}",
  "checkpoint_every_records": ${checkpoint_every},
  "read_chunk_bytes": ${read_chunk},
  "channel_capacity": ${channel_capacity},
  "output_buffer_bytes": ${buffer_bytes},
  "max_payload_bytes": 1048576
}
EOF
    printf '%s\n' "${path}"
}

json_integer() {
    local field="$1" path="$2"
    sed -n "s/.*\"${field}\":[[:space:]]*\(-\{0,1\}[0-9][0-9]*\).*/\1/p" "${path}" | head -1
}

record_symptom() {
    symptoms=$((symptoms + 1))
    printf 'OBSERVED %-24s %s\n' "$1" "$2"
}

echo "[1/4] valid spool fails under legal short reads"
case_one="${work_root}/case-one"
mkdir -p "${case_one}"
"${repo_root}/bin/tracegen" -root "${case_one}/input" -job short-read \
    -ranks 1 -records 12 -epoch 7001 -payload-bytes 80 -sequence-mode rank-local \
    >"${case_one}/generate.json"
config_one="$(write_config case-one "${case_one}/input/manifest.json" \
    "${case_one}/merged.twseg" "${case_one}/state.json" 5 7 8 4096)"
if "${repo_root}/bin/traceweave" -config "${config_one}" \
    >"${case_one}/merge.json" 2>"${case_one}/merge.err"; then
    echo "reproduce: fragmented valid spool unexpectedly merged" >&2
    exit 1
fi
if ! grep -q 'short record header' "${case_one}/merge.err"; then
    echo "reproduce: expected short-read diagnostic missing" >&2
    cat "${case_one}/merge.err" >&2
    exit 1
fi
record_symptom "short-read" "valid record rejected at 7-byte fragments"

echo "[2/4] rank-local identities collide across ranks"
case_two="${work_root}/case-two"
mkdir -p "${case_two}"
"${repo_root}/bin/tracegen" -root "${case_two}/input" -job local-sequences \
    -ranks 3 -records 24 -epoch 7002 -payload-bytes 32 -sequence-mode rank-local \
    >"${case_two}/generate.json"
config_two="$(write_config case-two "${case_two}/input/manifest.json" \
    "${case_two}/merged.twseg" "${case_two}/state.json" 100 0 96 8192)"
"${repo_root}/bin/traceweave" -config "${config_two}" \
    >"${case_two}/merge.json" 2>"${case_two}/merge.err"
"${repo_root}/bin/traceinspect" segment -path "${case_two}/merged.twseg" \
    >"${case_two}/segment.json"
case_two_count="$(json_integer record_count "${case_two}/segment.json")"
if [[ "${case_two_count}" != "24" ]]; then
    echo "reproduce: expected 24 surviving records, got ${case_two_count}" >&2
    exit 1
fi
if "${repo_root}/bin/traceinspect" verify -segment "${case_two}/merged.twseg" \
    -manifest "${case_two}/input/manifest.json" \
    >"${case_two}/verify.json" 2>"${case_two}/verify.err"; then
    echo "reproduce: incomplete multi-rank segment unexpectedly verified" >&2
    exit 1
fi
record_symptom "identity-scope" "expected=72 emitted=24"

echo "[3/4] reader scheduling becomes global event order"
case_three="${work_root}/case-three"
mkdir -p "${case_three}"
"${repo_root}/bin/tracegen" -root "${case_three}/input" -job delayed-rank \
    -ranks 2 -records 30 -epoch 7003 -payload-bytes 24 -sequence-mode global \
    -delay-rank 1 -delay-ms 3 >"${case_three}/generate.json"
config_three="$(write_config case-three "${case_three}/input/manifest.json" \
    "${case_three}/merged.twseg" "${case_three}/state.json" 100 0 128 8192)"
"${repo_root}/bin/traceweave" -config "${config_three}" \
    >"${case_three}/merge.json" 2>"${case_three}/merge.err"
if "${repo_root}/bin/traceinspect" verify -segment "${case_three}/merged.twseg" \
    -manifest "${case_three}/input/manifest.json" \
    >"${case_three}/verify.json" 2>"${case_three}/verify.err"; then
    echo "reproduce: delayed-rank segment unexpectedly remained ordered" >&2
    exit 1
fi
if ! grep -q 'ordering violation' "${case_three}/verify.err"; then
    echo "reproduce: delayed-rank failure was not an ordering violation" >&2
    cat "${case_three}/verify.err" >&2
    exit 1
fi
record_symptom "unsafe-watermark" "delayed rank produced decreasing canonical key"

echo "[4/4] checkpoint advances beyond durable output"
case_four="${work_root}/case-four"
mkdir -p "${case_four}"
"${repo_root}/bin/tracegen" -root "${case_four}/input" -job crash-resume \
    -ranks 1 -records 50 -epoch 7004 -payload-bytes 96 -sequence-mode global \
    >"${case_four}/generate.json"
config_four="$(write_config case-four "${case_four}/input/manifest.json" \
    "${case_four}/merged.twseg" "${case_four}/state.json" 10 0 128 65536)"
set +e
"${repo_root}/bin/traceweave" -config "${config_four}" -crash-after-checkpoints 1 \
    >"${case_four}/crash.json" 2>"${case_four}/crash.err"
crash_status=$?
set -e
if [[ "${crash_status}" -ne 86 ]]; then
    echo "reproduce: crash injection exit=${crash_status}, expected 86" >&2
    exit 1
fi
durable_size="$(stat -c '%s' "${case_four}/merged.twseg")"
"${repo_root}/bin/traceinspect" checkpoint -path "${case_four}/state.json" \
    >"${case_four}/checkpoint.json"
claimed_size="$(json_integer output_bytes "${case_four}/checkpoint.json")"
if [[ -z "${claimed_size}" || "${claimed_size}" -le "${durable_size}" ]]; then
    echo "reproduce: checkpoint did not advance beyond durable output: claimed=${claimed_size} durable=${durable_size}" >&2
    exit 1
fi
"${repo_root}/bin/traceweave" -config "${config_four}" -resume \
    >"${case_four}/resume.json" 2>"${case_four}/resume.err"
if "${repo_root}/bin/traceinspect" verify -segment "${case_four}/merged.twseg" \
    -manifest "${case_four}/input/manifest.json" \
    >"${case_four}/verify.json" 2>"${case_four}/verify.err"; then
    echo "reproduce: unsafe resumed segment unexpectedly verified" >&2
    exit 1
fi
record_symptom "unsafe-checkpoint" "checkpoint_bytes=${claimed_size} durable_bytes=${durable_size}"

if [[ "${symptoms}" -ne 4 ]]; then
    echo "reproduce: observed ${symptoms}/4 symptoms" >&2
    exit 1
fi

echo "incident reproduced: ${symptoms}/4 symptoms observed"
