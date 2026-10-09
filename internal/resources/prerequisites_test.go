package resources

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestComposePrerequisitesMatchHostFeatures(t *testing.T) {
	for _, scenario := range []struct {
		name, up, down          string
		versionFails, supported bool
	}{
		{name: "supported", up: "--wait --wait-timeout int", down: "--remove-orphans", supported: true},
		{name: "no plugin", versionFails: true},
		{name: "timeout alone is not wait support", up: "--wait-timeout int", down: "--remove-orphans"},
		{name: "no timeout", up: "--wait", down: "--remove-orphans"},
		{name: "no orphan cleanup", up: "--wait --wait-timeout int"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("COMPOSE_UP_HELP", scenario.up)
			t.Setenv("COMPOSE_DOWN_HELP", scenario.down)
			if scenario.versionFails {
				t.Setenv("COMPOSE_VERSION_FAILS", "1")
			}
			bin := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\ncase \"$2\" in\nversion) test \"$COMPOSE_VERSION_FAILS\" != 1 ;;\nup) printf '%s' \"$COMPOSE_UP_HELP\" ;;\ndown) printf '%s' \"$COMPOSE_DOWN_HELP\" ;;\nesac\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			executor := &Executor{DockerBin: bin}
			if err := executor.CheckPrerequisites(context.Background()); (err == nil) != scenario.supported {
				t.Fatalf("incorrect host capability: %v", err)
			}
		})
	}
}

func TestInstalledComposeSupportsResourceExecution(t *testing.T) {
	if os.Getenv("ROOK_DOCKER_TEST") != "1" {
		t.Skip("set ROOK_DOCKER_TEST=1 to check installed Compose")
	}
	if err := (&Executor{DockerBin: "docker"}).CheckPrerequisites(context.Background()); err != nil {
		t.Fatal(err)
	}
}
