package checkpoint

import (
	"path/filepath"
	"testing"

	"example.com/trace-weave/internal/model"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	state := State{
		FormatVersion:  1,
		ManifestSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		OutputPath:     "/tmp/out", OutputBytes: 400, OutputRecords: 3,
		SourceOffsets: map[string]int64{"0": 120},
		LastKey:       &model.OrderKey{Timestamp: 8, Rank: 0, Sequence: 3},
		MinTimestamp:  2, MaxTimestamp: 8,
	}
	if err := SaveAtomic(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OutputBytes != 400 || loaded.LastKey.Sequence != 3 {
		t.Fatalf("loaded=%+v", loaded)
	}
}
