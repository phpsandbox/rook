package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResourceStateRejectsPlansThatAbandonRetainedInventory(t *testing.T) {
	for _, omitted := range []string{"mount", "volume", "service"} {
		t.Run(omitted, func(t *testing.T) {
			store := resourceStateStore{stateDir: t.TempDir(), key: "project"}
			plan := *testExecution(t, "services", "project", "").Execution
			if err := store.save(plan); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(store.directory(), "execution.json"))
			if err != nil {
				t.Fatal(err)
			}
			var doc ComposeDocument
			if err := json.Unmarshal(plan.Compose, &doc); err != nil {
				t.Fatal(err)
			}
			switch omitted {
			case "mount":
				plan.Runtime.Mounts = nil
			case "volume":
				delete(doc.Volumes, "cache")
			case "service":
				delete(doc.Services, "cache")
			}
			plan.Compose, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.save(plan); err == nil {
				t.Fatal("plan abandoned a retained resource")
			}
			after, err := os.ReadFile(filepath.Join(store.directory(), "execution.json"))
			if err != nil || string(after) != string(before) {
				t.Fatal("rejected replacement damaged cleanup state", err)
			}
		})
	}
}

func TestResourceStateAcceptsEquivalentDependencyMaps(t *testing.T) {
	store := resourceStateStore{stateDir: t.TempDir(), key: "project"}
	plan := *testExecution(t, "services", "project", "").Execution
	if err := store.save(plan); err != nil {
		t.Fatal(err)
	}
	plan.Dependencies = map[string]string{}
	if err := store.save(plan); err != nil {
		t.Fatal("empty dependency map changed the resource set", err)
	}
	info, err := os.Stat(filepath.Join(store.directory(), "execution.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("cleanup state is not private", err)
	}
}

func TestResourceCleanupPreservesStateOnDockerFailure(t *testing.T) {
	store := resourceStateStore{stateDir: t.TempDir(), key: "project"}
	plan := ResourceExecution{Key: "project", Project: "host-project", Compose: json.RawMessage(`{}`), Runtime: ResourceRuntime{Mounts: map[string]string{"files": "/files"}}}
	if err := store.save(plan); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'Cannot connect to Docker daemon' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		manager := &DockerManager{bin: bin}
		err := manager.DeleteResources(ctx, "project", store.stateDir)
		cancel()
		if err == nil {
			t.Fatal("Docker failure was reported as successful cleanup")
		}
		if _, err := store.load(); err != nil {
			t.Fatal("cleanup instructions lost after Docker failure", err)
		}
	}
}
