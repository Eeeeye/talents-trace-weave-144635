package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

const (
	candidateUID     = 65532
	candidateGID     = 65532
	spoolHeaderSize  = 40
	recordHeaderSize = 40
	segmentHeadSize  = 56
)

type event struct {
	Epoch     uint64
	Rank      uint32
	Sequence  uint64
	Timestamp uint64
	Kind      uint16
	Flags     uint16
	Payload   []byte
	Start     int64
	End       int64
}

type manifestInput struct {
	Rank        uint32 `json:"rank"`
	Path        string `json:"path"`
	RecordCount uint64 `json:"record_count"`
	ReadDelayMS int    `json:"read_delay_ms"`
}

type manifestDoc struct {
	FormatVersion int             `json:"format_version"`
	JobID         string          `json:"job_id"`
	WorldSize     uint32          `json:"world_size"`
	Epoch         uint64          `json:"epoch"`
	Inputs        []manifestInput `json:"inputs"`
}

type configDoc struct {
	Manifest               string `json:"manifest"`
	Output                 string `json:"output"`
	Checkpoint             string `json:"checkpoint"`
	CheckpointEveryRecords int    `json:"checkpoint_every_records"`
	ReadChunkBytes         int    `json:"read_chunk_bytes"`
	ChannelCapacity        int    `json:"channel_capacity"`
	OutputBufferBytes      int    `json:"output_buffer_bytes"`
	MaxPayloadBytes        int    `json:"max_payload_bytes"`
}

type orderKey struct {
	Timestamp uint64 `json:"timestamp"`
	Rank      uint32 `json:"rank"`
	Sequence  uint64 `json:"sequence"`
}

type checkpointDoc struct {
	FormatVersion  int              `json:"format_version"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	OutputPath     string           `json:"output_path"`
	OutputBytes    int64            `json:"output_bytes"`
	OutputRecords  uint64           `json:"output_records"`
	SourceOffsets  map[string]int64 `json:"source_offsets"`
	LastKey        *orderKey        `json:"last_key,omitempty"`
	MinTimestamp   uint64           `json:"min_timestamp"`
	MaxTimestamp   uint64           `json:"max_timestamp"`
	Completed      bool             `json:"completed"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type mergeResult struct {
	ManifestPath    string `json:"manifest_path"`
	OutputPath      string `json:"output_path"`
	CheckpointPath  string `json:"checkpoint_path"`
	WorldSize       uint32 `json:"world_size"`
	Epoch           uint64 `json:"epoch"`
	ExpectedRecords uint64 `json:"expected_records"`
	Resumed         bool   `json:"resumed"`
	Summary         struct {
		InputRecords       uint64 `json:"input_records"`
		EmittedRecords     uint64 `json:"emitted_records"`
		SuppressedRecords  uint64 `json:"suppressed_records"`
		OrderingViolations uint64 `json:"ordering_violations"`
		CompletedSources   int    `json:"completed_sources"`
	} `json:"summary"`
}

type dataset struct {
	Root            string
	ManifestPath    string
	ConfigPath      string
	OutputPath      string
	CheckpointPath  string
	World           uint32
	Epoch           uint64
	Records         []event
	RecordsByRank   map[uint32][]event
	SpoolPaths      map[uint32]string
	SpoolSizes      map[uint32]int64
	InputSHA256     map[string]string
	ManifestSHA256  string
	CheckpointEvery int
}

type commandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: verifier TRACEWEAVE TRACEGEN TRACEINSPECT CASE_ROOT")
		os.Exit(2)
	}
	traceweave, tracegen, traceinspect, caseRoot := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	if err := os.MkdirAll(caseRoot, 0o711); err != nil {
		fatal(err)
	}

	tests := []struct {
		name string
		fn   func() error
	}{
		{"full fragmented multi-rank merge", func() error { return testFullMerge(traceweave, traceinspect, caseRoot) }},
		{"alternate scheduling and empty rank", func() error { return testAlternateMerge(traceweave, caseRoot) }},
		{"durable crash boundary and resume", func() error { return testCrashResume(traceweave, caseRoot) }},
		{"empty job", func() error { return testEmptyJob(traceweave, caseRoot) }},
		{"strict failures and artifact safety", func() error { return testFailureSafety(traceweave, caseRoot) }},
		{"companion CLI compatibility", func() error { return testCompanionCLIs(tracegen, traceinspect, caseRoot) }},
	}
	for _, test := range tests {
		fmt.Printf("[verifier] %s\n", test.name)
		if err := test.fn(); err != nil {
			fatal(fmt.Errorf("%s: %w", test.name, err))
		}
	}
	fmt.Println("[verifier] all independent checks passed")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "trace-weave verifier:", err)
	os.Exit(1)
}

