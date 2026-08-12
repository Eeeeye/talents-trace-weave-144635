package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"example.com/trace-weave/internal/checkpoint"
	"example.com/trace-weave/internal/config"
	traceformat "example.com/trace-weave/internal/format"
	"example.com/trace-weave/internal/manifest"
	"example.com/trace-weave/internal/merge"
	"example.com/trace-weave/internal/model"
	"example.com/trace-weave/internal/output"
	"example.com/trace-weave/internal/source"
)

type Options struct {
	Resume                bool
	CrashAfterCheckpoints int
}

type Result struct {
	ManifestPath    string        `json:"manifest_path"`
	OutputPath      string        `json:"output_path"`
	CheckpointPath  string        `json:"checkpoint_path"`
	WorldSize       uint32        `json:"world_size"`
	Epoch           uint64        `json:"epoch"`
	ExpectedRecords uint64        `json:"expected_records"`
	Summary         merge.Summary `json:"summary"`
	Resumed         bool          `json:"resumed"`
}

func Run(ctx context.Context, cfg config.Resolved, options Options) (Result, error) {
	if err := validateConfigAndManifestShapes(cfg); err != nil {
		return Result{}, err
	}
	manifestValue, err := manifest.Load(cfg.ManifestPath)
	if err != nil {
		return Result{}, err
	}
	if options.CrashAfterCheckpoints < 0 {
		return Result{}, errors.New("crash checkpoint count must not be negative")
	}
	if err := validateFreshArtifacts(cfg, options.Resume); err != nil {
		return Result{}, err
	}
	inputScans, err := scanInputs(manifestValue, cfg.MaxPayloadBytes)
	if err != nil {
		return Result{}, err
	}
	initialOffsets := make(map[uint32]int64, len(manifestValue.InputsResolved))
	for _, input := range manifestValue.InputsResolved {
		initialOffsets[input.Rank] = traceformat.SpoolHeaderSize
	}
	var (
		writer *output.Writer
		state  checkpoint.State
	)
	if options.Resume {
		if err := validateCheckpointShape(cfg.CheckpointPath); err != nil {
			return Result{}, err
		}
		state, err = checkpoint.Load(cfg.CheckpointPath)
		if err != nil {
			return Result{}, err
		}
		if state.Completed {
			return Result{}, errors.New("checkpoint is already complete")
		}
		if state.ManifestSHA256 != manifestValue.SHA256 {
			return Result{}, errors.New("checkpoint manifest hash does not match")
		}
		outputAbsolute, _ := filepath.Abs(cfg.OutputPath)
		if state.OutputPath != outputAbsolute {
			return Result{}, errors.New("checkpoint output path does not match config")
		}
		validatedOffsets, err := validateResumeState(cfg, manifestValue, state, inputScans)
		if err != nil {
			return Result{}, err
		}
		for rank, value := range validatedOffsets {
			initialOffsets[rank] = value
		}
		writer, err = output.Resume(cfg.OutputPath, manifestValue.WorldSize, manifestValue.Epoch,
			cfg.OutputBufferBytes, cfg.MaxPayloadBytes, state)
	} else {
		writer, err = output.Create(cfg.OutputPath, manifestValue.WorldSize, manifestValue.Epoch,
			cfg.OutputBufferBytes, cfg.MaxPayloadBytes)
	}
	if err != nil {
		return Result{}, err
	}
	defer writer.Close()
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	messages, _ := source.Start(runContext, manifestValue.InputsResolved,
		manifestValue.WorldSize, manifestValue.Epoch, initialOffsets,
		cfg.ReadChunkBytes, cfg.MaxPayloadBytes, cfg.ChannelCapacity)
	checkpointCount := 0
	publish := func(emitted uint64, last *model.OrderKey, offsets map[string]int64) error {
		checkpointCount++
		outputAbsolute, _ := filepath.Abs(cfg.OutputPath)
		state = checkpoint.State{
			FormatVersion: 1, ManifestSHA256: manifestValue.SHA256,
			OutputPath: outputAbsolute, OutputBytes: writer.LogicalBytes(),
			OutputRecords: writer.Records(), SourceOffsets: offsets,
			LastKey: writer.LastKey(), MinTimestamp: writer.MinTimestamp(),
			MaxTimestamp: writer.MaxTimestamp(), Completed: false,
		}
		if err := writer.FlushAndSync(); err != nil {
			return err
		}
		if err := checkpoint.SaveAtomic(cfg.CheckpointPath, state); err != nil {
			return err
		}
		if options.CrashAfterCheckpoints != 0 && checkpointCount == options.CrashAfterCheckpoints {
			os.Exit(86)
		}
		return nil
	}
	var initialLast *model.OrderKey
	if options.Resume {
		initialLast = state.LastKey
	}
	summary, err := merge.Run(runContext, messages, len(manifestValue.InputsResolved),
		initialOffsets, initialLast, writer, uint64(cfg.CheckpointEveryRecords), publish)
	if err != nil {
		return Result{}, err
	}
	if err := writer.Finalize(); err != nil {
		return Result{}, err
	}
	finalOffsets := summary.SourceOffsets
	outputAbsolute, _ := filepath.Abs(cfg.OutputPath)
	state = checkpoint.State{
		FormatVersion: 1, ManifestSHA256: manifestValue.SHA256,
		OutputPath: outputAbsolute, OutputBytes: writer.LogicalBytes(),
		OutputRecords: writer.Records(), SourceOffsets: finalOffsets,
		LastKey: writer.LastKey(), MinTimestamp: writer.MinTimestamp(),
		MaxTimestamp: writer.MaxTimestamp(), Completed: true,
	}
	if err := checkpoint.SaveAtomic(cfg.CheckpointPath, state); err != nil {
		return Result{}, err
	}
	return Result{
		ManifestPath: cfg.ManifestPath, OutputPath: cfg.OutputPath,
		CheckpointPath: cfg.CheckpointPath, WorldSize: manifestValue.WorldSize,
		Epoch: manifestValue.Epoch, ExpectedRecords: manifestValue.TotalRecords(),
		Summary: summary, Resumed: options.Resume,
	}, nil
}

