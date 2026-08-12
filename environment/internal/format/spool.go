package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"example.com/trace-weave/internal/model"
)

const (
	spoolMagic      uint32 = 0x54575331 // TWS1
	spoolVersion    uint16 = 1
	SpoolHeaderSize        = 40
)

type SpoolHeader struct {
	Rank        uint32 `json:"rank"`
	WorldSize   uint32 `json:"world_size"`
	Epoch       uint64 `json:"epoch"`
	RecordCount uint64 `json:"record_count"`
}

func encodeSpoolHeader(header SpoolHeader) ([]byte, error) {
	if header.WorldSize == 0 || header.Rank >= header.WorldSize {
		return nil, errors.New("invalid spool rank/world size")
	}
	if header.Epoch == 0 {
		return nil, errors.New("spool epoch must be positive")
	}
	data := make([]byte, SpoolHeaderSize)
	binary.BigEndian.PutUint32(data[0:4], spoolMagic)
	binary.BigEndian.PutUint16(data[4:6], spoolVersion)
	binary.BigEndian.PutUint16(data[6:8], SpoolHeaderSize)
	binary.BigEndian.PutUint32(data[8:12], header.Rank)
	binary.BigEndian.PutUint32(data[12:16], header.WorldSize)
	binary.BigEndian.PutUint64(data[16:24], header.Epoch)
	binary.BigEndian.PutUint64(data[24:32], header.RecordCount)
	binary.BigEndian.PutUint64(data[32:40], 0)
	return data, nil
}

func decodeSpoolHeader(data []byte) (SpoolHeader, error) {
	if len(data) != SpoolHeaderSize {
		return SpoolHeader{}, fmt.Errorf("spool header length %d, expected %d", len(data), SpoolHeaderSize)
	}
	if got := binary.BigEndian.Uint32(data[0:4]); got != spoolMagic {
		return SpoolHeader{}, fmt.Errorf("invalid spool magic %08x", got)
	}
	if got := binary.BigEndian.Uint16(data[4:6]); got != spoolVersion {
		return SpoolHeader{}, fmt.Errorf("unsupported spool version %d", got)
	}
	if got := binary.BigEndian.Uint16(data[6:8]); got != SpoolHeaderSize {
		return SpoolHeader{}, fmt.Errorf("spool header size %d, expected %d", got, SpoolHeaderSize)
	}
	if binary.BigEndian.Uint64(data[32:40]) != 0 {
		return SpoolHeader{}, errors.New("spool reserved bytes are non-zero")
	}
	header := SpoolHeader{
		Rank:        binary.BigEndian.Uint32(data[8:12]),
		WorldSize:   binary.BigEndian.Uint32(data[12:16]),
		Epoch:       binary.BigEndian.Uint64(data[16:24]),
		RecordCount: binary.BigEndian.Uint64(data[24:32]),
	}
	if header.WorldSize == 0 || header.Rank >= header.WorldSize || header.Epoch == 0 {
		return SpoolHeader{}, errors.New("spool header fields are outside bounds")
	}
	return header, nil
}

func ReadSpoolHeader(file *os.File) (SpoolHeader, error) {
	data := make([]byte, SpoolHeaderSize)
	if _, err := file.ReadAt(data, 0); err != nil {
		return SpoolHeader{}, fmt.Errorf("read spool header: %w", err)
	}
	return decodeSpoolHeader(data)
}

func WriteSpool(path string, header SpoolHeader, records []model.Record, maximum int) error {
	if uint64(len(records)) != header.RecordCount {
		return fmt.Errorf("header record count %d differs from %d records", header.RecordCount, len(records))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	encodedHeader, err := encodeSpoolHeader(header)
	if err != nil {
		return err
	}
	if err := writeAll(file, encodedHeader); err != nil {
		return err
	}
	var previous uint64
	for index, record := range records {
		if record.Rank != header.Rank || record.Epoch != header.Epoch {
			return fmt.Errorf("record %d identity does not match spool", index)
		}
		if index != 0 && record.Timestamp < previous {
			return fmt.Errorf("rank %d timestamps decrease at record %d", header.Rank, index)
		}
		encoded, err := EncodeRecord(record, maximum)
		if err != nil {
			return fmt.Errorf("encode record %d: %w", index, err)
		}
		if err := writeAll(file, encoded); err != nil {
			return err
		}
		previous = record.Timestamp
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}

type OpenSpoolResult struct {
	File    *os.File
	Header  SpoolHeader
	Decoder *Decoder
}

func OpenSpool(path string, rank, world uint32, epoch uint64, offset int64, chunkBytes, maxPayload int) (OpenSpoolResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return OpenSpoolResult{}, err
	}
	fail := func(err error) (OpenSpoolResult, error) {
		_ = file.Close()
		return OpenSpoolResult{}, err
	}
	header, err := ReadSpoolHeader(file)
	if err != nil {
		return fail(err)
	}
	if header.Rank != rank || header.WorldSize != world || header.Epoch != epoch {
		return fail(fmt.Errorf("spool identity rank/world/epoch %d/%d/%d, expected %d/%d/%d",
			header.Rank, header.WorldSize, header.Epoch, rank, world, epoch))
	}
	if offset == 0 {
		offset = SpoolHeaderSize
	}
	if offset < SpoolHeaderSize {
		return fail(fmt.Errorf("spool offset %d is before header", offset))
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return fail(err)
	}
	var reader io.Reader = file
	if chunkBytes > 0 {
		reader = &ChunkReader{Reader: file, Limit: chunkBytes}
	}
	return OpenSpoolResult{
		File: file, Header: header,
		Decoder: NewDecoder(reader, epoch, rank, maxPayload, offset),
	}, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
