package model

import "testing"

func TestCanonicalOrder(t *testing.T) {
	tests := []struct {
		left, right OrderKey
		want        int
	}{
		{OrderKey{Timestamp: 1}, OrderKey{Timestamp: 2}, -1},
		{OrderKey{Timestamp: 2}, OrderKey{Timestamp: 1}, 1},
		{OrderKey{Timestamp: 2, Rank: 1}, OrderKey{Timestamp: 2, Rank: 2}, -1},
		{OrderKey{Timestamp: 2, Rank: 2, Sequence: 5}, OrderKey{Timestamp: 2, Rank: 2, Sequence: 5}, 0},
	}
	for _, tc := range tests {
		if got := CompareOrder(tc.left, tc.right); got != tc.want {
			t.Fatalf("CompareOrder(%+v,%+v)=%d want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestRecordCloneOwnsPayload(t *testing.T) {
	original := Record{Payload: []byte{1, 2, 3}}
	clone := original.Clone()
	clone.Payload[0] = 9
	if original.Payload[0] != 1 {
		t.Fatal("clone shares payload")
	}
}
