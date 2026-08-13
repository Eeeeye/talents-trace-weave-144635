package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	spoolHeaderSize      = 40
	recordHeaderSize     = 40
	segmentHeadSize      = 56
	landlockCreate       = 444
	landlockAddRule      = 445
	landlockRestrict     = 446
	landlockRulePath     = 1
	prSetNoNewPrivs      = 38
	prSetSecurebits      = 28
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
	linuxCapVersion3     = 0x20080522
)

const (
	fsExecute uint64 = 1 << iota
	fsWriteFile
	fsReadFile
	fsReadDir
	fsRemoveDir
	fsRemoveFile
	fsMakeChar
	fsMakeDir
	fsMakeReg
	fsMakeSock
	fsMakeFIFO
	fsMakeBlock
	fsMakeSymlink
	fsRefer
	fsTruncate
)

const fsHandled = fsExecute | fsWriteFile | fsReadFile | fsReadDir | fsRemoveDir | fsRemoveFile |
	fsMakeChar | fsMakeDir | fsMakeReg | fsMakeSock | fsMakeFIFO | fsMakeBlock | fsMakeSymlink |
	fsRefer | fsTruncate

const fsReadOnly = fsExecute | fsReadFile | fsReadDir
const fsReadWrite = fsHandled &^ (fsMakeBlock | fsMakeChar)

type landlockRulesetAttr struct {
	HandledAccessFS uint64
}

type landlockPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

type linuxCapHeader struct {
	Version uint32
	PID     int32
}

type linuxCapData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

var runNonce uint64

type event struct {
	Epoch     uint64
	Rank      uint32
	Sequence  uint64
	Timestamp uint64
	Kind      uint16
	Flags     uint16
	Payload   []byte
	Start     int64
	End       int64
}

type manifestInput struct {
	Rank        uint32 `json:"rank"`
	Path        string `json:"path"`
	RecordCount uint64 `json:"record_count"`
	ReadDelayMS int    `json:"read_delay_ms"`
}

type manifestDoc struct {
	FormatVersion int             `json:"format_version"`
	JobID         string          `json:"job_id"`
	WorldSize     uint32          `json:"world_size"`
	Epoch         uint64          `json:"epoch"`
	Inputs        []manifestInput `json:"inputs"`
}

type configDoc struct {
	Manifest               string `json:"manifest"`
	Output                 string `json:"output"`
	Checkpoint             string `json:"checkpoint"`
	CheckpointEveryRecords int    `json:"checkpoint_every_records"`
	ReadChunkBytes         int    `json:"read_chunk_bytes"`
	ChannelCapacity        int    `json:"channel_capacity"`
	OutputBufferBytes      int    `json:"output_buffer_bytes"`
	MaxPayloadBytes        int    `json:"max_payload_bytes"`
}

type orderKey struct {
	Timestamp uint64 `json:"timestamp"`
	Rank      uint32 `json:"rank"`
	Sequence  uint64 `json:"sequence"`
}

type checkpointDoc struct {
	FormatVersion  int              `json:"format_version"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	OutputPath     string           `json:"output_path"`
	OutputBytes    int64            `json:"output_bytes"`
	OutputRecords  uint64           `json:"output_records"`
	SourceOffsets  map[string]int64 `json:"source_offsets"`
	LastKey        *orderKey        `json:"last_key,omitempty"`
	MinTimestamp   uint64           `json:"min_timestamp"`
	MaxTimestamp   uint64           `json:"max_timestamp"`
	Completed      bool             `json:"completed"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type mergeResult struct {
	ManifestPath    string `json:"manifest_path"`
	OutputPath      string `json:"output_path"`
	CheckpointPath  string `json:"checkpoint_path"`
	WorldSize       uint32 `json:"world_size"`
	Epoch           uint64 `json:"epoch"`
	ExpectedRecords uint64 `json:"expected_records"`
	Resumed         bool   `json:"resumed"`
	Summary         struct {
		InputRecords       uint64 `json:"input_records"`
		EmittedRecords     uint64 `json:"emitted_records"`
		SuppressedRecords  uint64 `json:"suppressed_records"`
		OrderingViolations uint64 `json:"ordering_violations"`
		CompletedSources   int    `json:"completed_sources"`
	} `json:"summary"`
}

type dataset struct {
	Root            string
	ManifestPath    string
	ConfigPath      string
	OutputPath      string
	CheckpointPath  string
	World           uint32
	Epoch           uint64
	Records         []event
	RecordsByRank   map[uint32][]event
	SpoolPaths      map[uint32]string
	SpoolSizes      map[uint32]int64
	InputSHA256     map[string]string
	ManifestSHA256  string
	CheckpointEvery int
}

type commandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: verifier TRACEWEAVE TRACEGEN TRACEINSPECT CASE_ROOT")
		os.Exit(2)
	}
	traceweave, tracegen, traceinspect, caseRoot := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	if err := binary.Read(rand.Reader, binary.BigEndian, &runNonce); err != nil {
		fatal(fmt.Errorf("verifier nonce: %w", err))
	}
	if err := os.MkdirAll(caseRoot, 0o700); err != nil {
		fatal(err)
	}
	if err := restrictFilesystem(traceweave, caseRoot); err != nil {
		fatal(fmt.Errorf("sandbox setup: %w", err))
	}
	fmt.Println("[verifier] candidate isolation: capability-free Landlock domain")

	tests := []struct {
		name string
		fn   func() error
	}{
		{"full fragmented multi-rank merge", func() error { return testFullMerge(traceweave, traceinspect, caseRoot) }},
		{"alternate scheduling and empty rank", func() error { return testAlternateMerge(traceweave, caseRoot) }},
		{"durable crash boundary and resume", func() error { return testCrashResume(traceweave, caseRoot) }},
		{"empty job", func() error { return testEmptyJob(traceweave, caseRoot) }},
		{"strict failures and artifact safety", func() error { return testFailureSafety(traceweave, caseRoot) }},
		{"companion CLI compatibility", func() error { return testCompanionCLIs(tracegen, traceinspect, caseRoot) }},
	}
	for _, test := range tests {
		fmt.Printf("[verifier] %s\n", test.name)
		if err := test.fn(); err != nil {
			fatal(fmt.Errorf("%s: %w", test.name, err))
		}
	}
	fmt.Println("[verifier] all independent checks passed")
}

func restrictFilesystem(traceweave, caseRoot string) error {
	version, _, errno := syscall.Syscall(landlockCreate, 0, 0, 1)
	if errno != 0 {
		return fmt.Errorf("landlock ABI query: %w", errno)
	}
	if version < 1 {
		return fmt.Errorf("landlock ABI %d is older than required ABI 1", version)
	}
	handled := fsHandled
	if version < 2 {
		handled &^= fsRefer
	}
	if version < 3 {
		handled &^= fsTruncate
	}
	rulesetAttr := landlockRulesetAttr{HandledAccessFS: handled}
	rulesetFD, _, errno := syscall.Syscall(landlockCreate,
		uintptr(unsafe.Pointer(&rulesetAttr)), unsafe.Sizeof(rulesetAttr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock ruleset creation: %w", errno)
	}
	defer syscall.Close(int(rulesetFD))

	if err := addLandlockPath(int(rulesetFD), filepath.Dir(traceweave), fsReadOnly&handled); err != nil {
		return err
	}
	if err := addLandlockPath(int(rulesetFD), "/dev/null", (fsReadFile|fsWriteFile)&handled); err != nil {
		return err
	}
	if err := addLandlockPath(int(rulesetFD), caseRoot, fsReadWrite&handled); err != nil {
		return err
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("set no_new_privs: %w", errno)
	}
	if err := dropCapabilities(); err != nil {
		return err
	}
	if _, _, errno := syscall.Syscall(landlockRestrict, rulesetFD, 0, 0); errno != 0 {
		return fmt.Errorf("landlock restrict self: %w", errno)
	}
	if file, err := os.Open("/tests/verifier.go"); err == nil {
		file.Close()
		return errors.New("sandbox unexpectedly permits reading /tests")
	} else if !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("sandbox /tests probe: %w", err)
	}
	if file, err := os.OpenFile("/logs/verifier/reward.txt", os.O_WRONLY, 0); err == nil {
		file.Close()
		return errors.New("sandbox unexpectedly permits writing /logs")
	} else if !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("sandbox /logs probe: %w", err)
	}
	return nil
}

