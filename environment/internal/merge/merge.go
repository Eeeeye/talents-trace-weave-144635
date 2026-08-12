package merge

import (
	"context"
	"fmt"

	"example.com/trace-weave/internal/model"
	"example.com/trace-weave/internal/source"
)

type Sink interface {
	Write(model.Record) error
}

type CheckpointFunc func(emitted uint64, last *model.OrderKey, offsets map[string]int64) error

type Summary struct {
	InputRecords       uint64 `json:"input_records"`
	EmittedRecords     uint64 `json:"emitted_records"`
	SuppressedRecords  uint64 `json:"suppressed_records"`
	OrderingViolations uint64 `json:"ordering_violations"`
	CompletedSources   int    `json:"completed_sources"`
}

func Run(ctx context.Context, messages <-chan source.Message, sourceCount int,
	progress *source.Progress, sink Sink, checkpointEvery uint64,
	checkpoint CheckpointFunc) (Summary, error) {
	var summary Summary
	seen := make(map[uint64]struct{})
	var previous *model.OrderKey
	for message := range messages {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if message.Err != nil {
			return summary, message.Err
		}
		if message.EOF {
			summary.CompletedSources++
			continue
		}
		summary.InputRecords++
		if _, duplicate := seen[message.Record.Sequence]; duplicate {
			summary.SuppressedRecords++
			continue
		}
		seen[message.Record.Sequence] = struct{}{}
		key := message.Record.OrderKey()
		if previous != nil && model.CompareOrder(key, *previous) < 0 {
			summary.OrderingViolations++
		}
		if err := sink.Write(message.Record); err != nil {
			return summary, err
		}
		summary.EmittedRecords++
		previous = &key
		if checkpointEvery != 0 && summary.EmittedRecords%checkpointEvery == 0 {
			if err := checkpoint(summary.EmittedRecords, previous, progress.Snapshot()); err != nil {
				return summary, fmt.Errorf("publish checkpoint: %w", err)
			}
		}
	}
	if summary.CompletedSources != sourceCount {
		return summary, fmt.Errorf("only %d of %d sources reached EOF", summary.CompletedSources, sourceCount)
	}
	return summary, nil
}
