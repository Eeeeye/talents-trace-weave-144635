package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResolvesRelativePaths(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "merge.json")
	body := `{"manifest":"in/manifest.json","output":"out.twseg","checkpoint":"state.json","checkpoint_every_records":4,"read_chunk_bytes":0,"channel_capacity":2,"output_buffer_bytes":1024,"max_payload_bytes":4096}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ManifestPath != filepath.Join(directory, "in/manifest.json") {
		t.Fatalf("manifest=%s", resolved.ManifestPath)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merge.json")
	if err := os.WriteFile(path, []byte(`{"unknown":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error=%v", err)
	}
}
