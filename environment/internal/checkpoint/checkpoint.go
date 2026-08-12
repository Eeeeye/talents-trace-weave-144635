package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"example.com/trace-weave/internal/model"
	"example.com/trace-weave/internal/strictjson"
)

type State struct {
	FormatVersion  int              `json:"format_version"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	OutputPath     string           `json:"output_path"`
	OutputBytes    int64            `json:"output_bytes"`
	OutputRecords  uint64           `json:"output_records"`
	SourceOffsets  map[string]int64 `json:"source_offsets"`
	LastKey        *model.OrderKey  `json:"last_key,omitempty"`
	MinTimestamp   uint64           `json:"min_timestamp"`
	MaxTimestamp   uint64           `json:"max_timestamp"`
	Completed      bool             `json:"completed"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

func (s State) Validate() error {
	if s.FormatVersion != 1 {
		return fmt.Errorf("checkpoint format_version must be 1, got %d", s.FormatVersion)
	}
	if len(s.ManifestSHA256) != 64 {
		return errors.New("checkpoint manifest_sha256 must contain 64 hexadecimal characters")
	}
	if s.OutputPath == "" {
		return errors.New("checkpoint output_path is empty")
	}
	if s.OutputBytes < 0 {
		return errors.New("checkpoint output_bytes is negative")
	}
	if s.SourceOffsets == nil {
		return errors.New("checkpoint source_offsets is null")
	}
	for rank, offset := range s.SourceOffsets {
		if rank == "" || offset < 0 {
			return fmt.Errorf("checkpoint source offset %q=%d is invalid", rank, offset)
		}
	}
	if s.OutputRecords == 0 && s.LastKey != nil {
		return errors.New("empty checkpoint must not have last_key")
	}
	if s.OutputRecords != 0 && s.LastKey == nil {
		return errors.New("non-empty checkpoint requires last_key")
	}
	return nil
}

func Load(path string) (State, error) {
	var state State
	if _, err := strictjson.ReadFile(path, &state); err != nil {
		return State{}, fmt.Errorf("read checkpoint: %w", err)
	}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

func SaveAtomic(path string, state State) error {
	state.UpdatedAt = time.Now().UTC()
	if err := state.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	published := false
	defer func() {
		_ = temporary.Close()
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	published = true
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
