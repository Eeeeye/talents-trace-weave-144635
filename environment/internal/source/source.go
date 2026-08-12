package source

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	traceformat "example.com/trace-weave/internal/format"
	"example.com/trace-weave/internal/manifest"
	"example.com/trace-weave/internal/model"
)

type Message struct {
	Rank       uint32
	Record     model.Record
	NextOffset int64
	EOF        bool
	Err        error
}

type Progress struct {
	mu      sync.Mutex
	offsets map[uint32]int64
}

func NewProgress(initial map[uint32]int64) *Progress {
	copyOffsets := make(map[uint32]int64, len(initial))
	for rank, offset := range initial {
		copyOffsets[rank] = offset
	}
	return &Progress{offsets: copyOffsets}
}

func (p *Progress) Update(rank uint32, offset int64) {
	p.mu.Lock()
	p.offsets[rank] = offset
	p.mu.Unlock()
}

func (p *Progress) Snapshot() map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(p.offsets))
	for rank, offset := range p.offsets {
		out[strconv.FormatUint(uint64(rank), 10)] = offset
	}
	return out
}

func Start(ctx context.Context, inputs []manifest.ResolvedInput, world uint32, epoch uint64,
	initial map[uint32]int64, chunkBytes, maxPayload, capacity int) (<-chan Message, *Progress) {
	messages := make(chan Message, capacity)
	progress := NewProgress(initial)
	var wait sync.WaitGroup
	for _, input := range inputs {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			readSource(ctx, messages, progress, input, world, epoch,
				initial[input.Rank], chunkBytes, maxPayload)
		}()
	}
	go func() {
		wait.Wait()
		close(messages)
	}()
	return messages, progress
}

func readSource(ctx context.Context, messages chan<- Message, progress *Progress,
	input manifest.ResolvedInput, world uint32, epoch uint64, offset int64,
	chunkBytes, maxPayload int) {
	opened, err := traceformat.OpenSpool(input.AbsolutePath, input.Rank, world, epoch,
		offset, chunkBytes, maxPayload)
	if err != nil {
		send(ctx, messages, Message{Rank: input.Rank, Err: fmt.Errorf("open rank %d: %w", input.Rank, err)})
		return
	}
	defer opened.File.Close()
	for {
		record, err := opened.Decoder.Next()
		if err == io.EOF {
			send(ctx, messages, Message{Rank: input.Rank, EOF: true, NextOffset: opened.Decoder.Offset()})
			return
		}
		if err != nil {
			send(ctx, messages, Message{Rank: input.Rank, Err: fmt.Errorf("decode rank %d: %w", input.Rank, err)})
			return
		}
		nextOffset := opened.Decoder.Offset()
		progress.Update(input.Rank, nextOffset)
		if input.ReadDelayMS > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(input.ReadDelayMS) * time.Millisecond):
			}
		}
		if !send(ctx, messages, Message{Rank: input.Rank, Record: record, NextOffset: nextOffset}) {
			return
		}
	}
}

func send(ctx context.Context, messages chan<- Message, message Message) bool {
	select {
	case <-ctx.Done():
		return false
	case messages <- message:
		return true
	}
}
