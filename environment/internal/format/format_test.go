package format

import (
	"bytes"
	"path/filepath"
	"testing"

	"example.com/trace-weave/internal/model"
)

func TestRecordRoundTrip(t *testing.T) {
	record := model.Record{Epoch: 7, Rank: 2, Sequence: 3, Timestamp: 99, Kind: 4, Flags: 1, Payload: []byte("payload")}
	encoded, err := EncodeRecord(record, 1024)
	if err != nil {
		t.Fatal(err)
	}
	decoder := NewDecoder(bytes.NewReader(encoded), record.Epoch, record.Rank, 1024, 40)
	decoded, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if model.CompareRecord(record, decoded) != 0 {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestSpoolRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rank.tws")
	records := []model.Record{
		{Epoch: 8, Rank: 0, Sequence: 1, Timestamp: 10, Payload: []byte{1}},
		{Epoch: 8, Rank: 0, Sequence: 2, Timestamp: 20, Payload: []byte{2}},
	}
	if err := WriteSpool(path, SpoolHeader{Rank: 0, WorldSize: 1, Epoch: 8, RecordCount: 2}, records, 1024); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenSpool(path, 0, 1, 8, 0, 0, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.File.Close()
	for index := range records {
		got, err := opened.Decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if model.CompareRecord(got, records[index]) != 0 {
			t.Fatalf("record %d differs", index)
		}
	}
}

func TestSegmentHeaderRoundTrip(t *testing.T) {
	header := SegmentHeader{WorldSize: 4, Epoch: 11, RecordCount: 9, MinTimestamp: 2, MaxTimestamp: 99}
	data, err := EncodeSegmentHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSegmentHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != header {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestRecordChecksumFailure(t *testing.T) {
	record := model.Record{Epoch: 1, Rank: 0, Sequence: 1, Payload: []byte("abc")}
	encoded, _ := EncodeRecord(record, 100)
	encoded[len(encoded)-1] ^= 0xff
	decoder := NewDecoder(bytes.NewReader(encoded), 1, 0, 100, 0)
	if _, err := decoder.Next(); err == nil {
		t.Fatal("expected checksum failure")
	}
}
