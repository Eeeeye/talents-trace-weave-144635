package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"example.com/trace-weave/internal/config"
	"example.com/trace-weave/internal/runner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "traceweave: %v\n", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	set := flag.NewFlagSet("traceweave", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	var configPath string
	options := runner.Options{}
	set.StringVar(&configPath, "config", "merge.json", "merge configuration")
	set.BoolVar(&options.Resume, "resume", false, "resume from the configured checkpoint")
	set.IntVar(&options.CrashAfterCheckpoints, "crash-after-checkpoints", 0,
		"fault injection: exit 86 after publishing this checkpoint; zero disables")
	if err := set.Parse(arguments); err != nil {
		return err
	}
	if len(set.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(set.Args(), " "))
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	result, err := runner.Run(ctx, cfg, options)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}
