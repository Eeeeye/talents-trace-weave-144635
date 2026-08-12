package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"example.com/trace-weave/internal/model"
)

const (
	segmentMagic      uint32 = 0x54574d31 // TWM1
	segmentVersion    uint16 = 1
	SegmentHeaderSize        = 56
)

type SegmentHeader struct {
	WorldSize    uint32 `json:"world_size"`
	Epoch        uint64 `json:"epoch"`
	RecordCount  uint64 `json:"record_count"`
	MinTimestamp uint64 `json:"min_timestamp"`
	MaxTimestamp uint64 `json:"max_timestamp"`
}

func EncodeSegmentHeader(header SegmentHeader) ([]byte, error) {
	if header.WorldSize == 0 || header.Epoch == 0 {
		return nil, errors.New("segment world size and epoch must be positive")
	}
	if header.RecordCount == 0 && (header.MinTimestamp != 0 || header.MaxTimestamp != 0) {
		return nil, errors.New("empty segment timestamps must be zero")
	}
	if header.RecordCount != 0 && header.MinTimestamp > header.MaxTimestamp {
		return nil, errors.New("segment minimum timestamp exceeds maximum")
	}
	data := make([]byte, SegmentHeaderSize)
	binary.BigEndian.PutUint32(data[0:4], segmentMagic)
	binary.BigEndian.PutUint16(data[4:6], segmentVersion)
	binary.BigEndian.PutUint16(data[6:8], SegmentHeaderSize)
	binary.BigEndian.PutUint32(data[8:12], header.WorldSize)
	binary.BigEndian.PutUint32(data[12:16], 0)
	binary.BigEndian.PutUint64(data[16:24], header.Epoch)
	binary.BigEndian.PutUint64(data[24:32], header.RecordCount)
	binary.BigEndian.PutUint64(data[32:40], header.MinTimestamp)
	binary.BigEndian.PutUint64(data[40:48], header.MaxTimestamp)
	binary.BigEndian.PutUint64(data[48:56], 0)
	return data, nil
}

func DecodeSegmentHeader(data []byte) (SegmentHeader, error) {
	if len(data) != SegmentHeaderSize {
		return SegmentHeader{}, fmt.Errorf("segment header length %d, expected %d", len(data), SegmentHeaderSize)
	}
	if got := binary.BigEndian.Uint32(data[0:4]); got != segmentMagic {
		return SegmentHeader{}, fmt.Errorf("invalid segment magic %08x", got)
	}
	if got := binary.BigEndian.Uint16(data[4:6]); got != segmentVersion {
		return SegmentHeader{}, fmt.Errorf("unsupported segment version %d", got)
	}
	if binary.BigEndian.Uint16(data[6:8]) != SegmentHeaderSize {
		return SegmentHeader{}, errors.New("invalid segment header size")
	}
	if binary.BigEndian.Uint32(data[12:16]) != 0 || binary.BigEndian.Uint64(data[48:56]) != 0 {
		return SegmentHeader{}, errors.New("segment reserved bytes are non-zero")
	}
	header := SegmentHeader{
		WorldSize:    binary.BigEndian.Uint32(data[8:12]),
		Epoch:        binary.BigEndian.Uint64(data[16:24]),
		RecordCount:  binary.BigEndian.Uint64(data[24:32]),
		MinTimestamp: binary.BigEndian.Uint64(data[32:40]),
		MaxTimestamp: binary.BigEndian.Uint64(data[40:48]),
	}
	if header.WorldSize == 0 || header.Epoch == 0 {
		return SegmentHeader{}, errors.New("segment header fields are outside bounds")
	}
	return header, nil
}

func ReadSegmentHeader(file *os.File) (SegmentHeader, error) {
	data := make([]byte, SegmentHeaderSize)
	if _, err := file.ReadAt(data, 0); err != nil {
		return SegmentHeader{}, fmt.Errorf("read segment header: %w", err)
	}
	return DecodeSegmentHeader(data)
}

type SegmentScan struct {
	Header  SegmentHeader
	Records []model.Record
	Bytes   int64
}

func ScanSegment(path string, maximum int) (SegmentScan, error) {
	file, err := os.Open(path)
	if err != nil {
		return SegmentScan{}, err
	}
	defer file.Close()
	header, err := ReadSegmentHeader(file)
	if err != nil {
		return SegmentScan{}, err
	}
	if _, err := file.Seek(SegmentHeaderSize, io.SeekStart); err != nil {
		return SegmentScan{}, err
	}
	decoder := NewDecoder(file, header.Epoch, 0, maximum, SegmentHeaderSize)
	var records []model.Record
	for index := uint64(0); index < header.RecordCount; index++ {
		// Segment records can belong to any rank. Update the expected rank from
		// the fixed record header before delegating to the common decoder.
		position, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return SegmentScan{}, err
		}
		prefix := make([]byte, 12)
		if _, err := io.ReadFull(file, prefix); err != nil {
			return SegmentScan{}, fmt.Errorf("read segment record %d prefix: %w", index, err)
		}
		rank := binary.BigEndian.Uint32(prefix[8:12])
		if _, err := file.Seek(position, io.SeekStart); err != nil {
			return SegmentScan{}, err
		}
		decoder = NewDecoder(file, header.Epoch, rank, maximum, position)
		record, err := decoder.Next()
		if err != nil {
			return SegmentScan{}, fmt.Errorf("decode segment record %d: %w", index, err)
		}
		records = append(records, record)
	}
	position, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return SegmentScan{}, err
	}
	var trailing [1]byte
	if n, err := file.Read(trailing[:]); err != io.EOF || n != 0 {
		return SegmentScan{}, fmt.Errorf("segment has trailing bytes after offset %d", position)
	}
	return SegmentScan{Header: header, Records: records, Bytes: position}, nil
}
