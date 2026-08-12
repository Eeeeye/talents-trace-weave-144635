package merge

import (
	"context"
	"fmt"
	"strconv"

	"example.com/trace-weave/internal/model"
	"example.com/trace-weave/internal/source"
)

type Sink interface {
	Write(model.Record) error
}

type CheckpointFunc func(emitted uint64, last *model.OrderKey, offsets map[string]int64) error

type Summary struct {
	InputRecords       uint64           `json:"input_records"`
	EmittedRecords     uint64           `json:"emitted_records"`
	SuppressedRecords  uint64           `json:"suppressed_records"`
	OrderingViolations uint64           `json:"ordering_violations"`
	CompletedSources   int              `json:"completed_sources"`
	SourceOffsets      map[string]int64 `json:"-"`
}

func Run(ctx context.Context, messages <-chan source.Message, sourceCount int,
	initialOffsets map[uint32]int64, initialLast *model.OrderKey, sink Sink,
	checkpointEvery uint64, checkpoint CheckpointFunc) (Summary, error) {
	summary := Summary{SourceOffsets: offsetStrings(initialOffsets)}
	seen := make(map[model.Identity]struct{})
	previous := cloneKey(initialLast)
	queues := make(map[uint32][]source.Message, sourceCount)
	finished := make(map[uint32]bool, sourceCount)
	active := make(map[uint32]bool, sourceCount)
	for rank := range initialOffsets {
		active[rank] = true
	}
	for summary.CompletedSources < sourceCount || queuedCount(queues) != 0 {
		for !allReady(active, finished, queues) {
			message, ok := <-messages
			if !ok {
				return summary, fmt.Errorf("message stream ended after %d of %d sources", summary.CompletedSources, sourceCount)
			}
			if err := ctx.Err(); err != nil {
				return summary, err
			}
			if message.Err != nil {
				return summary, message.Err
			}
			if !active[message.Rank] {
				return summary, fmt.Errorf("message from unexpected rank %d", message.Rank)
			}
			if message.EOF {
				if finished[message.Rank] {
					return summary, fmt.Errorf("rank %d sent EOF twice", message.Rank)
				}
				finished[message.Rank] = true
				summary.CompletedSources++
				continue
			}
			queues[message.Rank] = append(queues[message.Rank], message)
		}
		if queuedCount(queues) == 0 {
			break
		}
		rank, message := minimumHead(queues)
		queues[rank] = queues[rank][1:]
		summary.InputRecords++
		summary.SourceOffsets[strconv.FormatUint(uint64(rank), 10)] = message.NextOffset
		identity := message.Record.Identity()
		if _, duplicate := seen[identity]; duplicate {
			summary.SuppressedRecords++
			acknowledge(message)
			continue
		}
		seen[identity] = struct{}{}
		key := message.Record.OrderKey()
		if previous != nil && model.CompareOrder(key, *previous) < 0 {
			summary.OrderingViolations++
			return summary, fmt.Errorf("source order is inconsistent with canonical merge: %+v after %+v", key, *previous)
		}
		if err := sink.Write(message.Record); err != nil {
			return summary, err
		}
		summary.EmittedRecords++
		previous = &key
		if checkpointEvery != 0 && summary.EmittedRecords%checkpointEvery == 0 {
			if err := checkpoint(summary.EmittedRecords, previous, cloneOffsets(summary.SourceOffsets)); err != nil {
				return summary, fmt.Errorf("publish checkpoint: %w", err)
			}
		}
		acknowledge(message)
	}
	if summary.CompletedSources != sourceCount {
		return summary, fmt.Errorf("only %d of %d sources reached EOF", summary.CompletedSources, sourceCount)
	}
	return summary, nil
}

func acknowledge(message source.Message) {
	if message.Ack != nil {
		close(message.Ack)
	}
}

func allReady(active, finished map[uint32]bool, queues map[uint32][]source.Message) bool {
	for rank := range active {
		if !finished[rank] {
			if len(queues[rank]) == 0 {
				return false
			}
		}
	}
	return true
}

func minimumHead(queues map[uint32][]source.Message) (uint32, source.Message) {
	var selectedRank uint32
	var selected source.Message
	first := true
	for rank, queue := range queues {
		if len(queue) == 0 {
			continue
		}
		message := queue[0]
		if first || model.CompareRecord(message.Record, selected.Record) < 0 {
			selectedRank, selected, first = rank, message, false
		}
	}
	return selectedRank, selected
}

func queuedCount(queues map[uint32][]source.Message) int {
	count := 0
	for _, queue := range queues {
		count += len(queue)
	}
	return count
}

func offsetStrings(offsets map[uint32]int64) map[string]int64 {
	result := make(map[string]int64, len(offsets))
	for rank, offset := range offsets {
		result[strconv.FormatUint(uint64(rank), 10)] = offset
	}
	return result
}

func cloneOffsets(offsets map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(offsets))
	for rank, offset := range offsets {
		result[rank] = offset
	}
	return result
}

func cloneKey(key *model.OrderKey) *model.OrderKey {
	if key == nil {
		return nil
	}
	copyKey := *key
	return &copyKey
}
