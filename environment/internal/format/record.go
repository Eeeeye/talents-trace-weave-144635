package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"example.com/trace-weave/internal/model"
)

const (
	recordMagic       uint32 = 0x45565431 // EVT1
	RecordHeaderSize         = 40
	DefaultMaxPayload        = 1 << 20
)

func EncodeRecord(record model.Record, maximum int) ([]byte, error) {
	if maximum <= 0 {
		maximum = DefaultMaxPayload
	}
	if record.Sequence == 0 {
		return nil, errors.New("record sequence must be positive")
	}
	if len(record.Payload) > maximum {
		return nil, fmt.Errorf("record payload length %d exceeds %d", len(record.Payload), maximum)
	}
	frame := make([]byte, RecordHeaderSize+len(record.Payload))
	binary.BigEndian.PutUint32(frame[0:4], recordMagic)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(record.Payload)))
	binary.BigEndian.PutUint32(frame[8:12], record.Rank)
	binary.BigEndian.PutUint16(frame[12:14], record.Kind)
	binary.BigEndian.PutUint16(frame[14:16], record.Flags)
	binary.BigEndian.PutUint64(frame[16:24], record.Sequence)
	binary.BigEndian.PutUint64(frame[24:32], record.Timestamp)
	binary.BigEndian.PutUint32(frame[32:36], crc32.ChecksumIEEE(record.Payload))
	binary.BigEndian.PutUint32(frame[36:40], 0)
	copy(frame[RecordHeaderSize:], record.Payload)
	return frame, nil
}

type Decoder struct {
	reader       io.Reader
	epoch        uint64
	expectedRank uint32
	maxPayload   int
	offset       int64
}

func NewDecoder(reader io.Reader, epoch uint64, expectedRank uint32, maxPayload int, offset int64) *Decoder {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPayload
	}
	return &Decoder{
		reader: reader, epoch: epoch, expectedRank: expectedRank,
		maxPayload: maxPayload, offset: offset,
	}
}

func (d *Decoder) Offset() int64 {
	return d.offset
}

func (d *Decoder) Next() (model.Record, error) {
	header := make([]byte, RecordHeaderSize)
	n, err := d.reader.Read(header)
	if errors.Is(err, io.EOF) && n == 0 {
		return model.Record{}, io.EOF
	}
	if err != nil {
		return model.Record{}, fmt.Errorf("read record header at offset %d: %w", d.offset, err)
	}
	if n != RecordHeaderSize {
		return model.Record{}, fmt.Errorf("short record header at offset %d: got %d bytes, need %d", d.offset, n, RecordHeaderSize)
	}
	if got := binary.BigEndian.Uint32(header[0:4]); got != recordMagic {
		return model.Record{}, fmt.Errorf("record at offset %d has invalid magic %08x", d.offset, got)
	}
	length := binary.BigEndian.Uint32(header[4:8])
	if uint64(length) > uint64(d.maxPayload) {
		return model.Record{}, fmt.Errorf("record at offset %d payload length %d exceeds %d", d.offset, length, d.maxPayload)
	}
	rank := binary.BigEndian.Uint32(header[8:12])
	if rank != d.expectedRank {
		return model.Record{}, fmt.Errorf("record at offset %d rank %d, expected %d", d.offset, rank, d.expectedRank)
	}
	if reserved := binary.BigEndian.Uint32(header[36:40]); reserved != 0 {
		return model.Record{}, fmt.Errorf("record at offset %d has non-zero reserved field", d.offset)
	}
	payload := make([]byte, int(length))
	if len(payload) != 0 {
		n, err = d.reader.Read(payload)
		if err != nil {
			return model.Record{}, fmt.Errorf("read record payload at offset %d: %w", d.offset, err)
		}
		if n != len(payload) {
			return model.Record{}, fmt.Errorf("short record payload at offset %d: got %d bytes, need %d", d.offset, n, len(payload))
		}
	}
	wantCRC := binary.BigEndian.Uint32(header[32:36])
	if got := crc32.ChecksumIEEE(payload); got != wantCRC {
		return model.Record{}, fmt.Errorf("record at offset %d checksum %08x, want %08x", d.offset, got, wantCRC)
	}
	record := model.Record{
		Epoch: d.epoch, Rank: rank,
		Kind:      binary.BigEndian.Uint16(header[12:14]),
		Flags:     binary.BigEndian.Uint16(header[14:16]),
		Sequence:  binary.BigEndian.Uint64(header[16:24]),
		Timestamp: binary.BigEndian.Uint64(header[24:32]),
		Payload:   payload,
	}
	if record.Sequence == 0 {
		return model.Record{}, fmt.Errorf("record at offset %d has zero sequence", d.offset)
	}
	d.offset += int64(RecordHeaderSize) + int64(length)
	return record, nil
}

type ChunkReader struct {
	Reader io.Reader
	Limit  int
}

func (r *ChunkReader) Read(destination []byte) (int, error) {
	if r.Limit > 0 && len(destination) > r.Limit {
		destination = destination[:r.Limit]
	}
	return r.Reader.Read(destination)
}
