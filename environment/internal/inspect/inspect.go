package inspect

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	traceformat "example.com/trace-weave/internal/format"
	"example.com/trace-weave/internal/manifest"
	"example.com/trace-weave/internal/model"
)

type Report struct {
	WorldSize       uint32 `json:"world_size"`
	Epoch           uint64 `json:"epoch"`
	ExpectedRecords uint64 `json:"expected_records"`
	SegmentRecords  uint64 `json:"segment_records"`
	UniqueRecords   uint64 `json:"unique_records"`
	Ordered         bool   `json:"ordered"`
	PayloadsMatch   bool   `json:"payloads_match"`
}

type expectedRecord struct {
	OrderKey model.OrderKey
	Digest   [32]byte
}

func Verify(segmentPath, manifestPath string, maximum int) (Report, error) {
	manifestValue, err := manifest.Load(manifestPath)
	if err != nil {
		return Report{}, err
	}
	segment, err := traceformat.ScanSegment(segmentPath, maximum)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		WorldSize: manifestValue.WorldSize, Epoch: manifestValue.Epoch,
		ExpectedRecords: manifestValue.TotalRecords(),
		SegmentRecords:  segment.Header.RecordCount,
		Ordered:         true, PayloadsMatch: true,
	}
	if segment.Header.WorldSize != manifestValue.WorldSize || segment.Header.Epoch != manifestValue.Epoch {
		return report, errors.New("segment identity does not match manifest")
	}
	expected := make(map[model.Identity]expectedRecord)
	for _, input := range manifestValue.InputsResolved {
		opened, err := traceformat.OpenSpool(input.AbsolutePath, input.Rank,
			manifestValue.WorldSize, manifestValue.Epoch, 0, 0, maximum)
		if err != nil {
			return report, err
		}
		count := uint64(0)
		for {
			record, err := opened.Decoder.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = opened.File.Close()
				return report, err
			}
			count++
			identity := record.Identity()
			if _, exists := expected[identity]; exists {
				_ = opened.File.Close()
				return report, fmt.Errorf("input identity %s appears more than once", identity)
			}
			expected[identity] = expectedRecord{OrderKey: record.OrderKey(), Digest: sha256.Sum256(record.Payload)}
		}
		_ = opened.File.Close()
		if count != input.RecordCount {
			return report, fmt.Errorf("rank %d decoded %d records, manifest declares %d", input.Rank, count, input.RecordCount)
		}
	}
	seen := make(map[model.Identity]bool, len(segment.Records))
	var previous *model.OrderKey
	for index, record := range segment.Records {
		identity := record.Identity()
		if seen[identity] {
			return report, fmt.Errorf("segment identity %s is duplicated", identity)
		}
		seen[identity] = true
		expectedValue, ok := expected[identity]
		if !ok {
			return report, fmt.Errorf("segment identity %s is not in manifest inputs", identity)
		}
		if sha256.Sum256(record.Payload) != expectedValue.Digest {
			report.PayloadsMatch = false
			return report, fmt.Errorf("segment payload for identity %s differs", identity)
		}
		key := record.OrderKey()
		if previous != nil && model.CompareOrder(key, *previous) < 0 {
			report.Ordered = false
			return report, fmt.Errorf("segment ordering violation at record %d: %+v precedes %+v", index, key, *previous)
		}
		previous = &key
	}
	report.UniqueRecords = uint64(len(seen))
	if uint64(len(seen)) != uint64(len(expected)) {
		return report, fmt.Errorf("manifest expected_records=%d segment_records=%d missing=%d",
			len(expected), len(seen), len(expected)-len(seen))
	}
	return report, nil
}