func testFullMerge(traceweave, traceinspect, root string) error {
	counts := []int{19, 19, 0, 19, 19}
	records := makeRecords((uint64(1)<<63)+880301, counts, 11)
	ds, err := createDataset(filepath.Join(root, "full merge 空格"), "full-fragmented", records,
		map[uint32]int{0: 1, 1: 0, 2: 2, 3: 1, 4: 0}, configDoc{
			CheckpointEveryRecords: 7, ReadChunkBytes: 1, ChannelCapacity: 0,
			OutputBufferBytes: 257, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 30*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure("fresh merge", result)
	}
	if err := validateCompleted(ds, result.Stdout, false, true); err != nil {
		return err
	}
	if err := verifyInputsUnchanged(ds); err != nil {
		return err
	}

	inspect, err := runCandidate(traceinspect, ds.Root, 15*time.Second,
		"verify", "-segment", ds.OutputPath, "-manifest", ds.ManifestPath, "-max-payload-bytes", "4096")
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return commandFailure("traceinspect verify", inspect)
	}
	var report struct {
		ExpectedRecords uint64 `json:"expected_records"`
		SegmentRecords  uint64 `json:"segment_records"`
		UniqueRecords   uint64 `json:"unique_records"`
		Ordered         bool   `json:"ordered"`
		PayloadsMatch   bool   `json:"payloads_match"`
	}
	if err := decodeOneJSON(inspect.Stdout, &report, false); err != nil {
		return fmt.Errorf("traceinspect verify JSON: %w", err)
	}
	if report.ExpectedRecords != uint64(len(ds.Records)) || report.SegmentRecords != uint64(len(ds.Records)) ||
		report.UniqueRecords != uint64(len(ds.Records)) || !report.Ordered || !report.PayloadsMatch {
		return fmt.Errorf("traceinspect report disagrees with independent oracle: %+v", report)
	}
	return nil
}

func testAlternateMerge(traceweave, root string) error {
	counts := []int{31, 0, 23, 17, 29, 5}
	records := makeRecords((uint64(1)<<63)+991117, counts, 29)
	ds, err := createDataset(filepath.Join(root, "alternate"), "alternate-schedule", records,
		map[uint32]int{0: 0, 1: 4, 2: 1, 3: 2, 4: 0, 5: 3}, configDoc{
			CheckpointEveryRecords: 13, ReadChunkBytes: 7, ChannelCapacity: 4096,
			OutputBufferBytes: 65536, MaxPayloadBytes: 8192,
		})
	if err != nil {
		return err
	}
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 30*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure("alternate merge", result)
	}
	if err := validateCompleted(ds, result.Stdout, false, true); err != nil {
		return err
	}
	return verifyInputsUnchanged(ds)
}

