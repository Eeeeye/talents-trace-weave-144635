package manifest

import "testing"

func TestManifestValidation(t *testing.T) {
	valid := Manifest{
		FormatVersion: 1, JobID: "job-1", WorldSize: 2, Epoch: 9,
		Inputs: []Input{{Rank: 0, Path: "r0.tws", RecordCount: 3}, {Rank: 1, Path: "r1.tws", RecordCount: 4}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Inputs[1].Rank = 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("expected duplicate rank failure")
	}
}
