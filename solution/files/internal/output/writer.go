package output

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"example.com/trace-weave/internal/checkpoint"
	traceformat "example.com/trace-weave/internal/format"
	"example.com/trace-weave/internal/model"
)

type Writer struct {
	file         *os.File
	buffer       *bufio.Writer
	path         string
	world        uint32
	epoch        uint64
	maxPayload   int
	logicalBytes int64
	records      uint64
	minTimestamp uint64
	maxTimestamp uint64
	lastKey      *model.OrderKey
	closed       bool
}

func Create(path string, world uint32, epoch uint64, bufferBytes, maxPayload int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Writer, error) {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	header, err := traceformat.EncodeSegmentHeader(traceformat.SegmentHeader{WorldSize: world, Epoch: epoch})
	if err != nil {
		return fail(err)
	}
	if _, err := file.Write(header); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	return &Writer{
		file: file, buffer: bufio.NewWriterSize(file, bufferBytes), path: path,
		world: world, epoch: epoch, maxPayload: maxPayload,
		logicalBytes: traceformat.SegmentHeaderSize,
	}, nil
}

func Resume(path string, world uint32, epoch uint64, bufferBytes, maxPayload int, state checkpoint.State) (*Writer, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Writer, error) {
		_ = file.Close()
		return nil, err
	}
	header, err := traceformat.ReadSegmentHeader(file)
	if err != nil {
		return fail(err)
	}
	if header.WorldSize != world || header.Epoch != epoch {
		return fail(errors.New("segment identity does not match manifest"))
	}
	if state.OutputBytes < traceformat.SegmentHeaderSize {
		return fail(fmt.Errorf("checkpoint output boundary %d is before segment body", state.OutputBytes))
	}
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if info.Size() < state.OutputBytes {
		return fail(fmt.Errorf("output size %d is shorter than durable checkpoint boundary %d",
			info.Size(), state.OutputBytes))
	}
	if err := file.Truncate(state.OutputBytes); err != nil {
		return fail(fmt.Errorf("truncate output to checkpoint: %w", err))
	}
	if _, err := file.Seek(state.OutputBytes, io.SeekStart); err != nil {
		return fail(err)
	}
	return &Writer{
		file: file, buffer: bufio.NewWriterSize(file, bufferBytes), path: path,
		world: world, epoch: epoch, maxPayload: maxPayload,
		logicalBytes: state.OutputBytes, records: state.OutputRecords,
		minTimestamp: state.MinTimestamp, maxTimestamp: state.MaxTimestamp,
		lastKey: cloneKey(state.LastKey),
	}, nil
}

func (w *Writer) Write(record model.Record) error {
	if w.closed {
		return errors.New("segment writer is closed")
	}
	if record.Epoch != w.epoch || record.Rank >= w.world {
		return errors.New("record identity is outside segment")
	}
	encoded, err := traceformat.EncodeRecord(record, w.maxPayload)
	if err != nil {
		return err
	}
	if _, err := w.buffer.Write(encoded); err != nil {
		return err
	}
	w.logicalBytes += int64(len(encoded))
	w.records++
	if w.records == 1 || record.Timestamp < w.minTimestamp {
		w.minTimestamp = record.Timestamp
	}
	if w.records == 1 || record.Timestamp > w.maxTimestamp {
		w.maxTimestamp = record.Timestamp
	}
	key := record.OrderKey()
	w.lastKey = &key
	return nil
}

func (w *Writer) FlushAndSync() error {
	if w.closed {
		return errors.New("segment writer is closed")
	}
	if err := w.buffer.Flush(); err != nil {
		return err
	}
	return w.file.Sync()
}

func (w *Writer) Finalize() error {
	if err := w.FlushAndSync(); err != nil {
		return err
	}
	header, err := traceformat.EncodeSegmentHeader(traceformat.SegmentHeader{
		WorldSize: w.world, Epoch: w.epoch, RecordCount: w.records,
		MinTimestamp: w.minTimestamp, MaxTimestamp: w.maxTimestamp,
	})
	if err != nil {
		return err
	}
	if _, err := w.file.WriteAt(header, 0); err != nil {
		return err
	}
	return w.file.Sync()
}

func (w *Writer) LogicalBytes() int64      { return w.logicalBytes }
func (w *Writer) Records() uint64          { return w.records }
func (w *Writer) MinTimestamp() uint64     { return w.minTimestamp }
func (w *Writer) MaxTimestamp() uint64     { return w.maxTimestamp }
func (w *Writer) LastKey() *model.OrderKey { return cloneKey(w.lastKey) }

func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

func cloneKey(key *model.OrderKey) *model.OrderKey {
	if key == nil {
		return nil
	}
	copyKey := *key
	return &copyKey
}