func testCrashResume(traceweave, root string) error {
	counts := []int{37, 37, 37, 37}
	records := makeRecords((uint64(1)<<63)+771337, counts, 47)
	ds, err := createDataset(filepath.Join(root, "crash-resume"), "crash-resume", records,
		map[uint32]int{0: 0, 1: 2, 2: 1, 3: 3}, configDoc{
			CheckpointEveryRecords: 11, ReadChunkBytes: 3, ChannelCapacity: 512,
			OutputBufferBytes: 65536, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	crash, err := runCandidate(traceweave, ds.Root, 30*time.Second,
		"-config", ds.ConfigPath, "-crash-after-checkpoints", "2")
	if err != nil {
		return err
	}
	if crash.ExitCode != 86 {
		return commandFailure("fault injection (wanted exit 86)", crash)
	}
	state, crashOutput, crashState, err := validateCrashPrefix(ds, uint64(ds.CheckpointEvery*2))
	if err != nil {
		return err
	}
	if err := verifyInputsUnchanged(ds); err != nil {
		return err
	}

	shortSize := state.OutputBytes - 1
	if shortSize < segmentHeadSize {
		return errors.New("test fixture produced an unexpectedly short checkpoint")
	}
	if err := os.Truncate(ds.OutputPath, shortSize); err != nil {
		return err
	}
	shortBefore, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	shortRun, err := runCandidate(traceweave, ds.Root, 15*time.Second,
		"-config", ds.ConfigPath, "-resume")
	if err != nil {
		return err
	}
	if shortRun.ExitCode == 0 {
		return errors.New("resume accepted an output shorter than the durable checkpoint boundary")
	}
	shortAfter, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	stateAfterShort, err := os.ReadFile(ds.CheckpointPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(shortBefore, shortAfter) {
		return errors.New("failed short-output resume modified or extended the output")
	}
	if !bytes.Equal(crashState, stateAfterShort) {
		return errors.New("failed short-output resume modified the checkpoint")
	}

	if err := os.WriteFile(ds.OutputPath, crashOutput, 0o600); err != nil {
		return err
	}
	file, err := os.OpenFile(ds.OutputPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(bytes.Repeat([]byte{0xa7}, 73))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	resume, err := runCandidate(traceweave, ds.Root, 30*time.Second,
		"-config", ds.ConfigPath, "-resume")
	if err != nil {
		return err
	}
	if resume.ExitCode != 0 {
		return commandFailure("resume from durable prefix", resume)
	}
	if err := validateCompleted(ds, resume.Stdout, true, false); err != nil {
		return err
	}
	if err := verifyInputsUnchanged(ds); err != nil {
		return err
	}

	completedState, _, err := readCheckpoint(ds.CheckpointPath)
	if err != nil {
		return err
	}
	completedBytes, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	if int64(len(completedBytes)) != completedState.OutputBytes {
		return fmt.Errorf("completed output size %d differs from checkpoint %d", len(completedBytes), completedState.OutputBytes)
	}
	return nil
}

func testEmptyJob(traceweave, root string) error {
	records := makeRecords(90117, []int{0, 0, 0}, 83)
	ds, err := createDataset(filepath.Join(root, "empty"), "empty-job", records,
		map[uint32]int{0: 2, 1: 0, 2: 1}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 1, ChannelCapacity: 0,
			OutputBufferBytes: 256, MaxPayloadBytes: 1,
		})
	if err != nil {
		return err
	}
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure("empty merge", result)
	}
	return validateCompleted(ds, result.Stdout, false, true)
}

func testFailureSafety(traceweave, root string) error {
	if err := testBadCRC(traceweave, root); err != nil {
		return err
	}
	if err := testTruncatedFrame(traceweave, root); err != nil {
		return err
	}
	if err := testStrictConfig(traceweave, root); err != nil {
		return err
	}
	if err := testExistingArtifacts(traceweave, root); err != nil {
		return err
	}
	return nil
}

func testBadCRC(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "bad-crc"), "bad-crc",
		makeRecords(3001, []int{3}, 101), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 2, ChannelCapacity: 8,
			OutputBufferBytes: 4096, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	spool := ds.SpoolPaths[0]
	data, err := os.ReadFile(spool)
	if err != nil {
		return err
	}
	if len(data) <= spoolHeaderSize+recordHeaderSize {
		return errors.New("bad CRC fixture has no payload")
	}
	data[spoolHeaderSize+recordHeaderSize] ^= 0xff
	if err := os.WriteFile(spool, data, 0o600); err != nil {
		return err
	}
	if err := refreshInputDigests(ds); err != nil {
		return err
	}
	return expectInputFailure(traceweave, ds, "CRC-corrupted input")
}

func testTruncatedFrame(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "truncated"), "truncated",
		makeRecords(3002, []int{4}, 103), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 2, ReadChunkBytes: 5, ChannelCapacity: 8,
			OutputBufferBytes: 4096, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	spool := ds.SpoolPaths[0]
	info, err := os.Stat(spool)
	if err != nil {
		return err
	}
	if err := os.Truncate(spool, info.Size()-1); err != nil {
		return err
	}
	if err := refreshInputDigests(ds); err != nil {
		return err
	}
	return expectInputFailure(traceweave, ds, "truncated input")
}

func testStrictConfig(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "strict-json"), "strict-json",
		makeRecords(3003, []int{2}, 107), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 0, ChannelCapacity: 1,
			OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	data, err := os.ReadFile(ds.ConfigPath)
	if err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[len(trimmed)-1] != '}' {
		return errors.New("unexpected generated config")
	}
	trimmed = append(trimmed[:len(trimmed)-1], []byte(",\n  \"verifier_unknown\": true\n}\n")...)
	if err := os.WriteFile(ds.ConfigPath, trimmed, 0o600); err != nil {
		return err
	}
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return errors.New("configuration with an unknown field was accepted")
	}
	if _, err := os.Stat(ds.OutputPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("strict config failure created output: %v", err)
	}
	if _, err := os.Stat(ds.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("strict config failure created checkpoint: %v", err)
	}
	return verifyInputsUnchanged(ds)
}

