package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"example.com/trace-weave/internal/checkpoint"
	traceformat "example.com/trace-weave/internal/format"
	"example.com/trace-weave/internal/inspect"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "traceinspect: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: traceinspect verify|segment|spool|checkpoint [flags]")
	}
	switch arguments[0] {
	case "verify":
		return verifyCommand(arguments[1:])
	case "segment":
		return segmentCommand(arguments[1:])
	case "spool":
		return spoolCommand(arguments[1:])
	case "checkpoint":
		return checkpointCommand(arguments[1:])
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func verifyCommand(arguments []string) error {
	set := flag.NewFlagSet("verify", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	var segmentPath, manifestPath string
	var maximum int
	set.StringVar(&segmentPath, "segment", "", "merged segment path")
	set.StringVar(&manifestPath, "manifest", "", "input manifest path")
	set.IntVar(&maximum, "max-payload-bytes", 1<<20, "maximum payload size")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if segmentPath == "" || manifestPath == "" || len(set.Args()) != 0 {
		return errors.New("verify requires -segment FILE and -manifest FILE")
	}
	report, err := inspect.Verify(segmentPath, manifestPath, maximum)
	if err != nil {
		return err
	}
	return writeJSON(report)
}

func segmentCommand(arguments []string) error {
	set := flag.NewFlagSet("segment", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	var path string
	var maximum, limit int
	set.StringVar(&path, "path", "", "segment path")
	set.IntVar(&maximum, "max-payload-bytes", 1<<20, "maximum payload size")
	set.IntVar(&limit, "records", 0, "include at most this many records; zero includes none")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if path == "" || len(set.Args()) != 0 || limit < 0 {
		return errors.New("segment requires -path FILE and a non-negative -records")
	}
	scan, err := traceformat.ScanSegment(path, maximum)
	if err != nil {
		return err
	}
	records := scan.Records
	if limit == 0 {
		records = nil
	} else if len(records) > limit {
		records = records[:limit]
	}
	return writeJSON(map[string]any{
		"path": path, "header": scan.Header, "bytes": scan.Bytes, "records": records,
	})
}

func spoolCommand(arguments []string) error {
	set := flag.NewFlagSet("spool", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	var path string
	var maximum int
	set.StringVar(&path, "path", "", "spool path")
	set.IntVar(&maximum, "max-payload-bytes", 1<<20, "maximum payload size")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if path == "" || len(set.Args()) != 0 {
		return errors.New("spool requires -path FILE")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header, err := traceformat.ReadSpoolHeader(file)
	if err != nil {
		return err
	}
	opened, err := traceformat.OpenSpool(path, header.Rank, header.WorldSize, header.Epoch, 0, 0, maximum)
	if err != nil {
		return err
	}
	defer opened.File.Close()
	var count uint64
	for {
		_, err := opened.Decoder.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		count++
	}
	return writeJSON(map[string]any{"path": path, "header": header, "decoded_records": count})
}

func checkpointCommand(arguments []string) error {
	set := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	var path string
	set.StringVar(&path, "path", "", "checkpoint path")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if path == "" || len(set.Args()) != 0 {
		return errors.New("checkpoint requires -path FILE")
	}
	state, err := checkpoint.Load(path)
	if err != nil {
		return err
	}
	return writeJSON(state)
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