func dropCapabilities() error {
	const secureNoRoot = 1 | 2 | 4 | 8
	_, _, secureErrno := syscall.Syscall6(syscall.SYS_PRCTL, prSetSecurebits, secureNoRoot, 0, 0, 0, 0)
	if secureErrno != 0 && secureErrno != syscall.EPERM {
		return fmt.Errorf("set securebits: %w", secureErrno)
	}
	_, _, ambientErrno := syscall.Syscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0)
	if ambientErrno != 0 && ambientErrno != syscall.EINVAL {
		return fmt.Errorf("clear ambient capabilities: %w", ambientErrno)
	}
	header := linuxCapHeader{Version: linuxCapVersion3}
	data := [2]linuxCapData{}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("drop process capabilities: %w", errno)
	}
	observed := [2]linuxCapData{}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPGET,
		uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&observed[0])), 0); errno != 0 {
		return fmt.Errorf("verify process capabilities: %w", errno)
	}
	for _, set := range observed {
		if set.Effective != 0 || set.Permitted != 0 || set.Inheritable != 0 {
			return errors.New("process capabilities remain after sandbox setup")
		}
	}
	return nil
}

func addLandlockPath(rulesetFD int, path string, access uint64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open sandbox path %s: %w", path, err)
	}
	defer file.Close()
	attr := landlockPathBeneathAttr{AllowedAccess: access, ParentFD: int32(file.Fd())}
	_, _, errno := syscall.Syscall6(landlockAddRule, uintptr(rulesetFD), landlockRulePath,
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("add sandbox path %s: %w", path, errno)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "trace-weave verifier:", err)
	os.Exit(1)
}

func testFullMerge(traceweave, traceinspect, root string) error {
	counts := []int{19, 19, 0, 19, 19}
	records := makeRecords((uint64(1)<<63)+880301, counts, 11)
	ds, err := createDataset(filepath.Join(root, "full merge 空格"), "full-fragmented", records,
		map[uint32]int{0: 1, 1: 0, 2: 2, 3: 1, 4: 0}, configDoc{
			CheckpointEveryRecords: 7, ReadChunkBytes: 1, ChannelCapacity: 0,
			OutputBufferBytes: 257, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 30*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure("fresh merge", result)
	}
	if err := validateCompleted(ds, result.Stdout, false, true); err != nil {
		return err
	}
	if err := verifyInputsUnchanged(ds); err != nil {
		return err
	}

	inspect, err := runCandidate(traceinspect, ds.Root, 15*time.Second,
		"verify", "-segment", ds.OutputPath, "-manifest", ds.ManifestPath, "-max-payload-bytes", "4096")
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return commandFailure("traceinspect verify", inspect)
	}
	var report struct {
		ExpectedRecords uint64 `json:"expected_records"`
		SegmentRecords  uint64 `json:"segment_records"`
		UniqueRecords   uint64 `json:"unique_records"`
		Ordered         bool   `json:"ordered"`
		PayloadsMatch   bool   `json:"payloads_match"`
	}
	if err := decodeOneJSON(inspect.Stdout, &report, false); err != nil {
		return fmt.Errorf("traceinspect verify JSON: %w", err)
	}
	if report.ExpectedRecords != uint64(len(ds.Records)) || report.SegmentRecords != uint64(len(ds.Records)) ||
		report.UniqueRecords != uint64(len(ds.Records)) || !report.Ordered || !report.PayloadsMatch {
		return fmt.Errorf("traceinspect report disagrees with independent oracle: %+v", report)
	}
	return nil
}

func testAlternateMerge(traceweave, root string) error {
	counts := []int{31, 0, 23, 17, 29, 5}
	records := makeRecords((uint64(1)<<63)+991117, counts, 29)
	ds, err := createDataset(filepath.Join(root, "alternate"), "alternate-schedule", records,
		map[uint32]int{0: 0, 1: 4, 2: 1, 3: 2, 4: 0, 5: 3}, configDoc{
			CheckpointEveryRecords: 13, ReadChunkBytes: 7, ChannelCapacity: 4096,
			OutputBufferBytes: 65536, MaxPayloadBytes: 8192,
		})
	if err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 30*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure("alternate merge", result)
	}
	if err := validateCompleted(ds, result.Stdout, false, true); err != nil {
		return err
	}
	return verifyInputsUnchanged(ds)
}

