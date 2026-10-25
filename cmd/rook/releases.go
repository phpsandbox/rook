package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/phpsandbox/rook/internal/host"
)

type operationIDs []string

func (ids *operationIDs) String() string         { return strings.Join(*ids, ",") }
func (ids *operationIDs) Set(value string) error { *ids = append(*ids, value); return nil }

func runReleases(ctx context.Context, args []string, output, diagnostics io.Writer) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(diagnostics, "Usage: rook releases <list|inspect|restore> [release-id] --state-dir PATH --deployment ID")
		return flag.ErrHelp
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: rook releases <list|inspect|restore> [release-id] --state-dir PATH --deployment ID")
	}
	action := args[0]
	if action != "list" && action != "inspect" && action != "restore" {
		return fmt.Errorf("unknown releases command %q", action)
	}
	flags := flag.NewFlagSet("rook releases "+action, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	directory := flags.String("state-dir", "", "existing Rook state directory")
	deployment := flags.String("deployment", "", "deployment ID")
	var expected, service string
	var acknowledged operationIDs
	if action == "restore" {
		flags.StringVar(&expected, "expect-current", "", "expected current release ID")
		flags.StringVar(&service, "rook-service", "", "stopped Rook systemd service")
		flags.Var(&acknowledged, "acknowledge-operation", "inspected uncertain operation ID (repeat for each)")
	}
	// Accept the release ID before flags, as well as the standard flags-first form.
	arguments := args[1:]
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		arguments = append(append([]string{}, arguments[1:]...), arguments[0])
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *directory == "" || *deployment == "" {
		return fmt.Errorf("--state-dir and --deployment are required")
	}
	release := ""
	switch action {
	case "list":
		if flags.NArg() != 0 {
			return fmt.Errorf("list does not accept a release ID")
		}
	case "inspect", "restore":
		if flags.NArg() != 1 {
			return fmt.Errorf("%s requires one retained release ID", action)
		}
		release = flags.Arg(0)
	}
	store, err := host.NewReleaseStore(*directory)
	if err != nil {
		return err
	}
	var result any
	switch action {
	case "list":
		result, err = store.List(*deployment)
	case "inspect":
		result, err = store.Inspect(*deployment, release)
	case "restore":
		result, err = store.Restore(ctx, host.RestoreReleaseOptions{Deployment: *deployment, Release: release, ExpectCurrent: expected, Service: service, AcknowledgeOperations: acknowledged})
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