func validateConfigAndManifestShapes(cfg config.Resolved) error {
	configData, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if _, err := requireObjectFields(configData, "config", []string{
		"manifest", "output", "checkpoint", "checkpoint_every_records", "read_chunk_bytes",
		"channel_capacity", "output_buffer_bytes", "max_payload_bytes",
	}, nil); err != nil {
		return err
	}
	manifestData, err := os.ReadFile(cfg.ManifestPath)
	if err != nil {
		return err
	}
	manifestObject, err := requireObjectFields(manifestData, "manifest", []string{
		"format_version", "job_id", "world_size", "epoch", "inputs",
	}, nil)
	if err != nil {
		return err
	}
	var inputs []json.RawMessage
	if err := json.Unmarshal(manifestObject["inputs"], &inputs); err != nil {
		return fmt.Errorf("manifest inputs: %w", err)
	}
	for index, input := range inputs {
		if _, err := requireObjectFields(input, fmt.Sprintf("manifest inputs[%d]", index), []string{
			"rank", "path", "record_count", "read_delay_ms",
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func validateCheckpointShape(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	object, err := requireObjectFields(data, "checkpoint", []string{
		"format_version", "manifest_sha256", "output_path", "output_bytes", "output_records",
		"source_offsets", "min_timestamp", "max_timestamp", "completed", "updated_at",
	}, []string{"last_key"})
	if err != nil {
		return err
	}
	if lastKey, ok := object["last_key"]; ok {
		if _, err := requireObjectFields(lastKey, "checkpoint last_key", []string{
			"timestamp", "rank", "sequence",
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func requireObjectFields(data []byte, label string, required, optional []string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("%s JSON: %w", label, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", label)
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, field := range required {
		allowed[field] = true
	}
	for _, field := range optional {
		allowed[field] = true
	}
	var problems []string
	for _, field := range required {
		if _, ok := object[field]; !ok {
			problems = append(problems, "missing "+field)
		}
	}
	for field := range object {
		if !allowed[field] {
			problems = append(problems, "unknown "+field)
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("%s fields are not exact: %s", label, strings.Join(problems, ", "))
	}
	return object, nil
}

func validateFreshArtifacts(cfg config.Resolved, resume bool) error {
	if resume {
		return nil
	}
	if _, err := os.Stat(cfg.OutputPath); err == nil {
		return fmt.Errorf("output %s already exists", cfg.OutputPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(cfg.CheckpointPath); err == nil {
		return fmt.Errorf("checkpoint %s already exists", cfg.CheckpointPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type scannedRecord struct {
	record model.Record
	end    int64
}

type inputScan struct {
	records    []scannedRecord
	boundaries map[int64]int
}

func scanInputs(resolved manifest.Resolved, maxPayload int) (map[uint32]inputScan, error) {
	result := make(map[uint32]inputScan, len(resolved.InputsResolved))
	for _, input := range resolved.InputsResolved {
		opened, err := traceformat.OpenSpool(input.AbsolutePath, input.Rank, resolved.WorldSize,
			resolved.Epoch, traceformat.SpoolHeaderSize, 0, maxPayload)
		if err != nil {
			return nil, fmt.Errorf("validate rank %d: %w", input.Rank, err)
		}
		scan := inputScan{boundaries: map[int64]int{traceformat.SpoolHeaderSize: 0}}
		seenSequences := make(map[uint64]struct{})
		var previousTimestamp uint64
		for {
			record, readErr := opened.Decoder.Next()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				_ = opened.File.Close()
				return nil, fmt.Errorf("validate rank %d: %w", input.Rank, readErr)
			}
			if len(scan.records) != 0 && record.Timestamp < previousTimestamp {
				_ = opened.File.Close()
				return nil, fmt.Errorf("rank %d timestamps decrease at record %d", input.Rank, len(scan.records))
			}
			if _, duplicate := seenSequences[record.Sequence]; duplicate {
				_ = opened.File.Close()
				return nil, fmt.Errorf("rank %d repeats local sequence %d", input.Rank, record.Sequence)
			}
			seenSequences[record.Sequence] = struct{}{}
			previousTimestamp = record.Timestamp
			end := opened.Decoder.Offset()
			scan.records = append(scan.records, scannedRecord{record: record, end: end})
			scan.boundaries[end] = len(scan.records)
		}
		closeErr := opened.File.Close()
		if closeErr != nil {
			return nil, closeErr
		}
		if uint64(len(scan.records)) != opened.Header.RecordCount ||
			opened.Header.RecordCount != input.RecordCount {
			return nil, fmt.Errorf("rank %d decoded/header/manifest record counts are %d/%d/%d",
				input.Rank, len(scan.records), opened.Header.RecordCount, input.RecordCount)
		}
		result[input.Rank] = scan
	}
	return result, nil
}

func validateResumeState(cfg config.Resolved, resolved manifest.Resolved, state checkpoint.State,
	inputs map[uint32]inputScan) (map[uint32]int64, error) {
	if state.UpdatedAt.IsZero() {
		return nil, errors.New("checkpoint updated_at is missing or zero")
	}
	if len(state.SourceOffsets) != len(resolved.InputsResolved) {
		return nil, fmt.Errorf("checkpoint has %d source offsets, expected %d",
			len(state.SourceOffsets), len(resolved.InputsResolved))
	}
	offsets := make(map[uint32]int64, len(state.SourceOffsets))
	expected := make(map[model.Identity]model.Record)
	for key, offset := range state.SourceOffsets {
		parsed, err := strconv.ParseUint(key, 10, 32)
		if err != nil || strconv.FormatUint(parsed, 10) != key {
			return nil, fmt.Errorf("checkpoint source rank key %q is not canonical base-10", key)
		}
		rank := uint32(parsed)
		scan, ok := inputs[rank]
		if !ok {
			return nil, fmt.Errorf("checkpoint source rank %d is outside the manifest", rank)
		}
		count, ok := scan.boundaries[offset]
		if !ok {
			return nil, fmt.Errorf("checkpoint rank %d offset %d is not a record boundary", rank, offset)
		}
		offsets[rank] = offset
		for _, scanned := range scan.records[:count] {
			expected[scanned.record.Identity()] = scanned.record
		}
	}
	if uint64(len(expected)) != state.OutputRecords {
		return nil, fmt.Errorf("checkpoint source prefixes contain %d records, but output_records is %d",
			len(expected), state.OutputRecords)
	}
	if err := validateOutputPrefix(cfg.OutputPath, resolved.WorldSize, resolved.Epoch,
		cfg.MaxPayloadBytes, state, expected); err != nil {
		return nil, err
	}
	return offsets, nil
}

func validateOutputPrefix(path string, world uint32, epoch uint64, maxPayload int,
	state checkpoint.State, expected map[model.Identity]model.Record) error {
	if state.OutputBytes < traceformat.SegmentHeaderSize {
		return fmt.Errorf("checkpoint output boundary %d is before the segment body", state.OutputBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header, err := traceformat.ReadSegmentHeader(file)
	if err != nil {
		return err
	}
	if header.WorldSize != world || header.Epoch != epoch {
		return errors.New("segment identity does not match manifest")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < state.OutputBytes {
		return fmt.Errorf("output size %d is shorter than durable checkpoint boundary %d",
			info.Size(), state.OutputBytes)
	}
	remaining := make(map[model.Identity]model.Record, len(expected))
	for identity, record := range expected {
		remaining[identity] = record
	}
	position := int64(traceformat.SegmentHeaderSize)
	var previous *model.OrderKey
	var minimumTimestamp, maximumTimestamp uint64
	var count uint64
	for position < state.OutputBytes {
		var prefix [12]byte
		if _, err := file.ReadAt(prefix[:], position); err != nil {
			return fmt.Errorf("read checkpointed record prefix at %d: %w", position, err)
		}
		rank := binary.BigEndian.Uint32(prefix[8:12])
		if rank >= world {
			return fmt.Errorf("checkpointed output rank %d is outside world size %d", rank, world)
		}
		if _, err := file.Seek(position, io.SeekStart); err != nil {
			return err
		}
		decoder := traceformat.NewDecoder(io.LimitReader(file, state.OutputBytes-position),
			epoch, rank, maxPayload, position)
		record, err := decoder.Next()
		if err != nil {
			return fmt.Errorf("decode checkpointed output at %d: %w", position, err)
		}
		position = decoder.Offset()
		want, ok := remaining[record.Identity()]
		if !ok || !equalRecord(record, want) {
			return fmt.Errorf("checkpointed output record %s is absent from the source prefixes", record.Identity())
		}
		delete(remaining, record.Identity())
		key := record.OrderKey()
		if previous != nil && model.CompareOrder(key, *previous) < 0 {
			return errors.New("checkpointed output prefix is not in canonical order")
		}
		previous = &key
		if count == 0 || record.Timestamp < minimumTimestamp {
			minimumTimestamp = record.Timestamp
		}
		if count == 0 || record.Timestamp > maximumTimestamp {
			maximumTimestamp = record.Timestamp
		}
		count++
	}
	if position != state.OutputBytes || count != state.OutputRecords || len(remaining) != 0 {
		return fmt.Errorf("checkpoint output prefix disagrees with byte, record, or source boundaries")
	}
	if count == 0 {
		if state.LastKey != nil || state.MinTimestamp != 0 || state.MaxTimestamp != 0 {
			return errors.New("empty checkpoint prefix has non-empty metadata")
		}
	} else if state.LastKey == nil || model.CompareOrder(*state.LastKey, *previous) != 0 ||
		state.MinTimestamp != minimumTimestamp || state.MaxTimestamp != maximumTimestamp {
		return errors.New("checkpoint last key or timestamp bounds disagree with the output prefix")
	}
	return nil
}

func equalRecord(left, right model.Record) bool {
	return left.Epoch == right.Epoch && left.Rank == right.Rank && left.Sequence == right.Sequence &&
		left.Timestamp == right.Timestamp && left.Kind == right.Kind && left.Flags == right.Flags &&
		bytes.Equal(left.Payload, right.Payload)
}