func testCrashResume(traceweave, root string) error {
	counts := []int{37, 37, 37, 37}
	records := makeRecords((uint64(1)<<63)+771337, counts, 47)
	ds, err := createDataset(filepath.Join(root, "crash-resume"), "crash-resume", records,
		map[uint32]int{0: 0, 1: 2, 2: 1, 3: 3}, configDoc{
			CheckpointEveryRecords: 11, ReadChunkBytes: 3, ChannelCapacity: 512,
			OutputBufferBytes: 65536, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	crash, err := runCandidate(traceweave, ds.Root, 30*time.Second,
		"-config", ds.ConfigPath, "-crash-after-checkpoints", "2")
	if err != nil {
		return err
	}
	if crash.ExitCode != 86 {
		return commandFailure("fault injection (wanted exit 86)", crash)
	}
	state, crashOutput, crashState, err := validateCrashPrefix(ds, uint64(ds.CheckpointEvery*2))
	if err != nil {
		return err
	}
	if err := verifyInputsUnchanged(ds); err != nil {
		return err
	}

	shortSize := state.OutputBytes - 1
	if shortSize < segmentHeadSize {
		return errors.New("test fixture produced an unexpectedly short checkpoint")
	}
	if err := os.Truncate(ds.OutputPath, shortSize); err != nil {
		return err
	}
	shortBefore, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	shortRun, err := runCandidate(traceweave, ds.Root, 15*time.Second,
		"-config", ds.ConfigPath, "-resume")
	if err != nil {
		return err
	}
	if shortRun.ExitCode == 0 {
		return errors.New("resume accepted an output shorter than the durable checkpoint boundary")
	}
	shortAfter, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	stateAfterShort, err := os.ReadFile(ds.CheckpointPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(shortBefore, shortAfter) {
		return errors.New("failed short-output resume modified or extended the output")
	}
	if !bytes.Equal(crashState, stateAfterShort) {
		return errors.New("failed short-output resume modified the checkpoint")
	}

	if err := os.WriteFile(ds.OutputPath, crashOutput, 0o600); err != nil {
		return err
	}
	file, err := os.OpenFile(ds.OutputPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(bytes.Repeat([]byte{0xa7}, 73))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	resume, err := runCandidate(traceweave, ds.Root, 30*time.Second,
		"-config", ds.ConfigPath, "-resume")
	if err != nil {
		return err
	}
	if resume.ExitCode != 0 {
		return commandFailure("resume from durable prefix", resume)
	}
	if err := validateCompleted(ds, resume.Stdout, true, false); err != nil {
		return err
	}
	if err := verifyInputsUnchanged(ds); err != nil {
		return err
	}

	completedState, _, err := readCheckpoint(ds.CheckpointPath)
	if err != nil {
		return err
	}
	completedBytes, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	if int64(len(completedBytes)) != completedState.OutputBytes {
		return fmt.Errorf("completed output size %d differs from checkpoint %d", len(completedBytes), completedState.OutputBytes)
	}
	return nil
}

func testEmptyJob(traceweave, root string) error {
	records := makeRecords(90117, []int{0, 0, 0}, 83)
	ds, err := createDataset(filepath.Join(root, "empty"), "empty-job", records,
		map[uint32]int{0: 2, 1: 0, 2: 1}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 1, ChannelCapacity: 0,
			OutputBufferBytes: 256, MaxPayloadBytes: 1,
		})
	if err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return commandFailure("empty merge", result)
	}
	return validateCompleted(ds, result.Stdout, false, true)
}

func testFailureSafety(traceweave, root string) error {
	if err := testStrictManifest(traceweave, root); err != nil {
		return err
	}
	if err := testSourceInvariants(traceweave, root); err != nil {
		return err
	}
	if err := testBadCRC(traceweave, root); err != nil {
		return err
	}
	if err := testTruncatedFrame(traceweave, root); err != nil {
		return err
	}
	if err := testStrictConfig(traceweave, root); err != nil {
		return err
	}
	if err := testExistingArtifacts(traceweave, root); err != nil {
		return err
	}
	if err := testInvalidResumeStates(traceweave, root); err != nil {
		return err
	}
	return nil
}

func testStrictManifest(traceweave, root string) error {
	type manifestCase struct {
		name   string
		mutate func([]byte) ([]byte, error)
	}
	objectMutation := func(mutate func(map[string]json.RawMessage)) func([]byte) ([]byte, error) {
		return func(data []byte) ([]byte, error) {
			return rewriteJSONObject(data, func(object map[string]json.RawMessage) error {
				mutate(object)
				return nil
			})
		}
	}
	documentMutation := func(mutate func(*manifestDoc)) func([]byte) ([]byte, error) {
		return func(data []byte) ([]byte, error) {
			var document manifestDoc
			if err := decodeOneJSON(data, &document, true); err != nil {
				return nil, err
			}
			mutate(&document)
			return marshalJSON(document)
		}
	}
	cases := []manifestCase{
		{"unknown top-level field", objectMutation(func(object map[string]json.RawMessage) {
			object["verifier_unknown"] = json.RawMessage(`true`)
		})},
		{"trailing JSON value", func(data []byte) ([]byte, error) {
			return append(append([]byte(nil), bytes.TrimSpace(data)...), []byte("\n{\"trailing\":true}\n")...), nil
		}},
		{"missing required format_version", objectMutation(func(object map[string]json.RawMessage) {
			delete(object, "format_version")
		})},
		{"wrong-type world_size", objectMutation(func(object map[string]json.RawMessage) {
			object["world_size"] = json.RawMessage(`"two"`)
		})},
		{"input missing record_count", func(data []byte) ([]byte, error) {
			return mutateManifestInput(data, 0, func(object map[string]json.RawMessage) {
				delete(object, "record_count")
			})
		}},
		{"input unknown field", func(data []byte) ([]byte, error) {
			return mutateManifestInput(data, 0, func(object map[string]json.RawMessage) {
				object["verifier_unknown"] = json.RawMessage(`true`)
			})
		}},
		{"unsupported format version", documentMutation(func(document *manifestDoc) {
			document.FormatVersion = 2
		})},
		{"empty job ID", documentMutation(func(document *manifestDoc) {
			document.JobID = ""
		})},
		{"zero epoch", documentMutation(func(document *manifestDoc) {
			document.Epoch = 0
		})},
		{"world size and input count mismatch", documentMutation(func(document *manifestDoc) {
			document.WorldSize++
		})},
		{"duplicate rank", documentMutation(func(document *manifestDoc) {
			document.Inputs[1].Rank = document.Inputs[0].Rank
		})},
		{"rank outside world size", documentMutation(func(document *manifestDoc) {
			document.Inputs[0].Rank = document.WorldSize
		})},
		{"duplicate path", documentMutation(func(document *manifestDoc) {
			document.Inputs[1].Path = document.Inputs[0].Path
		})},
		{"empty input path", documentMutation(func(document *manifestDoc) {
			document.Inputs[0].Path = ""
		})},
		{"record count above bound", documentMutation(func(document *manifestDoc) {
			document.Inputs[0].RecordCount = 1_000_000_001
		})},
		{"negative read delay", documentMutation(func(document *manifestDoc) {
			document.Inputs[0].ReadDelayMS = -1
		})},
		{"read delay above bound", documentMutation(func(document *manifestDoc) {
			document.Inputs[0].ReadDelayMS = 60_001
		})},
	}

	for index, test := range cases {
		ds, err := createDataset(filepath.Join(root, fmt.Sprintf("strict-manifest-%02d", index)),
			"strict-manifest", makeRecords(21001+uint64(index), []int{2, 1}, 191+uint64(index)),
			map[uint32]int{}, configDoc{
				CheckpointEveryRecords: 1, ReadChunkBytes: 1, ChannelCapacity: 2,
				OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
			})
		if err != nil {
			return err
		}
		original, err := os.ReadFile(ds.ManifestPath)
		if err != nil {
			return err
		}
		invalid, err := test.mutate(original)
		if err != nil {
			return fmt.Errorf("prepare manifest case %q: %w", test.name, err)
		}
		if err := os.WriteFile(ds.ManifestPath, invalid, 0o600); err != nil {
			return err
		}
		if err := refreshInputDigests(ds); err != nil {
			return err
		}
		if err := expectInputFailure(traceweave, ds, "manifest "+test.name); err != nil {
			return err
		}
	}
	return nil
}

func testSourceInvariants(traceweave, root string) error {
	type sourceCase struct {
		name   string
		mutate func([]byte, []event) error
	}
	cases := []sourceCase{
		{"decreasing per-rank timestamp", func(data []byte, records []event) error {
			if records[0].Timestamp == 0 {
				return errors.New("source fixture timestamp cannot be decremented")
			}
			binary.BigEndian.PutUint64(data[records[1].Start+24:records[1].Start+32], records[0].Timestamp-1)
			return nil
		}},
		{"duplicate positive rank-local sequence", func(data []byte, records []event) error {
			binary.BigEndian.PutUint64(data[records[1].Start+16:records[1].Start+24], records[0].Sequence)
			return nil
		}},
	}
	for index, test := range cases {
		ds, err := createDataset(filepath.Join(root, fmt.Sprintf("source-invariant-%02d", index)),
			"source-invariant", makeRecords(22001+uint64(index), []int{4}, 223+uint64(index)),
			map[uint32]int{}, configDoc{
				CheckpointEveryRecords: 1, ReadChunkBytes: 2, ChannelCapacity: 2,
				OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
			})
		if err != nil {
			return err
		}
		spool := ds.SpoolPaths[0]
		data, err := os.ReadFile(spool)
		if err != nil {
			return err
		}
		records := ds.RecordsByRank[0]
		if len(records) < 2 {
			return errors.New("source invariant fixture has fewer than two records")
		}
		if err := test.mutate(data, records); err != nil {
			return err
		}
		if err := os.WriteFile(spool, data, 0o600); err != nil {
			return err
		}
		if err := refreshInputDigests(ds); err != nil {
			return err
		}
		if err := expectInputFailure(traceweave, ds, test.name); err != nil {
			return err
		}
	}
	return nil
}

func testBadCRC(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "bad-crc"), "bad-crc",
		makeRecords(3001, []int{3}, 101), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 2, ChannelCapacity: 8,
			OutputBufferBytes: 4096, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	spool := ds.SpoolPaths[0]
	data, err := os.ReadFile(spool)
	if err != nil {
		return err
	}
	if len(data) <= spoolHeaderSize+recordHeaderSize {
		return errors.New("bad CRC fixture has no payload")
	}
	data[spoolHeaderSize+recordHeaderSize] ^= 0xff
	if err := os.WriteFile(spool, data, 0o600); err != nil {
		return err
	}
	if err := refreshInputDigests(ds); err != nil {
		return err
	}
	return expectInputFailure(traceweave, ds, "CRC-corrupted input")
}

func testTruncatedFrame(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "truncated"), "truncated",
		makeRecords(3002, []int{4}, 103), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 2, ReadChunkBytes: 5, ChannelCapacity: 8,
			OutputBufferBytes: 4096, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	spool := ds.SpoolPaths[0]
	info, err := os.Stat(spool)
	if err != nil {
		return err
	}
	if err := os.Truncate(spool, info.Size()-1); err != nil {
		return err
	}
	if err := refreshInputDigests(ds); err != nil {
		return err
	}
	return expectInputFailure(traceweave, ds, "truncated input")
}

