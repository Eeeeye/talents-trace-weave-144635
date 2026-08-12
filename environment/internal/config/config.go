package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"example.com/trace-weave/internal/strictjson"
)

type Config struct {
	Manifest               string `json:"manifest"`
	Output                 string `json:"output"`
	Checkpoint             string `json:"checkpoint"`
	CheckpointEveryRecords int    `json:"checkpoint_every_records"`
	ReadChunkBytes         int    `json:"read_chunk_bytes"`
	ChannelCapacity        int    `json:"channel_capacity"`
	OutputBufferBytes      int    `json:"output_buffer_bytes"`
	MaxPayloadBytes        int    `json:"max_payload_bytes"`
}

type Resolved struct {
	Config
	ConfigPath     string
	ManifestPath   string
	OutputPath     string
	CheckpointPath string
}

func Load(path string) (Resolved, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Resolved{}, err
	}
	var cfg Config
	if _, err := strictjson.ReadFile(absolute, &cfg); err != nil {
		return Resolved{}, fmt.Errorf("read config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Resolved{}, err
	}
	base := filepath.Dir(absolute)
	return Resolved{
		Config:         cfg,
		ConfigPath:     absolute,
		ManifestPath:   resolve(base, cfg.Manifest),
		OutputPath:     resolve(base, cfg.Output),
		CheckpointPath: resolve(base, cfg.Checkpoint),
	}, nil
}

func (c Config) Validate() error {
	var problems []string
	for name, value := range map[string]string{
		"manifest": c.Manifest, "output": c.Output, "checkpoint": c.Checkpoint,
	} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, name+" must not be empty")
		}
	}
	if c.Output != "" && c.Output == c.Checkpoint {
		problems = append(problems, "output and checkpoint must differ")
	}
	if c.CheckpointEveryRecords < 1 || c.CheckpointEveryRecords > 10_000_000 {
		problems = append(problems, "checkpoint_every_records must be in [1,10000000]")
	}
	if c.ReadChunkBytes < 0 || c.ReadChunkBytes > 16<<20 {
		problems = append(problems, "read_chunk_bytes must be in [0,16777216]")
	}
	if c.ChannelCapacity < 0 || c.ChannelCapacity > 1_000_000 {
		problems = append(problems, "channel_capacity must be in [0,1000000]")
	}
	if c.OutputBufferBytes < 256 || c.OutputBufferBytes > 64<<20 {
		problems = append(problems, "output_buffer_bytes must be in [256,67108864]")
	}
	if c.MaxPayloadBytes < 1 || c.MaxPayloadBytes > 64<<20 {
		problems = append(problems, "max_payload_bytes must be in [1,67108864]")
	}
	if len(problems) != 0 {
		return errors.New("invalid config: " + strings.Join(problems, "; "))
	}
	return nil
}

func resolve(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
