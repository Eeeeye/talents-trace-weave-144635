package model

import (
	"bytes"
	"fmt"
)

type Record struct {
	Epoch     uint64
	Rank      uint32
	Sequence  uint64
	Timestamp uint64
	Kind      uint16
	Flags     uint16
	Payload   []byte
}

func (r Record) Clone() Record {
	copyRecord := r
	copyRecord.Payload = append([]byte(nil), r.Payload...)
	return copyRecord
}

type Identity struct {
	Epoch    uint64 `json:"epoch"`
	Rank     uint32 `json:"rank"`
	Sequence uint64 `json:"sequence"`
}

func (r Record) Identity() Identity {
	return Identity{Epoch: r.Epoch, Rank: r.Rank, Sequence: r.Sequence}
}

type OrderKey struct {
	Timestamp uint64 `json:"timestamp"`
	Rank      uint32 `json:"rank"`
	Sequence  uint64 `json:"sequence"`
}

func (r Record) OrderKey() OrderKey {
	return OrderKey{Timestamp: r.Timestamp, Rank: r.Rank, Sequence: r.Sequence}
}

func CompareOrder(left, right OrderKey) int {
	if left.Timestamp < right.Timestamp {
		return -1
	}
	if left.Timestamp > right.Timestamp {
		return 1
	}
	if left.Rank < right.Rank {
		return -1
	}
	if left.Rank > right.Rank {
		return 1
	}
	if left.Sequence < right.Sequence {
		return -1
	}
	if left.Sequence > right.Sequence {
		return 1
	}
	return 0
}

func CompareRecord(left, right Record) int {
	if comparison := CompareOrder(left.OrderKey(), right.OrderKey()); comparison != 0 {
		return comparison
	}
	if left.Epoch < right.Epoch {
		return -1
	}
	if left.Epoch > right.Epoch {
		return 1
	}
	if left.Kind < right.Kind {
		return -1
	}
	if left.Kind > right.Kind {
		return 1
	}
	if left.Flags < right.Flags {
		return -1
	}
	if left.Flags > right.Flags {
		return 1
	}
	return bytes.Compare(left.Payload, right.Payload)
}

func (i Identity) String() string {
	return fmt.Sprintf("%d/%d/%d", i.Epoch, i.Rank, i.Sequence)
}