func testStrictConfig(traceweave, root string) error {
	type configCase struct {
		name   string
		mutate func([]byte) ([]byte, error)
	}
	objectMutation := func(mutate func(map[string]json.RawMessage)) func([]byte) ([]byte, error) {
		return func(data []byte) ([]byte, error) {
			return rewriteJSONObject(data, func(object map[string]json.RawMessage) error {
				mutate(object)
				return nil
			})
		}
	}
	documentMutation := func(mutate func(*configDoc)) func([]byte) ([]byte, error) {
		return func(data []byte) ([]byte, error) {
			var document configDoc
			if err := decodeOneJSON(data, &document, true); err != nil {
				return nil, err
			}
			mutate(&document)
			return marshalJSON(document)
		}
	}
	cases := []configCase{
		{"unknown field", objectMutation(func(object map[string]json.RawMessage) {
			object["verifier_unknown"] = json.RawMessage(`true`)
		})},
		{"trailing JSON value", func(data []byte) ([]byte, error) {
			return append(append([]byte(nil), bytes.TrimSpace(data)...), []byte("\nnull\n")...), nil
		}},
		{"missing manifest", objectMutation(func(object map[string]json.RawMessage) {
			delete(object, "manifest")
		})},
		{"wrong-type checkpoint interval", objectMutation(func(object map[string]json.RawMessage) {
			object["checkpoint_every_records"] = json.RawMessage(`"one"`)
		})},
		{"same output and checkpoint", documentMutation(func(document *configDoc) {
			document.Checkpoint = document.Output
		})},
		{"zero checkpoint interval", documentMutation(func(document *configDoc) {
			document.CheckpointEveryRecords = 0
		})},
		{"negative read chunk", documentMutation(func(document *configDoc) {
			document.ReadChunkBytes = -1
		})},
		{"negative channel capacity", documentMutation(func(document *configDoc) {
			document.ChannelCapacity = -1
		})},
		{"too-small output buffer", documentMutation(func(document *configDoc) {
			document.OutputBufferBytes = 255
		})},
		{"zero maximum payload", documentMutation(func(document *configDoc) {
			document.MaxPayloadBytes = 0
		})},
	}
	for index, test := range cases {
		ds, err := createDataset(filepath.Join(root, fmt.Sprintf("strict-config-%02d", index)), "strict-config",
			makeRecords(3003+uint64(index), []int{2}, 107+uint64(index)), map[uint32]int{}, configDoc{
				CheckpointEveryRecords: 1, ReadChunkBytes: 0, ChannelCapacity: 1,
				OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
			})
		if err != nil {
			return err
		}
		data, err := os.ReadFile(ds.ConfigPath)
		if err != nil {
			return err
		}
		invalid, err := test.mutate(data)
		if err != nil {
			return fmt.Errorf("prepare config case %q: %w", test.name, err)
		}
		if err := os.WriteFile(ds.ConfigPath, invalid, 0o600); err != nil {
			return err
		}
		result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
		if err != nil {
			return err
		}
		if result.ExitCode == 0 {
			return fmt.Errorf("invalid configuration %q was accepted", test.name)
		}
		if _, err := os.Stat(ds.OutputPath); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("strict config failure %q created output: %v", test.name, err)
		}
		if _, err := os.Stat(ds.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("strict config failure %q created checkpoint: %v", test.name, err)
		}
		if err := verifyInputsUnchanged(ds); err != nil {
			return err
		}
	}
	return nil
}

func testExistingArtifacts(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "existing-output"), "existing-output",
		makeRecords(3004, []int{2}, 109), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 0, ChannelCapacity: 1,
			OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ds.OutputPath), 0o700); err != nil {
		return err
	}
	sentinel := []byte("existing-output-must-survive\x00\xff")
	if err := os.WriteFile(ds.OutputPath, sentinel, 0o600); err != nil {
		return err
	}
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return errors.New("fresh run replaced an existing output")
	}
	after, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(sentinel, after) {
		return errors.New("fresh-run failure modified existing output bytes")
	}
	if _, err := os.Stat(ds.CheckpointPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("existing-output failure created checkpoint: %v", err)
	}

	ds2, err := createDataset(filepath.Join(root, "existing-checkpoint"), "existing-checkpoint",
		makeRecords(3005, []int{2}, 113), map[uint32]int{}, configDoc{
			CheckpointEveryRecords: 1, ReadChunkBytes: 0, ChannelCapacity: 1,
			OutputBufferBytes: 1024, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ds2.CheckpointPath), 0o700); err != nil {
		return err
	}
	checkpointSentinel := []byte("existing checkpoint is intentionally opaque\n")
	if err := os.WriteFile(ds2.CheckpointPath, checkpointSentinel, 0o600); err != nil {
		return err
	}
	result2, err := runCandidate(traceweave, ds2.Root, 15*time.Second, "-config", ds2.ConfigPath)
	if err != nil {
		return err
	}
	if result2.ExitCode == 0 {
		return errors.New("fresh run accepted an existing checkpoint")
	}
	afterCheckpoint, err := os.ReadFile(ds2.CheckpointPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(checkpointSentinel, afterCheckpoint) {
		return errors.New("fresh-run failure modified existing checkpoint")
	}
	if _, err := os.Stat(ds2.OutputPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("existing-checkpoint failure created output: %v", err)
	}
	return nil
}

