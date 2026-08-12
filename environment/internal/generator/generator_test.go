package generator

import (
	"testing"

	"example.com/trace-weave/internal/manifest"
)

func TestGenerateDeterministicManifest(t *testing.T) {
	result, err := Generate(Options{
		Root: t.TempDir() + "/fixture", JobID: "job", Ranks: 3,
		Records: 4, Epoch: 9, PayloadBytes: 32,
		SequenceMode: "rank-local", DelayedRank: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manifest.Load(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorldSize != 3 || loaded.TotalRecords() != 12 {
		t.Fatalf("manifest=%+v", loaded)
	}
}
