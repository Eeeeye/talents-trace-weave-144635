# Repair TraceWeave's deterministic merge and crash recovery

TraceWeave is an offline post-processor for profiler events produced by every
rank of an MPI job. Each rank writes a binary spool. `traceweave` reads the
spools concurrently, emits one globally ordered segment, and publishes a JSON
checkpoint so a preempted merge can resume.

The repository at `/workspace/trace-weave` is a buildable incident snapshot.
A simple one-rank run works, but legal fragmented reads, ordinary rank-local
sequence numbers, delayed sources, and a crash at a periodic checkpoint expose
data loss, ordering, or recovery failures. Repair the implementation so it
meets the complete contract below.

## Scope and required commands

You may edit regular files under `cmd/**`, `internal/**`, `scripts/**`, and the
top-level `Makefile`. Do not change `go.mod`, `LICENSE`, `README.md`,
`configs/**`, existing `*_test.go` files, or replace input/output verification
with hard-coded fixtures. Keep the module dependency-free and buildable with
Go 1.22 while module and checksum downloads are disabled.

These commands and flags must continue to work:

```text
make build
go test ./...
./bin/traceweave -config FILE
./bin/traceweave -config FILE -resume
./bin/traceweave -config FILE -crash-after-checkpoints N
./bin/traceinspect verify -segment FILE -manifest FILE [-max-payload-bytes N]
./bin/traceinspect segment -path FILE [-records N] [-max-payload-bytes N]
./bin/traceinspect spool -path FILE [-max-payload-bytes N]
./bin/traceinspect checkpoint -path FILE
```

`-crash-after-checkpoints N` is deliberate fault injection. `0` disables it.
For positive `N`, the process must exit with status 86 immediately after the
Nth periodic checkpoint has been durably published. Negative values are
invalid. All other failures must return a non-zero status and write a useful
diagnostic to stderr.

## Binary formats

All integers are unsigned big-endian. Every reserved field must be zero.

### Rank spool (`TWS1`)

The fixed 40-byte spool header is:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | ASCII magic `TWS1` |
| 4 | 2 | format version `1` |
| 6 | 2 | header size `40` |
| 8 | 4 | rank |
| 12 | 4 | world size |
| 16 | 8 | epoch |
| 24 | 8 | record count |
| 32 | 8 | reserved zero |

World size and epoch are positive, and `rank < world_size`. A valid spool's
rank, world size, epoch, and record count agree with its manifest entry. Its
records have nondecreasing timestamps and unique positive local sequence
numbers.

### Event frame (`EVT1`)

Both spool bodies and merged segment bodies use a 40-byte frame header followed
immediately by `payload_length` bytes:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | ASCII magic `EVT1` |
| 4 | 4 | payload length |
| 8 | 4 | rank |
| 12 | 2 | kind |
| 14 | 2 | flags |
| 16 | 8 | local sequence |
| 24 | 8 | timestamp |
| 32 | 4 | IEEE CRC-32 of the payload |
| 36 | 4 | reserved zero |

Sequence zero is invalid. Payload length must not exceed the configured
`max_payload_bytes`. Header or payload truncation, bad magic, non-zero reserved
bytes, wrong rank, an oversized payload, or a CRC mismatch is an error. A
reader is allowed to return any positive prefix of a requested read without
EOF; valid frames must decode correctly under arbitrary such fragmentation.
Clean EOF exists only before the first byte of the next frame header.

### Merged segment (`TWM1`)

The segment starts with this fixed 56-byte header, followed by event frames:

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 4 | ASCII magic `TWM1` |
| 4 | 2 | format version `1` |
| 6 | 2 | header size `56` |
| 8 | 4 | world size |
| 12 | 4 | reserved zero |
| 16 | 8 | epoch |
| 24 | 8 | final record count |
| 32 | 8 | minimum timestamp, or zero when empty |
| 40 | 8 | maximum timestamp, or zero when empty |
| 48 | 8 | reserved zero |

A completed segment contains no trailing bytes. It preserves every frame's
rank, sequence, timestamp, kind, flags, and payload exactly.

## Identity, deduplication, and canonical order

An event identity is the tuple `(epoch, rank, local_sequence)`. Sequence
numbers are rank-local: rank 0 sequence 7 and rank 1 sequence 7 are distinct
events and both must be emitted. Every valid input identity appears exactly
once in the completed segment.

The canonical global order key is `(timestamp, rank, local_sequence)`, compared
lexicographically as unsigned integers. Output must be in nondecreasing
canonical order regardless of goroutine scheduling, manifest input order,
per-source delay, channel capacity, read fragmentation, or output buffer size.
Empty ranks and an entirely empty job are valid.