func testInvalidResumeStates(traceweave, root string) error {
	ds, err := createDataset(filepath.Join(root, "invalid-resume"), "invalid-resume",
		makeRecords(23001, []int{11, 9}, 251), map[uint32]int{0: 1, 1: 0}, configDoc{
			CheckpointEveryRecords: 5, ReadChunkBytes: 3, ChannelCapacity: 16,
			OutputBufferBytes: 4096, MaxPayloadBytes: 4096,
		})
	if err != nil {
		return err
	}
	crash, err := runCandidate(traceweave, ds.Root, 30*time.Second,
		"-config", ds.ConfigPath, "-crash-after-checkpoints", "1")
	if err != nil {
		return err
	}
	if crash.ExitCode != 86 {
		return commandFailure("invalid-resume fixture crash (wanted exit 86)", crash)
	}
	_, baseOutput, baseCheckpoint, err := validateCrashPrefix(ds, 5)
	if err != nil {
		return err
	}

	type resumeCase struct {
		name   string
		mutate func(*checkpointDoc, []byte, []byte) ([]byte, []byte, error)
	}
	stateMutation := func(mutate func(*checkpointDoc)) func(*checkpointDoc, []byte, []byte) ([]byte, []byte, error) {
		return func(state *checkpointDoc, _ []byte, output []byte) ([]byte, []byte, error) {
			mutate(state)
			encoded, err := marshalJSON(state)
			return encoded, append([]byte(nil), output...), err
		}
	}
	rawMutation := func(mutate func(map[string]json.RawMessage)) func(*checkpointDoc, []byte, []byte) ([]byte, []byte, error) {
		return func(_ *checkpointDoc, checkpointBytes, output []byte) ([]byte, []byte, error) {
			encoded, err := rewriteJSONObject(checkpointBytes, func(object map[string]json.RawMessage) error {
				mutate(object)
				return nil
			})
			return encoded, append([]byte(nil), output...), err
		}
	}
	firstRank := uint32(0)
	firstRankKey := strconv.FormatUint(uint64(firstRank), 10)
	firstRecord := ds.RecordsByRank[firstRank][0]
	cases := []resumeCase{
		{"checkpoint unknown field", rawMutation(func(object map[string]json.RawMessage) {
			object["verifier_unknown"] = json.RawMessage(`true`)
		})},
		{"checkpoint trailing JSON value", func(_ *checkpointDoc, checkpointBytes, output []byte) ([]byte, []byte, error) {
			checkpointBytes = append(append([]byte(nil), bytes.TrimSpace(checkpointBytes)...), []byte("\nfalse\n")...)
			return checkpointBytes, append([]byte(nil), output...), nil
		}},
		{"checkpoint missing updated_at", rawMutation(func(object map[string]json.RawMessage) {
			delete(object, "updated_at")
		})},
		{"checkpoint wrong-type output_records", rawMutation(func(object map[string]json.RawMessage) {
			object["output_records"] = json.RawMessage(`"five"`)
		})},
		{"last key missing sequence", rawMutation(func(object map[string]json.RawMessage) {
			var lastKey map[string]json.RawMessage
			_ = json.Unmarshal(object["last_key"], &lastKey)
			delete(lastKey, "sequence")
			object["last_key"], _ = json.Marshal(lastKey)
		})},
		{"last key unknown field", rawMutation(func(object map[string]json.RawMessage) {
			var lastKey map[string]json.RawMessage
			_ = json.Unmarshal(object["last_key"], &lastKey)
			lastKey["verifier_unknown"] = json.RawMessage(`true`)
			object["last_key"], _ = json.Marshal(lastKey)
		})},
		{"unsupported checkpoint format version", stateMutation(func(state *checkpointDoc) {
			state.FormatVersion = 2
		})},
		{"malformed manifest hash", stateMutation(func(state *checkpointDoc) {
			state.ManifestSHA256 = strings.Repeat("g", 64)
		})},
		{"manifest hash mismatch", stateMutation(func(state *checkpointDoc) {
			state.ManifestSHA256 = strings.Repeat("0", 64)
		})},
		{"configured output path mismatch", stateMutation(func(state *checkpointDoc) {
			state.OutputPath = filepath.Join(ds.Root, "other-output.twseg")
		})},
		{"completed checkpoint", stateMutation(func(state *checkpointDoc) {
			state.Completed = true
		})},
		{"missing source rank", stateMutation(func(state *checkpointDoc) {
			delete(state.SourceOffsets, firstRankKey)
		})},
		{"noncanonical source rank key", stateMutation(func(state *checkpointDoc) {
			value := state.SourceOffsets[firstRankKey]
			delete(state.SourceOffsets, firstRankKey)
			state.SourceOffsets["00"] = value
		})},
		{"source rank outside manifest", stateMutation(func(state *checkpointDoc) {
			value := state.SourceOffsets[firstRankKey]
			delete(state.SourceOffsets, firstRankKey)
			state.SourceOffsets[strconv.FormatUint(uint64(ds.World), 10)] = value
		})},
		{"negative source offset", stateMutation(func(state *checkpointDoc) {
			state.SourceOffsets[firstRankKey] = -1
		})},
		{"source offset before spool body", stateMutation(func(state *checkpointDoc) {
			state.SourceOffsets[firstRankKey] = spoolHeaderSize - 1
		})},
		{"source offset beyond EOF", stateMutation(func(state *checkpointDoc) {
			state.SourceOffsets[firstRankKey] = ds.SpoolSizes[firstRank] + 1
		})},
		{"source offset inside a frame", stateMutation(func(state *checkpointDoc) {
			state.SourceOffsets[firstRankKey] = firstRecord.Start + 1
		})},
		{"valid source boundary disagrees with prefix", stateMutation(func(state *checkpointDoc) {
			state.SourceOffsets[firstRankKey] = spoolHeaderSize
		})},
		{"output record count disagrees with prefix", stateMutation(func(state *checkpointDoc) {
			state.OutputRecords++
		})},
		{"last key disagrees with prefix", stateMutation(func(state *checkpointDoc) {
			state.LastKey.Sequence++
		})},
		{"minimum timestamp disagrees with prefix", stateMutation(func(state *checkpointDoc) {
			state.MinTimestamp--
		})},
		{"maximum timestamp disagrees with prefix", stateMutation(func(state *checkpointDoc) {
			state.MaxTimestamp++
		})},
		{"output byte boundary inside a frame", stateMutation(func(state *checkpointDoc) {
			state.OutputBytes--
		})},
		{"output byte boundary before segment body", stateMutation(func(state *checkpointDoc) {
			state.OutputBytes = segmentHeadSize - 1
		})},
		{"checkpointed output record differs from source", func(_ *checkpointDoc, checkpointBytes, output []byte) ([]byte, []byte, error) {
			mutated := append([]byte(nil), output...)
			length := int(binary.BigEndian.Uint32(mutated[segmentHeadSize+4 : segmentHeadSize+8]))
			if length == 0 {
				return nil, nil, errors.New("resume fixture first record has no payload")
			}
			payload := mutated[segmentHeadSize+recordHeaderSize : segmentHeadSize+recordHeaderSize+length]
			payload[0] ^= 0xff
			binary.BigEndian.PutUint32(mutated[segmentHeadSize+32:segmentHeadSize+36], crc32.ChecksumIEEE(payload))
			return append([]byte(nil), checkpointBytes...), mutated, nil
		}},
		{"segment world size mismatch", func(_ *checkpointDoc, checkpointBytes, output []byte) ([]byte, []byte, error) {
			mutated := append([]byte(nil), output...)
			binary.BigEndian.PutUint32(mutated[8:12], ds.World+1)
			return append([]byte(nil), checkpointBytes...), mutated, nil
		}},
		{"segment epoch mismatch", func(_ *checkpointDoc, checkpointBytes, output []byte) ([]byte, []byte, error) {
			mutated := append([]byte(nil), output...)
			binary.BigEndian.PutUint64(mutated[16:24], ds.Epoch+1)
			return append([]byte(nil), checkpointBytes...), mutated, nil
		}},
	}

	for _, test := range cases {
		var state checkpointDoc
		if err := decodeOneJSON(baseCheckpoint, &state, true); err != nil {
			return err
		}
		checkpointBytes, outputBytes, err := test.mutate(&state, baseCheckpoint, baseOutput)
		if err != nil {
			return fmt.Errorf("prepare resume case %q: %w", test.name, err)
		}
		if err := os.WriteFile(ds.OutputPath, outputBytes, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(ds.CheckpointPath, checkpointBytes, 0o600); err != nil {
			return err
		}
		if err := expectResumeFailure(traceweave, ds, test.name, outputBytes, checkpointBytes); err != nil {
			return err
		}
	}
	return nil
}

func expectResumeFailure(traceweave string, ds *dataset, label string, outputBefore, checkpointBefore []byte) error {
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second,
		"-config", ds.ConfigPath, "-resume")
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return fmt.Errorf("invalid resume state %q was accepted", label)
	}
	outputAfter, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return err
	}
	checkpointAfter, err := os.ReadFile(ds.CheckpointPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(outputBefore, outputAfter) {
		return fmt.Errorf("failed resume %q modified output bytes", label)
	}
	if !bytes.Equal(checkpointBefore, checkpointAfter) {
		return fmt.Errorf("failed resume %q modified checkpoint bytes", label)
	}
	return verifyInputsUnchanged(ds)
}

