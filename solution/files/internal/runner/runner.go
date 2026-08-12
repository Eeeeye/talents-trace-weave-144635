package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
	manifestValue, err := manifest.Load(cfg.ManifestPath)
	if err != nil {
		return Result{}, err
	}
	if options.CrashAfterCheckpoints < 0 {
		return Result{}, errors.New("crash checkpoint count must not be negative")
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
		for _, input := range manifestValue.InputsResolved {
			value, ok := state.SourceOffsets[strconv.FormatUint(uint64(input.Rank), 10)]
			if !ok {
				return Result{}, fmt.Errorf("checkpoint is missing rank %d offset", input.Rank)
			}
			initialOffsets[input.Rank] = value
		}
		writer, err = output.Resume(cfg.OutputPath, manifestValue.WorldSize, manifestValue.Epoch,
			cfg.OutputBufferBytes, cfg.MaxPayloadBytes, state)
	} else {
		if _, err := os.Stat(cfg.CheckpointPath); err == nil {
			return Result{}, fmt.Errorf("checkpoint %s already exists", cfg.CheckpointPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, err
		}
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