func testExistingArtifacts(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "existing-output"), "existing-output",
		makeRecords(3004, []int{2}, 109), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 0, ChannelCapacity: 1,
			OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ds.OutputPath), 0o700); err != nil {
		return err
	}
	sentinel := []byte("existing-output-must-survive\x00\xff")
	if err := os.WriteFile(ds.OutputPath, sentinel, 0o600); err != nil {
		return err
	}
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return errors.New("fresh run replaced an existing output")
	}
	after, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(sentinel, after) {
		return errors.New("fresh-run failure modified existing output bytes")
	}
	if _, err := os.Stat(ds.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("existing-output failure created checkpoint: %v", err)
	}

	ds2, err := createDataset(filepath.Join(root, "existing-checkpoint"), "existing-checkpoint",
		makeRecords(3005, []int{2}, 113), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 0, ChannelCapacity: 1,
			OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ds2.CheckpointPath), 0o700); err != nil {
		return err
	}
	checkpointSentinel := []byte("existing checkpoint is intentionally opaque\n")
	if err := os.WriteFile(ds2.CheckpointPath, checkpointSentinel, 0o600); err != nil {
		return err
	}
	if err := chownTree(ds2.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result2, err := runCandidate(traceweave, ds2.Root, 15*time.Second, "-config", ds2.ConfigPath)
	if err != nil {
		return err
	}
	if result2.ExitCode == 0 {
		return errors.New("fresh run accepted an existing checkpoint")
	}
	afterCheckpoint, err := os.ReadFile(ds2.CheckpointPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(checkpointSentinel, afterCheckpoint) {
		return errors.New("fresh-run failure modified existing checkpoint")
	}
	if _, err := os.Stat(ds2.OutputPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("existing-checkpoint failure created output: %v", err)
	}
	return nil
}

func expectInputFailure(traceweave string, ds *dataset, label string) error {
	if err := chownTree(ds.Root, candidateUID, candidateGID); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return fmt.Errorf("%s was accepted", label)
	}
	if data, err := os.ReadFile(ds.CheckpointPath); err == nil {
		var state checkpointDoc
		if decodeOneJSON(data, &state, true) == nil && state.Completed {
			return fmt.Errorf("%s published completed=true", label)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return verifyInputsUnchanged(ds)
}

func testCompanionCLIs(tracegen, traceinspect, root string) error {
	caseDir := filepath.Join(root, "companion-cli")
	if err := freshDir(caseDir); err != nil {
		return err
	}
	if err := chownTree(caseDir, candidateUID, candidateGID); err != nil {
		return err
	}
	generatedRoot := filepath.Join(caseDir, "generated data")
	gen, err := runCandidate(tracegen, caseDir, 15*time.Second,
		"-root", generatedRoot, "-job", "verifier-cli", "-ranks", "2", "-records", "5",
		"-epoch", "9223372036854775999", "-payload-bytes", "17", "-sequence-mode", "rank-local",
		"-delay-rank", "1", "-delay-ms", "1")
	if err != nil {
		return err
	}
	if gen.ExitCode != 0 {
		return commandFailure("tracegen", gen)
	}
	manifest := filepath.Join(generatedRoot, "manifest.json")
	spool := filepath.Join(generatedRoot, "rank-0001.tws")
	for _, path := range []string{manifest, spool} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("tracegen did not create regular file %s: %v", path, err)
		}
	}
	inspect, err := runCandidate(traceinspect, caseDir, 15*time.Second,
		"spool", "-path", spool, "-max-payload-bytes", "17")
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return commandFailure("traceinspect spool", inspect)
	}
	var report struct {
		DecodedRecords uint64 `json:"decoded_records"`
	}
	if err := decodeOneJSON(inspect.Stdout, &report, false); err != nil {
		return err
	}
	if report.DecodedRecords != 5 {
		return fmt.Errorf("traceinspect spool decoded %d records, want 5", report.DecodedRecords)
	}
	return nil
}

func createDataset(root, job string, byRank [][]event, delays map[uint32]int, options configDoc) (*dataset, error) {
	if len(byRank) == 0 {
		return nil, errors.New("dataset requires at least one rank")
	}
	if err := freshDir(root); err != nil {
		return nil, err
	}
	inputDir := filepath.Join(root, "input data β")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		return nil, err
	}
	epoch := uint64(90117)
	for _, records := range byRank {
		if len(records) != 0 {
			epoch = records[0].Epoch
			break
		}
	}
	ds := &dataset{
		Root: root, World: uint32(len(byRank)), Epoch: epoch,
		RecordsByRank: make(map[uint32][]event), SpoolPaths: make(map[uint32]string),
		SpoolSizes: make(map[uint32]int64), InputSHA256: make(map[string]string),
		CheckpointEvery: options.CheckpointEveryRecords,
	}
	inputs := make([]manifestInput, 0, len(byRank))
	for rank := len(byRank) - 1; rank >= 0; rank-- {
		records := append([]event(nil), byRank[rank]...)
		for index := range records {
			records[index].Epoch = epoch
			records[index].Rank = uint32(rank)
		}
		name := fmt.Sprintf("rank %04d λ.tws", rank)
		path := filepath.Join(inputDir, name)
		written, normalized, err := writeSpool(path, uint32(rank), uint32(len(byRank)), epoch, records)
		if err != nil {
			return nil, err
		}
		ds.RecordsByRank[uint32(rank)] = normalized
		ds.Records = append(ds.Records, normalized...)
		ds.SpoolPaths[uint32(rank)] = path
		ds.SpoolSizes[uint32(rank)] = int64(len(written))
		inputs = append(inputs, manifestInput{
			Rank: uint32(rank), Path: name, RecordCount: uint64(len(records)), ReadDelayMS: delays[uint32(rank)],
		})
	}
	sortEvents(ds.Records)
	manifest := manifestDoc{
		FormatVersion: 1, JobID: job, WorldSize: uint32(len(byRank)), Epoch: epoch, Inputs: inputs,
	}
	ds.ManifestPath = filepath.Join(inputDir, "manifest.json")
	manifestBytes, err := writeJSON(ds.ManifestPath, manifest)
	if err != nil {
		return nil, err
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	ds.ManifestSHA256 = hex.EncodeToString(manifestDigest[:])

	options.Manifest = filepath.ToSlash(filepath.Join("input data β", "manifest.json"))
	options.Output = filepath.ToSlash(filepath.Join("output data", "merged result.twseg"))
	options.Checkpoint = filepath.ToSlash(filepath.Join("output data", "merge state.json"))
	ds.ConfigPath = filepath.Join(root, "merge config.json")
	if _, err := writeJSON(ds.ConfigPath, options); err != nil {
		return nil, err
	}
	ds.OutputPath = filepath.Join(root, filepath.FromSlash(options.Output))
	ds.CheckpointPath = filepath.Join(root, filepath.FromSlash(options.Checkpoint))
	if err := refreshInputDigests(ds); err != nil {
		return nil, err
	}
	return ds, nil
}

