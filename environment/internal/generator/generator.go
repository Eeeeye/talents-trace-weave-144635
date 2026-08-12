package generator

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	traceformat "example.com/trace-weave/internal/format"
	"example.com/trace-weave/internal/manifest"
	"example.com/trace-weave/internal/model"
)

type Options struct {
	Root         string
	JobID        string
	Ranks        int
	Records      int
	Epoch        uint64
	PayloadBytes int
	SequenceMode string
	DelayedRank  int
	DelayMS      int
}

type Result struct {
	ManifestPath string `json:"manifest_path"`
	WorldSize    int    `json:"world_size"`
	Epoch        uint64 `json:"epoch"`
	TotalRecords int    `json:"total_records"`
}

func (o Options) Validate() error {
	if o.Root == "" {
		return errors.New("root is required")
	}
	if o.JobID == "" || len(o.JobID) > 128 {
		return errors.New("job ID length must be in [1,128]")
	}
	if o.Ranks < 1 || o.Ranks > 4096 {
		return errors.New("ranks must be in [1,4096]")
	}
	if o.Records < 0 || o.Records > 10_000_000 {
		return errors.New("records must be in [0,10000000]")
	}
	if o.Epoch == 0 {
		return errors.New("epoch must be positive")
	}
	if o.PayloadBytes < 0 || o.PayloadBytes > 1<<20 {
		return errors.New("payload bytes must be in [0,1048576]")
	}
	if o.SequenceMode != "rank-local" && o.SequenceMode != "global" {
		return errors.New("sequence mode must be rank-local or global")
	}
	if o.DelayedRank < -1 || o.DelayedRank >= o.Ranks {
		return errors.New("delayed rank is outside the world")
	}
	if o.DelayMS < 0 || o.DelayMS > 60_000 {
		return errors.New("delay must be in [0,60000] milliseconds")
	}
	return nil
}

func Generate(options Options) (Result, error) {
	if err := options.Validate(); err != nil {
		return Result{}, err
	}
	absoluteRoot, err := filepath.Abs(options.Root)
	if err != nil {
		return Result{}, err
	}
	if entries, err := os.ReadDir(absoluteRoot); err == nil && len(entries) != 0 {
		return Result{}, fmt.Errorf("root %s is not empty", absoluteRoot)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if err := os.MkdirAll(absoluteRoot, 0o700); err != nil {
		return Result{}, err
	}
	value := manifest.Manifest{
		FormatVersion: 1, JobID: options.JobID,
		WorldSize: uint32(options.Ranks), Epoch: options.Epoch,
		Inputs: make([]manifest.Input, 0, options.Ranks),
	}
	for rank := 0; rank < options.Ranks; rank++ {
		records := make([]model.Record, 0, options.Records)
		for index := 0; index < options.Records; index++ {
			sequence := uint64(index + 1)
			if options.SequenceMode == "global" {
				sequence = uint64(rank*options.Records + index + 1)
			}
			records = append(records, model.Record{
				Epoch: options.Epoch, Rank: uint32(rank), Sequence: sequence,
				Timestamp: 1_000_000 + uint64(index)*1_000 + uint64(rank)*10,
				Kind:      uint16(1 + index%5), Flags: uint16(index % 2),
				Payload: payload(options, rank, sequence),
			})
		}
		name := fmt.Sprintf("rank-%04d.tws", rank)
		path := filepath.Join(absoluteRoot, name)
		if err := traceformat.WriteSpool(path, traceformat.SpoolHeader{
			Rank: uint32(rank), WorldSize: uint32(options.Ranks),
			Epoch: options.Epoch, RecordCount: uint64(options.Records),
		}, records, max(1, options.PayloadBytes)); err != nil {
			return Result{}, err
		}
		delay := 0
		if rank == options.DelayedRank {
			delay = options.DelayMS
		}
		value.Inputs = append(value.Inputs, manifest.Input{
			Rank: uint32(rank), Path: name, RecordCount: uint64(options.Records),
			ReadDelayMS: delay,
		})
	}
	manifestPath := filepath.Join(absoluteRoot, "manifest.json")
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return Result{}, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		return Result{}, err
	}
	return Result{
		ManifestPath: manifestPath, WorldSize: options.Ranks,
		Epoch: options.Epoch, TotalRecords: options.Ranks * options.Records,
	}, nil
}

func payload(options Options, rank int, sequence uint64) []byte {
	if options.PayloadBytes == 0 {
		return nil
	}
	seed := make([]byte, 24+len(options.JobID))
	copy(seed, options.JobID)
	binary.BigEndian.PutUint64(seed[len(options.JobID):], options.Epoch)
	binary.BigEndian.PutUint64(seed[len(options.JobID)+8:], uint64(rank))
	binary.BigEndian.PutUint64(seed[len(options.JobID)+16:], sequence)
	digest := sha256.Sum256(seed)
	result := make([]byte, options.PayloadBytes)
	for index := range result {
		result[index] = digest[index%len(digest)] ^ byte(index/len(digest))
	}
	return result
}