func expectInputFailure(traceweave string, ds *dataset, label string) error {
	result, err := runCandidate(traceweave, ds.Root, 15*time.Second, "-config", ds.ConfigPath)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return fmt.Errorf("%s was accepted", label)
	}
	if data, err := os.ReadFile(ds.CheckpointPath); err == nil {
		var state checkpointDoc
		if decodeOneJSON(data, &state, true) == nil && state.Completed {
			return fmt.Errorf("%s published completed=true", label)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return verifyInputsUnchanged(ds)
}

func testCompanionCLIs(tracegen, traceinspect, root string) error {
	caseDir := filepath.Join(root, "companion-cli")
	if err := freshDir(caseDir); err != nil {
		return err
	}
	generatedRoot := filepath.Join(caseDir, "generated data")
	gen, err := runCandidate(tracegen, caseDir, 15*time.Second,
		"-root", generatedRoot, "-job", "verifier-cli", "-ranks", "2", "-records", "5",
		"-epoch", "9223372036854775999", "-payload-bytes", "17", "-sequence-mode", "rank-local",
		"-delay-rank", "1", "-delay-ms", "1")
	if err != nil {
		return err
	}
	if gen.ExitCode != 0 {
		return commandFailure("tracegen", gen)
	}
	manifest := filepath.Join(generatedRoot, "manifest.json")
	spool := filepath.Join(generatedRoot, "rank-0001.tws")
	for _, path := range []string{manifest, spool} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("tracegen did not create regular file %s: %v", path, err)
		}
	}
	inspect, err := runCandidate(traceinspect, caseDir, 15*time.Second,
		"spool", "-path", spool, "-max-payload-bytes", "17")
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return commandFailure("traceinspect spool", inspect)
	}
	var report struct {
		DecodedRecords uint64 `json:"decoded_records"`
	}
	if err := decodeOneJSON(inspect.Stdout, &report, false); err != nil {
		return err
	}
	if report.DecodedRecords != 5 {
		return fmt.Errorf("traceinspect spool decoded %d records, want 5", report.DecodedRecords)
	}
	return nil
}

func createDataset(root, job string, byRank [][]event, delays map[uint32]int, options configDoc) (*dataset, error) {
	if len(byRank) == 0 {
		return nil, errors.New("dataset requires at least one rank")
	}
	if err := freshDir(root); err != nil {
		return nil, err
	}
	inputDir := filepath.Join(root, "input data β")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		return nil, err
	}
	epoch := uint64(90117)
	for _, records := range byRank {
		if len(records) != 0 {
			epoch = records[0].Epoch
			break
		}
	}
	ds := &dataset{
		Root: root, World: uint32(len(byRank)), Epoch: epoch,
		RecordsByRank: make(map[uint32][]event), SpoolPaths: make(map[uint32]string),
		SpoolSizes: make(map[uint32]int64), InputSHA256: make(map[string]string),
		CheckpointEvery: options.CheckpointEveryRecords,
	}
	inputs := make([]manifestInput, 0, len(byRank))
	for rank := len(byRank) - 1; rank >= 0; rank-- {
		records := append([]event(nil), byRank[rank]...)
		for index := range records {
			records[index].Epoch = epoch
			records[index].Rank = uint32(rank)
		}
		name := fmt.Sprintf("rank %04d λ.tws", rank)
		path := filepath.Join(inputDir, name)
		written, normalized, err := writeSpool(path, uint32(rank), uint32(len(byRank)), epoch, records)
		if err != nil {
			return nil, err
		}
		ds.RecordsByRank[uint32(rank)] = normalized
		ds.Records = append(ds.Records, normalized...)
		ds.SpoolPaths[uint32(rank)] = path
		ds.SpoolSizes[uint32(rank)] = int64(len(written))
		inputs = append(inputs, manifestInput{
			Rank: uint32(rank), Path: name, RecordCount: uint64(len(records)), ReadDelayMS: delays[uint32(rank)],
		})
	}
	sortEvents(ds.Records)
	manifest := manifestDoc{
		FormatVersion: 1, JobID: job, WorldSize: uint32(len(byRank)), Epoch: epoch, Inputs: inputs,
	}
	ds.ManifestPath = filepath.Join(inputDir, "manifest.json")
	manifestBytes, err := writeJSON(ds.ManifestPath, manifest)
	if err != nil {
		return nil, err
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	ds.ManifestSHA256 = hex.EncodeToString(manifestDigest[:])

	options.Manifest = filepath.ToSlash(filepath.Join("input data β", "manifest.json"))
	options.Output = filepath.ToSlash(filepath.Join("output data", "merged result.twseg"))
	options.Checkpoint = filepath.ToSlash(filepath.Join("output data", "merge state.json"))
	ds.ConfigPath = filepath.Join(root, "merge config.json")
	if _, err := writeJSON(ds.ConfigPath, options); err != nil {
		return nil, err
	}
	ds.OutputPath = filepath.Join(root, filepath.FromSlash(options.Output))
	ds.CheckpointPath = filepath.Join(root, filepath.FromSlash(options.Checkpoint))
	if err := refreshInputDigests(ds); err != nil {
		return nil, err
	}
	return ds, nil
}

func makeRecords(epoch uint64, counts []int, salt uint64) [][]event {
	epoch ^= runNonce
	salt ^= runNonce >> 17
	result := make([][]event, len(counts))
	for rank, count := range counts {
		for index := 0; index < count; index++ {
			sequence := uint64(index + 1)
			if index == count-1 && count > 3 {
				sequence = ^uint64(0) - uint64(rank)
			}
			timestamp := (uint64(1) << 63) + salt*100000 + uint64(index/2)*100 + uint64(rank%3)*7
			payloadSizes := []int{1, 0, 39, 40, 41, 97, 513, 17, 255}
			size := payloadSizes[(index+rank*3)%len(payloadSizes)]
			seed := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%d", salt, rank, sequence, timestamp)))
			payload := make([]byte, size)
			for offset := range payload {
				payload[offset] = seed[offset%len(seed)] ^ byte(offset/len(seed))
			}
			result[rank] = append(result[rank], event{
				Epoch: epoch, Rank: uint32(rank), Sequence: sequence, Timestamp: timestamp,
				Kind: uint16(1 + (index+rank)%65534), Flags: uint16((index*17 + rank) % 65536), Payload: payload,
			})
		}
	}
	return result
}

func writeSpool(path string, rank, world uint32, epoch uint64, records []event) ([]byte, []event, error) {
	var buffer bytes.Buffer
	header := make([]byte, spoolHeaderSize)
	copy(header[0:4], []byte("TWS1"))
	binary.BigEndian.PutUint16(header[4:6], 1)
	binary.BigEndian.PutUint16(header[6:8], spoolHeaderSize)
	binary.BigEndian.PutUint32(header[8:12], rank)
	binary.BigEndian.PutUint32(header[12:16], world)
	binary.BigEndian.PutUint64(header[16:24], epoch)
	binary.BigEndian.PutUint64(header[24:32], uint64(len(records)))
	buffer.Write(header)
	normalized := make([]event, 0, len(records))
	for _, record := range records {
		record.Epoch, record.Rank = epoch, rank
		record.Start = int64(buffer.Len())
		frame := encodeFrame(record)
		buffer.Write(frame)
		record.End = int64(buffer.Len())
		normalized = append(normalized, record)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		return nil, nil, err
	}
	return append([]byte(nil), buffer.Bytes()...), normalized, nil
}

func encodeFrame(record event) []byte {
	frame := make([]byte, recordHeaderSize+len(record.Payload))
	copy(frame[0:4], []byte("EVT1"))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(record.Payload)))
	binary.BigEndian.PutUint32(frame[8:12], record.Rank)
	binary.BigEndian.PutUint16(frame[12:14], record.Kind)
	binary.BigEndian.PutUint16(frame[14:16], record.Flags)
	binary.BigEndian.PutUint64(frame[16:24], record.Sequence)
	binary.BigEndian.PutUint64(frame[24:32], record.Timestamp)
	binary.BigEndian.PutUint32(frame[32:36], crc32.ChecksumIEEE(record.Payload))
	copy(frame[recordHeaderSize:], record.Payload)
	return frame
}