func makeRecords(epoch uint64, counts []int, salt uint64) [][]event {
	result := make([][]event, len(counts))
	for rank, count := range counts {
		for index := 0; index < count; index++ {
			sequence := uint64(index + 1)
			if index == count-1 && count > 3 {
				sequence = ^uint64(0) - uint64(rank)
			}
			timestamp := (uint64(1) << 63) + salt*100000 + uint64(index/2)*100 + uint64(rank%3)*7
			payloadSizes := []int{1, 0, 39, 40, 41, 97, 513, 17, 255}
			size := payloadSizes[(index+rank*3)%len(payloadSizes)]
			seed := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%d", salt, rank, sequence, timestamp)))
			payload := make([]byte, size)
			for offset := range payload {
				payload[offset] = seed[offset%len(seed)] ^ byte(offset/len(seed))
			}
			result[rank] = append(result[rank], event{
				Epoch: epoch, Rank: uint32(rank), Sequence: sequence, Timestamp: timestamp,
				Kind: uint16(1 + (index+rank)%65534), Flags: uint16((index*17 + rank) % 65536), Payload: payload,
			})
		}
	}
	return result
}

func writeSpool(path string, rank, world uint32, epoch uint64, records []event) ([]byte, []event, error) {
	var buffer bytes.Buffer
	header := make([]byte, spoolHeaderSize)
	copy(header[0:4], []byte("TWS1"))
	binary.BigEndian.PutUint16(header[4:6], 1)
	binary.BigEndian.PutUint16(header[6:8], spoolHeaderSize)
	binary.BigEndian.PutUint32(header[8:12], rank)
	binary.BigEndian.PutUint32(header[12:16], world)
	binary.BigEndian.PutUint64(header[16:24], epoch)
	binary.BigEndian.PutUint64(header[24:32], uint64(len(records)))
	buffer.Write(header)
	normalized := make([]event, 0, len(records))
	for _, record := range records {
		record.Epoch, record.Rank = epoch, rank
		record.Start = int64(buffer.Len())
		frame := encodeFrame(record)
		buffer.Write(frame)
		record.End = int64(buffer.Len())
		normalized = append(normalized, record)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		return nil, nil, err
	}
	return append([]byte(nil), buffer.Bytes()...), normalized, nil
}

func encodeFrame(record event) []byte {
	frame := make([]byte, recordHeaderSize+len(record.Payload))
	copy(frame[0:4], []byte("EVT1"))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(record.Payload)))
	binary.BigEndian.PutUint32(frame[8:12], record.Rank)
	binary.BigEndian.PutUint16(frame[12:14], record.Kind)
	binary.BigEndian.PutUint16(frame[14:16], record.Flags)
	binary.BigEndian.PutUint64(frame[16:24], record.Sequence)
	binary.BigEndian.PutUint64(frame[24:32], record.Timestamp)
	binary.BigEndian.PutUint32(frame[32:36], crc32.ChecksumIEEE(record.Payload))
	copy(frame[recordHeaderSize:], record.Payload)
	return frame
}

