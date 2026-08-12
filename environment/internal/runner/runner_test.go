package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"example.com/trace-weave/internal/config"
	"example.com/trace-weave/internal/generator"
	"example.com/trace-weave/internal/inspect"
)

func TestSingleRankFreshMerge(t *testing.T) {
	directory := t.TempDir()
	input := filepath.Join(directory, "input")
	generated, err := generator.Generate(generator.Options{
		Root: input, JobID: "normal", Ranks: 1, Records: 12,
		Epoch: 5, PayloadBytes: 16, SequenceMode: "rank-local", DelayedRank: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "merge.json")
	body := fmt.Sprintf(`{"manifest":%q,"output":"out.twseg","checkpoint":"state.json","checkpoint_every_records":5,"read_chunk_bytes":0,"channel_capacity":4,"output_buffer_bytes":4096,"max_payload_bytes":4096}`, generated.ManifestPath)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.EmittedRecords != 12 || result.Summary.OrderingViolations != 0 {
		t.Fatalf("result=%+v", result)
	}
	report, err := inspect.Verify(cfg.OutputPath, generated.ManifestPath, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if report.UniqueRecords != 12 || !report.Ordered {
		t.Fatalf("report=%+v", report)
	}
}
