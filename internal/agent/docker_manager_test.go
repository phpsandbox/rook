package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPassesEnvironmentAsBuildKitSecrets(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	envPath := filepath.Join(dir, "env")
	dockerPath := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_PATH\"\nprintf '%s' \"$COMPOSER_AUTH\" > \"$ENV_PATH\"\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGS_PATH", argsPath)
	t.Setenv("ENV_PATH", envPath)

	manager := &DockerManager{bin: dockerPath}
	if err := manager.Build(context.Background(), dir, "deployment:latest", map[string]string{
		"COMPOSER_AUTH": `{"token":"secret"}`,
	}, nil); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "id=COMPOSER_AUTH,env=COMPOSER_AUTH") {
		t.Fatalf("build args = %s", args)
	}
	value, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != `{"token":"secret"}` {
		t.Fatalf("build environment value = %q", value)
	}
}
