# TraceWeave

TraceWeave merges binary performance events produced independently by every
rank of an MPI job. HPC profiler agents write one spool file per rank to local
or burst-buffer storage. After the job, `traceweave` concurrently decodes those
files, produces a single canonical timeline, and publishes a checkpoint so a
large merge can resume after preemption.

This repository contains a buildable incident snapshot of a trace-processing
failure. A one-rank, uninterrupted merge works. Fragmented reads, ordinary
multi-rank sequence numbers, delayed readers, and checkpoint recovery expose
several interacting correctness faults.

## Binaries

- `tracegen`: deterministic per-rank spool and manifest generator;
- `traceweave`: concurrent merge and resume CLI;
- `traceinspect`: strict segment, spool, manifest, ordering, and count checks.

The implementation uses only the Go standard library. Go 1.22 or newer on
Linux is sufficient and no module download is required.

## Build

```bash
./scripts/offline-check.sh
```

## Generate and merge a small trace

```bash
mkdir -p demo
./bin/tracegen -root demo/input -ranks 1 -records 20
cp configs/dev.json demo/merge.json
./bin/traceweave -config demo/merge.json
./bin/traceinspect verify \
  -segment demo/merged.twseg \
  -manifest demo/input/manifest.json
```

Configuration paths are resolved relative to the configuration file. The
generator writes manifest input paths relative to its manifest directory.

## Reproduce the incident

```bash
make reproduce
```

The script creates only `.incident-work` under the repository and checks four
terminal-visible symptoms:

1. a valid record split into small reads is rejected;
2. rank-local sequence numbers are treated as global identities and records
   disappear;
3. one delayed rank produces a globally out-of-order output segment;
4. a crash immediately after checkpoint publication cannot resume safely.

The script exits zero only after observing all four faults.

## Repair task

This checkout is the intentionally faulty starting point for the repair task in
`/workspace/trace-weave`. The complete required behavior, binary layouts, JSON
schemas, and recovery guarantees are provided in the task instruction. Grading
uses verifier-owned inputs and an independent byte-level decoder.
