package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"example.com/trace-weave/internal/strictjson"
)

type Input struct {
	Rank        uint32 `json:"rank"`
	Path        string `json:"path"`
	RecordCount uint64 `json:"record_count"`
	ReadDelayMS int    `json:"read_delay_ms"`
}

type Manifest struct {
	FormatVersion int     `json:"format_version"`
	JobID         string  `json:"job_id"`
	WorldSize     uint32  `json:"world_size"`
	Epoch         uint64  `json:"epoch"`
	Inputs        []Input `json:"inputs"`
}

type ResolvedInput struct {
	Input
	AbsolutePath string
}

type Resolved struct {
	Manifest
	Path           string
	SHA256         string
	InputsResolved []ResolvedInput
}

func Load(path string) (Resolved, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Resolved{}, err
	}
	var value Manifest
	data, err := strictjson.ReadFile(absolute, &value)
	if err != nil {
		return Resolved{}, fmt.Errorf("read manifest: %w", err)
	}
	if err := value.Validate(); err != nil {
		return Resolved{}, err
	}
	digest := sha256.Sum256(data)
	resolved := Resolved{
		Manifest: value, Path: absolute, SHA256: hex.EncodeToString(digest[:]),
		InputsResolved: make([]ResolvedInput, 0, len(value.Inputs)),
	}
	base := filepath.Dir(absolute)
	for _, input := range value.Inputs {
		path := input.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		resolved.InputsResolved = append(resolved.InputsResolved, ResolvedInput{
			Input: input, AbsolutePath: filepath.Clean(path),
		})
	}
	return resolved, nil
}

func (m Manifest) Validate() error {
	var problems []string
	if m.FormatVersion != 1 {
		problems = append(problems, "format_version must be 1")
	}
	if strings.TrimSpace(m.JobID) == "" || len(m.JobID) > 128 {
		problems = append(problems, "job_id length must be in [1,128]")
	}
	if m.WorldSize < 1 || m.WorldSize > 1_000_000 {
		problems = append(problems, "world_size must be in [1,1000000]")
	}
	if m.Epoch == 0 {
		problems = append(problems, "epoch must be positive")
	}
	if uint32(len(m.Inputs)) != m.WorldSize {
		problems = append(problems, "inputs length must equal world_size")
	}
	ranks := make(map[uint32]bool, len(m.Inputs))
	paths := make(map[string]bool, len(m.Inputs))
	for index, input := range m.Inputs {
		if input.Rank >= m.WorldSize {
			problems = append(problems, fmt.Sprintf("inputs[%d].rank is outside world_size", index))
		}
		if ranks[input.Rank] {
			problems = append(problems, fmt.Sprintf("rank %d appears more than once", input.Rank))
		}
		ranks[input.Rank] = true
		if strings.TrimSpace(input.Path) == "" {
			problems = append(problems, fmt.Sprintf("inputs[%d].path is empty", index))
		} else if paths[input.Path] {
			problems = append(problems, fmt.Sprintf("path %q appears more than once", input.Path))
		}
		paths[input.Path] = true
		if input.RecordCount > 1_000_000_000 {
			problems = append(problems, fmt.Sprintf("inputs[%d].record_count is too large", index))
		}
		if input.ReadDelayMS < 0 || input.ReadDelayMS > 60_000 {
			problems = append(problems, fmt.Sprintf("inputs[%d].read_delay_ms is outside [0,60000]", index))
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return errors.New("invalid manifest: " + strings.Join(problems, "; "))
	}
	return nil
}

func (m Resolved) TotalRecords() uint64 {
	var total uint64
	for _, input := range m.Inputs {
		total += input.RecordCount
	}
	return total
}