func validateCompleted(ds *dataset, stdout []byte, resumed, checkFreshSummary bool) error {
	segment, header, err := decodeCompletedSegment(ds.OutputPath, 64<<20)
	if err != nil {
		return err
	}
	if header.WorldSize != ds.World || header.Epoch != ds.Epoch || header.RecordCount != uint64(len(ds.Records)) {
		return fmt.Errorf("segment header mismatch: %+v", header)
	}
	if len(segment) != len(ds.Records) {
		return fmt.Errorf("segment decoded %d records, want %d", len(segment), len(ds.Records))
	}
	for index := range ds.Records {
		if !sameEvent(segment[index], ds.Records[index]) {
			return fmt.Errorf("record %d mismatch: got=%s want=%s", index, describeEvent(segment[index]), describeEvent(ds.Records[index]))
		}
	}
	if len(ds.Records) == 0 {
		if header.MinTimestamp != 0 || header.MaxTimestamp != 0 {
			return fmt.Errorf("empty segment has timestamp bounds %d..%d", header.MinTimestamp, header.MaxTimestamp)
		}
	} else if header.MinTimestamp != ds.Records[0].Timestamp || header.MaxTimestamp != maximumTimestamp(ds.Records) {
		return fmt.Errorf("segment timestamp bounds %d..%d are wrong", header.MinTimestamp, header.MaxTimestamp)
	}

	state, _, err := readCheckpoint(ds.CheckpointPath)
	if err != nil {
		return err
	}
	if err := validateCheckpointCommon(ds, state); err != nil {
		return err
	}
	outputInfo, err := os.Stat(ds.OutputPath)
	if err != nil {
		return err
	}
	if !state.Completed || state.OutputBytes != outputInfo.Size() || state.OutputRecords != uint64(len(ds.Records)) {
		return fmt.Errorf("completed checkpoint count/size mismatch: %+v file=%d", state, outputInfo.Size())
	}
	for rank := uint32(0); rank < ds.World; rank++ {
		key := strconv.FormatUint(uint64(rank), 10)
		if state.SourceOffsets[key] != ds.SpoolSizes[rank] {
			return fmt.Errorf("completed rank %d offset=%d, want EOF %d", rank, state.SourceOffsets[key], ds.SpoolSizes[rank])
		}
	}
	if len(ds.Records) == 0 {
		if state.LastKey != nil || state.MinTimestamp != 0 || state.MaxTimestamp != 0 {
			return errors.New("empty completed checkpoint has last key or timestamp bounds")
		}
	} else {
		if !sameKey(state.LastKey, keyOf(ds.Records[len(ds.Records)-1])) {
			return fmt.Errorf("completed checkpoint last key mismatch: %+v", state.LastKey)
		}
		if state.MinTimestamp != ds.Records[0].Timestamp || state.MaxTimestamp != maximumTimestamp(ds.Records) {
			return errors.New("completed checkpoint timestamp bounds mismatch")
		}
	}

	var result mergeResult
	if err := decodeOneJSON(stdout, &result, false); err != nil {
		return fmt.Errorf("traceweave success JSON: %w", err)
	}
	if result.Resumed != resumed || result.WorldSize != ds.World || result.Epoch != ds.Epoch ||
		result.ExpectedRecords != uint64(len(ds.Records)) {
		return fmt.Errorf("traceweave result identity mismatch: %+v", result)
	}
	if cleanAbsolute(result.ManifestPath) != cleanAbsolute(ds.ManifestPath) ||
		cleanAbsolute(result.OutputPath) != cleanAbsolute(ds.OutputPath) ||
		cleanAbsolute(result.CheckpointPath) != cleanAbsolute(ds.CheckpointPath) {
		return fmt.Errorf("traceweave result paths mismatch: %+v", result)
	}
	if checkFreshSummary {
		want := uint64(len(ds.Records))
		if result.Summary.InputRecords != want || result.Summary.EmittedRecords != want ||
			result.Summary.SuppressedRecords != 0 || result.Summary.OrderingViolations != 0 ||
			result.Summary.CompletedSources != int(ds.World) {
			return fmt.Errorf("fresh merge summary mismatch: %+v", result.Summary)
		}
	}
	return nil
}

type segmentHeader struct {
	WorldSize    uint32
	Epoch        uint64
	RecordCount  uint64
	MinTimestamp uint64
	MaxTimestamp uint64
}

func decodeCompletedSegment(path string, maxPayload int) ([]event, segmentHeader, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, segmentHeader{}, err
	}
	header, err := decodeSegmentHeader(data)
	if err != nil {
		return nil, segmentHeader{}, err
	}
	records, consumed, err := decodeFrames(data[segmentHeadSize:], header.Epoch, maxPayload)
	if err != nil {
		return nil, header, err
	}
	if consumed != len(data)-segmentHeadSize {
		return nil, header, errors.New("segment decoder left trailing bytes")
	}
	if uint64(len(records)) != header.RecordCount {
		return nil, header, fmt.Errorf("segment header count %d differs from decoded %d", header.RecordCount, len(records))
	}
	return records, header, nil
}

func decodeSegmentHeader(data []byte) (segmentHeader, error) {
	if len(data) < segmentHeadSize {
		return segmentHeader{}, fmt.Errorf("segment is only %d bytes", len(data))
	}
	if string(data[0:4]) != "TWM1" || binary.BigEndian.Uint16(data[4:6]) != 1 ||
		binary.BigEndian.Uint16(data[6:8]) != segmentHeadSize {
		return segmentHeader{}, errors.New("invalid segment magic, version, or header size")
	}
	if binary.BigEndian.Uint32(data[12:16]) != 0 || binary.BigEndian.Uint64(data[48:56]) != 0 {
		return segmentHeader{}, errors.New("non-zero segment reserved bytes")
	}
	header := segmentHeader{
		WorldSize: binary.BigEndian.Uint32(data[8:12]), Epoch: binary.BigEndian.Uint64(data[16:24]),
		RecordCount: binary.BigEndian.Uint64(data[24:32]), MinTimestamp: binary.BigEndian.Uint64(data[32:40]),
		MaxTimestamp: binary.BigEndian.Uint64(data[40:48]),
	}
	if header.WorldSize == 0 || header.Epoch == 0 {
		return segmentHeader{}, errors.New("zero segment world size or epoch")
	}
	return header, nil
}

func decodeFrames(data []byte, epoch uint64, maxPayload int) ([]event, int, error) {
	position := 0
	var records []event
	for position < len(data) {
		if len(data)-position < recordHeaderSize {
			return nil, position, fmt.Errorf("truncated frame header at %d", position)
		}
		header := data[position : position+recordHeaderSize]
		if string(header[0:4]) != "EVT1" {
			return nil, position, fmt.Errorf("invalid frame magic at %d", position)
		}
		length := int(binary.BigEndian.Uint32(header[4:8]))
		if length > maxPayload || length < 0 || len(data)-position-recordHeaderSize < length {
			return nil, position, fmt.Errorf("invalid/truncated payload length %d at %d", length, position)
		}
		if binary.BigEndian.Uint32(header[36:40]) != 0 {
			return nil, position, fmt.Errorf("non-zero frame reserved bytes at %d", position)
		}
		payload := append([]byte(nil), data[position+recordHeaderSize:position+recordHeaderSize+length]...)
		if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(header[32:36]) {
			return nil, position, fmt.Errorf("payload CRC mismatch at %d", position)
		}
		record := event{
			Epoch: epoch, Rank: binary.BigEndian.Uint32(header[8:12]),
			Kind: binary.BigEndian.Uint16(header[12:14]), Flags: binary.BigEndian.Uint16(header[14:16]),
			Sequence: binary.BigEndian.Uint64(header[16:24]), Timestamp: binary.BigEndian.Uint64(header[24:32]),
			Payload: payload, Start: int64(position), End: int64(position + recordHeaderSize + length),
		}
		if record.Sequence == 0 {
			return nil, position, fmt.Errorf("zero sequence at %d", position)
		}
		records = append(records, record)
		position += recordHeaderSize + length
	}
	return records, position, nil
}