func validateCompleted(ds *dataset, stdout []byte, resumed, checkFreshSummary bool) error {
	segment, header, err := decodeCompletedSegment(ds.OutputPath, 64<<20)
	if err != nil {
		return err
	}
	if header.WorldSize != ds.World || header.Epoch != ds.Epoch || header.RecordCount != uint64(len(ds.Records)) {
		return fmt.Errorf("segment header mismatch: %+v", header)
	}
	if len(segment) != len(ds.Records) {
		return fmt.Errorf("segment decoded %d records, want %d", len(segment), len(ds.Records))
	}
	for index := range ds.Records {
		if !sameEvent(segment[index], ds.Records[index]) {
			return fmt.Errorf("record %d mismatch: got=%s want=%s", index, describeEvent(segment[index]), describeEvent(ds.Records[index]))
		}
	}
	if len(ds.Records) == 0 {
		if header.MinTimestamp != 0 || header.MaxTimestamp != 0 {
			return fmt.Errorf("empty segment has timestamp bounds %d..%d", header.MinTimestamp, header.MaxTimestamp)
		}
	} else if header.MinTimestamp != ds.Records[0].Timestamp || header.MaxTimestamp != maximumTimestamp(ds.Records) {
		return fmt.Errorf("segment timestamp bounds %d..%d are wrong", header.MinTimestamp, header.MaxTimestamp)
	}

	state, _, err := readCheckpoint(ds.CheckpointPath)
	if err != nil {
		return err
	}
	if err := validateCheckpointCommon(ds, state); err != nil {
		return err
	}
	outputInfo, err := os.Stat(ds.OutputPath)
	if err != nil {
		return err
	}
	if !state.Completed || state.OutputBytes != outputInfo.Size() || state.OutputRecords != uint64(len(ds.Records)) {
		return fmt.Errorf("completed checkpoint count/size mismatch: %+v file=%d", state, outputInfo.Size())
	}
	for rank := uint32(0); rank < ds.World; rank++ {
		key := strconv.FormatUint(uint64(rank), 10)
		if state.SourceOffsets[key] != ds.SpoolSizes[rank] {
			return fmt.Errorf("completed rank %d offset=%d, want EOF %d", rank, state.SourceOffsets[key], ds.SpoolSizes[rank])
		}
	}
	if len(ds.Records) == 0 {
		if state.LastKey != nil || state.MinTimestamp != 0 || state.MaxTimestamp != 0 {
			return errors.New("empty completed checkpoint has last key or timestamp bounds")
		}
	} else {
		if !sameKey(state.LastKey, keyOf(ds.Records[len(ds.Records)-1])) {
			return fmt.Errorf("completed checkpoint last key mismatch: %+v", state.LastKey)
		}
		if state.MinTimestamp != ds.Records[0].Timestamp || state.MaxTimestamp != maximumTimestamp(ds.Records) {
			return errors.New("completed checkpoint timestamp bounds mismatch")
		}
	}

	var result mergeResult
	if err := decodeOneJSON(stdout, &result, false); err != nil {
		return fmt.Errorf("traceweave success JSON: %w", err)
	}
	if result.Resumed != resumed || result.WorldSize != ds.World || result.Epoch != ds.Epoch ||
		result.ExpectedRecords != uint64(len(ds.Records)) {
		return fmt.Errorf("traceweave result identity mismatch: %+v", result)
	}
	if cleanAbsolute(result.ManifestPath) != cleanAbsolute(ds.ManifestPath) ||
		cleanAbsolute(result.OutputPath) != cleanAbsolute(ds.OutputPath) ||
		cleanAbsolute(result.CheckpointPath) != cleanAbsolute(ds.CheckpointPath) {
		return fmt.Errorf("traceweave result paths mismatch: %+v", result)
	}
	if checkFreshSummary {
		want := uint64(len(ds.Records))
		if result.Summary.InputRecords != want || result.Summary.EmittedRecords != want ||
			result.Summary.SuppressedRecords != 0 || result.Summary.OrderingViolations != 0 ||
			result.Summary.CompletedSources != int(ds.World) {
			return fmt.Errorf("fresh merge summary mismatch: %+v", result.Summary)
		}
	}
	return nil
}

type segmentHeader struct {
	WorldSize    uint32
	Epoch        uint64
	RecordCount  uint64
	MinTimestamp uint64
	MaxTimestamp uint64
}

func decodeCompletedSegment(path string, maxPayload int) ([]event, segmentHeader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, segmentHeader{}, err
	}
	header, err := decodeSegmentHeader(data)
	if err != nil {
		return nil, segmentHeader{}, err
	}
	records, consumed, err := decodeFrames(data[segmentHeadSize:], header.Epoch, maxPayload)
	if err != nil {
		return nil, header, err
	}
	if consumed != len(data)-segmentHeadSize {
		return nil, header, errors.New("segment decoder left trailing bytes")
	}
	if uint64(len(records)) != header.RecordCount {
		return nil, header, fmt.Errorf("segment header count %d differs from decoded %d", header.RecordCount, len(records))
	}
	return records, header, nil
}

func decodeSegmentHeader(data []byte) (segmentHeader, error) {
	if len(data) < segmentHeadSize {
		return segmentHeader{}, fmt.Errorf("segment is only %d bytes", len(data))
	}
	if string(data[0:4]) != "TWM1" || binary.BigEndian.Uint16(data[4:6]) != 1 ||
		binary.BigEndian.Uint16(data[6:8]) != segmentHeadSize {
		return segmentHeader{}, errors.New("invalid segment magic, version, or header size")
	}
	if binary.BigEndian.Uint32(data[12:16]) != 0 || binary.BigEndian.Uint64(data[48:56]) != 0 {
		return segmentHeader{}, errors.New("non-zero segment reserved bytes")
	}
	header := segmentHeader{
		WorldSize: binary.BigEndian.Uint32(data[8:12]), Epoch: binary.BigEndian.Uint64(data[16:24]),
		RecordCount: binary.BigEndian.Uint64(data[24:32]), MinTimestamp: binary.BigEndian.Uint64(data[32:40]),
		MaxTimestamp: binary.BigEndian.Uint64(data[40:48]),
	}
	if header.WorldSize == 0 || header.Epoch == 0 {
		return segmentHeader{}, errors.New("zero segment world size or epoch")
	}
	return header, nil
}