## Strict JSON inputs

JSON is strict: unknown fields, trailing JSON values, wrong types, missing
required values, and values outside the bounds below are errors.

The merge configuration contains exactly:

```json
{
  "manifest": "input/manifest.json",
  "output": "merged.twseg",
  "checkpoint": "state.json",
  "checkpoint_every_records": 17,
  "read_chunk_bytes": 7,
  "channel_capacity": 64,
  "output_buffer_bytes": 65536,
  "max_payload_bytes": 1048576
}
```

`manifest`, `output`, and `checkpoint` are nonempty; output and checkpoint
differ. Bounds are: checkpoint interval `[1, 10000000]`, read chunk
`[0, 16777216]` (`0` means the normal file reader), channel capacity
`[0, 1000000]`, output buffer `[256, 67108864]`, and maximum payload
`[1, 67108864]`. Relative paths are resolved against the configuration file's
directory.

The manifest contains exactly:

```json
{
  "format_version": 1,
  "job_id": "job-name",
  "world_size": 2,
  "epoch": 42,
  "inputs": [
    {"rank": 0, "path": "rank-0000.tws", "record_count": 10, "read_delay_ms": 0},
    {"rank": 1, "path": "rank-0001.tws", "record_count": 10, "read_delay_ms": 3}
  ]
}
```

Format version is `1`; job ID length is `[1,128]`; world size is
`[1,1000000]`; epoch is positive; inputs length equals world size; every rank
is unique and below world size; every path is nonempty and unique; record count
is at most `1000000000`; and delay is `[0,60000]` milliseconds. Manifest input
paths are absolute or relative to the manifest file's directory.

## Checkpoint and resume contract

A checkpoint is strict JSON with these fields:

```json
{
  "format_version": 1,
  "manifest_sha256": "64 lowercase hexadecimal characters",
  "output_path": "/absolute/path/to/merged.twseg",
  "output_bytes": 1234,
  "output_records": 17,
  "source_offsets": {"0": 456, "1": 789},
  "last_key": {"timestamp": 1000, "rank": 1, "sequence": 9},
  "min_timestamp": 100,
  "max_timestamp": 1000,
  "completed": false,
  "updated_at": "an RFC 3339 timestamp"
}
```

`manifest_sha256` is SHA-256 of the manifest's exact bytes. `output_path` is
the configured output's absolute clean path. Rank keys in `source_offsets` are
base-10 strings. Each offset is the byte boundary immediately after the last
record from that rank which is included in the checkpointed output prefix, or
40 if none has been emitted. It must never describe decoder read-ahead. For
zero output records, `last_key` is absent; otherwise it equals the last emitted
canonical key. Counts, timestamps, output bytes, and offsets all describe the
same prefix.

Before a periodic checkpoint becomes visible, every output byte through
`output_bytes` must have been flushed and synchronized. The checkpoint itself
must be atomically replaced and durably published. Therefore, after fault
injection exits with 86, the file size is at least the claimed boundary and
the exact prefix through that boundary is complete and decodable.

On `-resume`:

- the checkpoint must be incomplete and must match the exact manifest hash and
  configured absolute output path;
- the output header's world size and epoch must match the manifest;
- if the output is shorter than `output_bytes`, fail without extending or
  otherwise modifying it;
- if the output is longer than `output_bytes`, discard the uncheckpointed
  suffix before continuing;
- continue after the recorded semantic source offsets and last key, without
  loss, duplication, corruption, or reordering.

Successful completion rewrites the segment header with final counts and
timestamp bounds, synchronizes it, and atomically publishes a checkpoint with
`completed: true`, final file size, final source EOF offsets, and the final
order key. Attempting to resume a completed checkpoint is an error.

Without `-resume`, an existing output or checkpoint must cause failure without
modifying the existing artifact. Any malformed input or runtime failure must
return non-zero and must never publish `completed: true` for partial output.

## Successful CLI result

On success, `traceweave` prints one JSON object to stdout with
`manifest_path`, `output_path`, `checkpoint_path`, `world_size`, `epoch`,
`expected_records`, `resumed`, and `summary`. `summary` contains
`input_records`, `emitted_records`, `suppressed_records`,
`ordering_violations`, and `completed_sources`. Paths are resolved paths;
summary counts describe work performed by this invocation (so a resumed run
reports the remaining suffix, while `expected_records` remains the whole job).
Diagnostics go to stderr.

Do not rely on the supplied `traceinspect` as the only correctness signal. The
grader independently generates spools, decodes segment bytes, hashes payloads,
checks input immutability, observes crash boundaries, and exercises restart
failure cases with verifier-owned values.