func validateCrashPrefix(ds *dataset, expectedRecords uint64) (checkpointDoc, []byte, []byte, error) {
	state, stateBytes, err := readCheckpoint(ds.CheckpointPath)
	if err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if err := validateCheckpointCommon(ds, state); err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if state.Completed || state.OutputRecords != expectedRecords {
		return checkpointDoc{}, nil, nil, fmt.Errorf("crash checkpoint completed=%v records=%d, want incomplete/%d",
			state.Completed, state.OutputRecords, expectedRecords)
	}
	output, err := os.ReadFile(ds.OutputPath)
	if err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if state.OutputBytes < segmentHeadSize || int64(len(output)) < state.OutputBytes {
		return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint boundary %d exceeds durable file %d", state.OutputBytes, len(output))
	}
	boundary := int(state.OutputBytes)
	prefixRecords, consumed, err := decodeFrames(output[segmentHeadSize:boundary], ds.Epoch, 64<<20)
	if err != nil {
		return checkpointDoc{}, nil, nil, err
	}
	if segmentHeadSize+consumed != boundary || uint64(len(prefixRecords)) != expectedRecords {
		return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint prefix decoded %d records/%d bytes", len(prefixRecords), consumed)
	}
	for index := range prefixRecords {
		if !sameEvent(prefixRecords[index], ds.Records[index]) {
			return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint prefix record %d mismatch", index)
		}
	}
	if !sameKey(state.LastKey, keyOf(ds.Records[expectedRecords-1])) {
		return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint last key mismatch: %+v", state.LastKey)
	}
	if state.MinTimestamp != ds.Records[0].Timestamp || state.MaxTimestamp != maximumTimestamp(ds.Records[:expectedRecords]) {
		return checkpointDoc{}, nil, nil, errors.New("checkpoint prefix timestamp bounds mismatch")
	}
	for rank := uint32(0); rank < ds.World; rank++ {
		want := int64(spoolHeaderSize)
		for _, record := range ds.Records[:expectedRecords] {
			if record.Rank == rank {
				want = record.End
			}
		}
		key := strconv.FormatUint(uint64(rank), 10)
		if state.SourceOffsets[key] != want {
			return checkpointDoc{}, nil, nil, fmt.Errorf("checkpoint rank %d offset=%d, want semantic boundary %d",
				rank, state.SourceOffsets[key], want)
		}
	}
	return state, output, stateBytes, nil
}

func validateCheckpointCommon(ds *dataset, state checkpointDoc) error {
	if state.FormatVersion != 1 || state.ManifestSHA256 != ds.ManifestSHA256 ||
		cleanAbsolute(state.OutputPath) != cleanAbsolute(ds.OutputPath) || state.UpdatedAt.IsZero() {
		return fmt.Errorf("checkpoint identity mismatch: version=%d manifest=%q path=%q updated=%v",
			state.FormatVersion, state.ManifestSHA256, state.OutputPath, state.UpdatedAt)
	}
	if len(state.SourceOffsets) != int(ds.World) {
		return fmt.Errorf("checkpoint has %d source offsets, want %d", len(state.SourceOffsets), ds.World)
	}
	for rank := uint32(0); rank < ds.World; rank++ {
		if _, ok := state.SourceOffsets[strconv.FormatUint(uint64(rank), 10)]; !ok {
			return fmt.Errorf("checkpoint missing rank %d offset", rank)
		}
	}
	return nil
}

func readCheckpoint(path string) (checkpointDoc, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return checkpointDoc{}, nil, err
	}
	var state checkpointDoc
	if err := decodeOneJSON(data, &state, true); err != nil {
		return checkpointDoc{}, data, fmt.Errorf("checkpoint JSON: %w", err)
	}
	return state, data, nil
}

func decodeOneJSON(data []byte, destination any, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("more than one JSON value")
		}
		return err
	}
	return nil
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func rewriteJSONObject(data []byte, mutate func(map[string]json.RawMessage) error) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := decodeOneJSON(data, &object, false); err != nil {
		return nil, err
	}
	if err := mutate(object); err != nil {
		return nil, err
	}
	return marshalJSON(object)
}

func mutateManifestInput(data []byte, index int, mutate func(map[string]json.RawMessage)) ([]byte, error) {
	return rewriteJSONObject(data, func(manifest map[string]json.RawMessage) error {
		var inputs []json.RawMessage
		if err := json.Unmarshal(manifest["inputs"], &inputs); err != nil {
			return err
		}
		if index < 0 || index >= len(inputs) {
			return fmt.Errorf("manifest input index %d is outside fixture", index)
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal(inputs[index], &input); err != nil {
			return err
		}
		mutate(input)
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		inputs[index] = encoded
		manifest["inputs"], err = json.Marshal(inputs)
		return err
	})
}

func runCandidate(binary, workdir string, timeout time.Duration, arguments ...string) (commandResult, error) {
	home := filepath.Join(workdir, ".candidate-home")
	temporary := filepath.Join(workdir, ".candidate-tmp")
	for _, path := range []string{home, temporary} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return commandResult{}, err
		}
	}
	candidateEnv := []string{
		"HOME=" + home, "USER=traceweave-candidate", "LOGNAME=traceweave-candidate",
		"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR=" + temporary, "TZ=UTC", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly",
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.Command(binary, arguments...)
	cmd.Dir = workdir
	cmd.Env = candidateEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return commandResult{}, err
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		waitErr = <-done
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		} else {
			return commandResult{}, waitErr
		}
	}
	if timedOut {
		exitCode = -1
	}
	return commandResult{ExitCode: exitCode, Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), TimedOut: timedOut}, nil
}

func commandFailure(label string, result commandResult) error {
	return fmt.Errorf("%s exit=%d timed_out=%v\nstdout:\n%s\nstderr:\n%s",
		label, result.ExitCode, result.TimedOut, bounded(result.Stdout), bounded(result.Stderr))
}

func bounded(data []byte) string {
	const maximum = 8000
	if len(data) <= maximum {
		return string(data)
	}
	return string(data[:maximum]) + "\n...[truncated]"
}

func refreshInputDigests(ds *dataset) error {
	ds.InputSHA256 = make(map[string]string)
	paths := []string{ds.ManifestPath}
	for rank := uint32(0); rank < ds.World; rank++ {
		paths = append(paths, ds.SpoolPaths[rank])
	}
	for _, path := range paths {
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		ds.InputSHA256[path] = digest
	}
	return nil
}

func verifyInputsUnchanged(ds *dataset) error {
	for path, want := range ds.InputSHA256 {
		got, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("input was modified: %s got=%s want=%s", path, got, want)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func writeJSON(path string, value any) ([]byte, error) {
	data, err := marshalJSON(value)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return data, nil
}

func freshDir(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.MkdirAll(path, 0o700)
}

func sortEvents(records []event) {
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i], records[j]
		if left.Timestamp != right.Timestamp {
			return left.Timestamp < right.Timestamp
		}
		if left.Rank != right.Rank {
			return left.Rank < right.Rank
		}
		return left.Sequence < right.Sequence
	})
}

func sameEvent(left, right event) bool {
	return left.Epoch == right.Epoch && left.Rank == right.Rank && left.Sequence == right.Sequence &&
		left.Timestamp == right.Timestamp && left.Kind == right.Kind && left.Flags == right.Flags &&
		bytes.Equal(left.Payload, right.Payload)
}

func keyOf(record event) orderKey {
	return orderKey{Timestamp: record.Timestamp, Rank: record.Rank, Sequence: record.Sequence}
}

func sameKey(left *orderKey, right orderKey) bool {
	return left != nil && *left == right
}

func maximumTimestamp(records []event) uint64 {
	var maximum uint64
	for index, record := range records {
		if index == 0 || record.Timestamp > maximum {
			maximum = record.Timestamp
		}
	}
	return maximum
}

func describeEvent(record event) string {
	return fmt.Sprintf("epoch=%d rank=%d sequence=%d timestamp=%d kind=%d flags=%d payload_sha=%x",
		record.Epoch, record.Rank, record.Sequence, record.Timestamp, record.Kind, record.Flags, sha256.Sum256(record.Payload))
}

func cleanAbsolute(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(absolute)
}