func decodeFrames(data []byte, epoch uint64, maxPayload int) ([]event, int, error) {
	position := 0
	var records []event
	for position < len(data) {
		if len(data)-position < recordHeaderSize {
			return nil, position, fmt.Errorf("truncated frame header at %d", position)
		}
		header := data[position : position+recordHeaderSize]
		if string(header[0:4]) != "EVT1" {
			return nil, position, fmt.Errorf("invalid frame magic at %d", position)
		}
		length := int(binary.BigEndian.Uint32(header[4:8]))
		if length > maxPayload || length < 0 || len(data)-position-recordHeaderSize < length {
			return nil, position, fmt.Errorf("invalid/truncated payload length %d at %d", length, position)
		}
		if binary.BigEndian.Uint32(header[36:40]) != 0 {
			return nil, position, fmt.Errorf("non-zero frame reserved bytes at %d", position)
		}
		payload := append([]byte(nil), data[position+recordHeaderSize:position+recordHeaderSize+length]...)
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(header[32:36]) {
			return nil, position, fmt.Errorf("payload CRC mismatch at %d", position)
		}
		record := event{
			Epoch: epoch, Rank: binary.BigEndian.Uint32(header[8:12]),
			Kind: binary.BigEndian.Uint16(header[12:14]), Flags: binary.BigEndian.Uint16(header[14:16]),
			Sequence: binary.BigEndian.Uint64(header[16:24]), Timestamp: binary.BigEndian.Uint64(header[24:32]),
			Payload: payload, Start: int64(position), End: int64(position + recordHeaderSize + length),
		}
		if record.Sequence == 0 {
			return nil, position, fmt.Errorf("zero sequence at %d", position)
		}
		records = append(records, record)
		position += recordHeaderSize + length
	}
	return records, position, nil
}

func validateCrashPrefix(ds *dataset, expectedRecords uint64) (checkpointDoc, []byte, []byte, error) {
	state, stateBytes, err := readCheckpoint(ds.CheckpointPath)
	if err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if err := validateCheckpointCommon(ds, state); err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if state.Completed || state.OutputRecords != expectedRecords {
		return checkpointDoc{}, nil, nil, fmt.Errorf("crash checkpoint completed=%v records=%d, want incomplete/%d",
			state.Completed, state.OutputRecords, expectedRecords)
	}
	output, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if state.OutputBytes < segmentHeadSize || int64(len(output)) < state.OutputBytes {
		return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint boundary %d exceeds durable file %d", state.OutputBytes, len(output))
	}
	boundary := int(state.OutputBytes)
	prefixRecords, consumed, err := decodeFrames(output[segmentHeadSize:boundary], ds.Epoch, 64<<20)
	if err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if segmentHeadSize+consumed != boundary || uint64(len(prefixRecords)) != expectedRecords {
		return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint prefix decoded %d records/%d bytes", len(prefixRecords), consumed)
	}
	for index := range prefixRecords {
		if !sameEvent(prefixRecords[index], ds.Records[index]) {
			return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint prefix record %d mismatch", index)
		}
	}
	if !sameKey(state.LastKey, keyOf(ds.Records[expectedRecords-1])) {
		return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint last key mismatch: %+v", state.LastKey)
	}
	if state.MinTimestamp != ds.Records[0].Timestamp || state.MaxTimestamp != maximumTimestamp(ds.Records[:expectedRecords]) {
		return checkpointDoc{}, nil, nil, errors.New("checkpoint prefix timestamp bounds mismatch")
	}
	for rank := uint32(0); rank < ds.World; rank++ {
		want := int64(spoolHeaderSize)
		for _, record := range ds.Records[:expectedRecords] {
			if record.Rank == rank {
				want = record.End
			}
		}
		key := strconv.FormatUint(uint64(rank), 10)
		if state.SourceOffsets[key] != want {
			return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint rank %d offset=%d, want semantic boundary %d",
				rank, state.SourceOffsets[key], want)
		}
	}
	return state, output, stateBytes, nil
}

