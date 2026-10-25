package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phpsandbox/rook/internal/host"
)

func TestReleasesCommandsReadLocalStateWithoutAgentConfiguration(t *testing.T) {
	directory := t.TempDir()
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	metadata := json.RawMessage(`{"releaseId":"release-a","containerId":"` + id + `","port":3000,"healthPath":"/_health"}`)
	bindings := host.NewBindings(directory)
	if err := bindings.Set("app", host.Binding{Port: 3000, Metadata: metadata}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "releases", "app")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "release-a.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"list", "--state-dir", directory, "--deployment", "app"},
		{"inspect", "release-a", "--state-dir", directory, "--deployment", "app"},
		{"inspect", "--state-dir", directory, "--deployment", "app", "release-a"},
	} {
		t.Run(strings.Join(args[:1], ""), func(t *testing.T) {
			var output, diagnostics bytes.Buffer

			if err := runReleases(context.Background(), args, &output, &diagnostics); err != nil {
				t.Fatal(err)
			}

			var inventory host.ReleaseInventory
			if err := json.Unmarshal(output.Bytes(), &inventory); err != nil {
				t.Fatal(err)
			}
			if inventory.CurrentReleaseID != "release-a" || len(inventory.Releases) != 1 || inventory.Releases[0].ContainerID != id {
				t.Fatalf("inventory=%+v", inventory)
			}
			if diagnostics.Len() != 0 {
				t.Fatal(diagnostics.String())
			}
		})
	}
}

func TestReleasesCLIRejectsInvalidArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"missing command":          {},
		"unknown command":          {"delete"},
		"missing state":            {"list", "--deployment", "app"},
		"missing deployment":       {"list", "--state-dir", "/tmp"},
		"list with release":        {"list", "--state-dir", "/tmp", "--deployment", "app", "release-a"},
		"missing release":          {"inspect", "--state-dir", "/tmp", "--deployment", "app"},
		"unknown flag":             {"restore", "release-a", "--force"},
		"missing expected release": {"restore", "release-a", "--state-dir", "/tmp", "--deployment", "app", "--rook-service", "rook.service"},
	} {
		t.Run(name, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			err := runReleases(context.Background(), args, &output, &diagnostics)
			if err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if output.Len() != 0 {
				t.Fatal("invalid command wrote a result")
			}
		})
	}
}

func TestReleasesHelpDoesNotReadState(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"restore", "--help"}} {
		var output, diagnostics bytes.Buffer

		err := runReleases(context.Background(), args, &output, &diagnostics)

		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("error=%v", err)
		}
		if diagnostics.Len() == 0 || output.Len() != 0 {
			t.Fatal("invalid help output")
		}
	}
}
