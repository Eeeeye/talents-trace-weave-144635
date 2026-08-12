package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"example.com/trace-weave/internal/generator"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "tracegen: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	set := flag.NewFlagSet("tracegen", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	options := generator.Options{}
	set.StringVar(&options.Root, "root", "", "output fixture directory")
	set.StringVar(&options.JobID, "job", "trace-job", "trace job identifier")
	set.IntVar(&options.Ranks, "ranks", 1, "MPI ranks")
	set.IntVar(&options.Records, "records", 20, "records per rank")
	set.Uint64Var(&options.Epoch, "epoch", 4_815_162_342, "controller/job epoch")
	set.IntVar(&options.PayloadBytes, "payload-bytes", 64, "payload bytes per record")
	set.StringVar(&options.SequenceMode, "sequence-mode", "rank-local", "rank-local or global")
	set.IntVar(&options.DelayedRank, "delay-rank", -1, "rank with an artificial per-record delay")
	set.IntVar(&options.DelayMS, "delay-ms", 0, "per-record delay for delay-rank")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if len(set.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	result, err := generator.Generate(options)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