func validateCheckpointCommon(ds *dataset, state checkpointDoc) error {
	if state.FormatVersion != 1 || state.ManifestSHA256 != ds.ManifestSHA256 ||
		cleanAbsolute(state.OutputPath) != cleanAbsolute(ds.OutputPath) || state.UpdatedAt.IsZero() {
		return fmt.Errorf("checkpoint identity mismatch: version=%d manifest=%q path=%q updated=%v",
			state.FormatVersion, state.ManifestSHA256, state.OutputPath, state.UpdatedAt)
	}
	if len(state.SourceOffsets) != int(ds.World) {
		return fmt.Errorf("checkpoint has %d source offsets, want %d", len(state.SourceOffsets), ds.World)
	}
	for rank := uint32(0); rank < ds.World; rank++ {
		if _, ok := state.SourceOffsets[strconv.FormatUint(uint64(rank), 10)]; !ok {
			return fmt.Errorf("checkpoint missing rank %d offset", rank)
		}
	}
	return nil
}

func readCheckpoint(path string) (checkpointDoc, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return checkpointDoc{}, nil, err
	}
	var state checkpointDoc
	if err := decodeOneJSON(data, &state, true); err != nil {
		return checkpointDoc{}, data, fmt.Errorf("checkpoint JSON: %w", err)
	}
	return state, data, nil
}

func decodeOneJSON(data []byte, destination any, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("more than one JSON value")
		}
		return err
	}
	return nil
}

func runCandidate(binary, workdir string, timeout time.Duration, arguments ...string) (commandResult, error) {
	home := filepath.Join(workdir, ".candidate-home")
	temporary := filepath.Join(workdir, ".candidate-tmp")
	for _, path := range []string{home, temporary} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return commandResult{}, err
		}
		if err := os.Chown(path, candidateUID, candidateGID); err != nil {
			return commandResult{}, err
		}
	}
	setprivArgs := []string{
		"--reuid=" + strconv.Itoa(candidateUID), "--regid=" + strconv.Itoa(candidateGID),
		"--clear-groups", "--no-new-privs", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all",
		"/usr/bin/env", "-i", "HOME=" + home, "USER=traceweave-candidate", "LOGNAME=traceweave-candidate",
		"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR=" + temporary, "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly",
		binary,
	}
	setprivArgs = append(setprivArgs, arguments...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.Command("/usr/bin/setpriv", setprivArgs...)
	cmd.Dir = workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return commandResult{}, err
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		waitErr = <-done
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return commandResult{}, waitErr
		}
	}
	if timedOut {
		exitCode = -1
	}
	return commandResult{ExitCode: exitCode, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), TimedOut: timedOut}, nil
}

func commandFailure(label string, result commandResult) error {
	return fmt.Errorf("%s exit=%d timed_out=%v\nstdout:\n%s\nstderr:\n%s",
		label, result.ExitCode, result.TimedOut, bounded(result.Stdout), bounded(result.Stderr))
}

func bounded(data []byte) string {
	const maximum = 8000
	if len(data) <= maximum {
		return string(data)
	}
	return string(data[:maximum]) + "\n...[truncated]"
}

func refreshInputDigests(ds *dataset) error {
	ds.InputSHA256 = make(map[string]string)
	paths := []string{ds.ManifestPath}
	for rank := uint32(0); rank < ds.World; rank++ {
		paths = append(paths, ds.SpoolPaths[rank])
	}
	for _, path := range paths {
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		ds.InputSHA256[path] = digest
	}
	return nil
}

func verifyInputsUnchanged(ds *dataset) error {
	for path, want := range ds.InputSHA256 {
		got, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("input was modified: %s got=%s want=%s", path, got, want)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func writeJSON(path string, value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return data, nil
}

func freshDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.MkdirAll(path, 0o700)
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture unexpectedly contains symlink %s", path)
		}
		return os.Chown(path, uid, gid)
	})
}

func sortEvents(records []event) {
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.Timestamp != right.Timestamp {
			return left.Timestamp < right.Timestamp
		}
		if left.Rank != right.Rank {
			return left.Rank < right.Rank
		}
		return left.Sequence < right.Sequence
	})
}

func sameEvent(left, right event) bool {
	return left.Epoch == right.Epoch && left.Rank == right.Rank && left.Sequence == right.Sequence &&
		left.Timestamp == right.Timestamp && left.Kind == right.Kind && left.Flags == right.Flags &&
		bytes.Equal(left.Payload, right.Payload)
}

func keyOf(record event) orderKey {
	return orderKey{Timestamp: record.Timestamp, Rank: record.Rank, Sequence: record.Sequence}
}

func sameKey(left *orderKey, right orderKey) bool {
	return left != nil && *left == right
}

func maximumTimestamp(records []event) uint64 {
	var maximum uint64
	for index, record := range records {
		if index == 0 || record.Timestamp > maximum {
			maximum = record.Timestamp
		}
	}
	return maximum
}

func describeEvent(record event) string {
	return fmt.Sprintf("epoch=%d rank=%d sequence=%d timestamp=%d kind=%d flags=%d payload_sha=%x",
		record.Epoch, record.Rank, record.Sequence, record.Timestamp, record.Kind, record.Flags, sha256.Sum256(record.Payload))
}

func cleanAbsolute(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}
