package resources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResourceStateRetainsExplicitOwnershipAcrossRuntimeChanges(t *testing.T) {
	store := resourceStateStore{stateDir: t.TempDir(), key: "project"}
	plan := *testExecution(t, "services", "project", "")
	if err := store.save(plan); err != nil {
		t.Fatal(err)
	}
	before := len(plan.Ownership)
	// Operator can rename services or stop mounting a volume without losing its cleanup ownership.
	plan.Compose = json.RawMessage(`{"services":{"renamed":{"image":"redis:7-alpine"}}}`)
	plan.Runtime.Mounts = nil
	plan.Ownership = map[string]Cleanup{}
	if err := store.save(plan); err != nil {
		t.Fatal("agent inferred lifecycle policy from the new Compose document", err)
	}
	retained, err := store.load()
	if err != nil || len(retained.Ownership) != before {
		t.Fatal("omitted cleanup ownership was lost", err)
	}
}

func TestResourceStateAcceptsEquivalentDependencyMaps(t *testing.T) {
	store := resourceStateStore{stateDir: t.TempDir(), key: "project"}
	plan := *testExecution(t, "services", "project", "")
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

func TestCleanupPreservesStateOnDockerFailure(t *testing.T) {
	store := resourceStateStore{stateDir: t.TempDir(), key: "project"}
	plan := Plan{Key: "project", Project: "host-project", Compose: json.RawMessage(`{}`), Ownership: map[string]Cleanup{"volume:files": {Volume: "files"}}}
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
		manager := &Executor{DockerBin: bin, StateDir: store.stateDir}
		err := manager.Delete(ctx, "project")
		cancel()
		if err == nil {
			t.Fatal("Docker failure was reported as successful cleanup")
		}
		if _, err := store.load(); err != nil {
			t.Fatal("cleanup instructions lost after Docker failure", err)
		}
	}
}
