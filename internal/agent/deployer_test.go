package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type fakeDockerClient struct {
	events        []string
	runID         string
	waitHealthyFn func(containerID string) error
}

func (f *fakeDockerClient) Build(_ context.Context, _ string, tag string, _ func(string)) error {
	f.events = append(f.events, "build:"+tag)
	return nil
}

func (f *fakeDockerClient) RunBuildCommand(_ context.Context, _ string, command string, _ map[string]string, _ func(string)) error {
	f.events = append(f.events, "build-command:"+command)
	return nil
}

func (f *fakeDockerClient) Run(_ context.Context, opts RunOptions) (string, error) {
	id := f.runID
	if id == "" {
		id = "new-container"
	}
	f.events = append(f.events, "run:"+opts.Name)
	return id, nil
}

func (f *fakeDockerClient) Stop(_ context.Context, containerID string) error {
	f.events = append(f.events, "stop:"+containerID)
	return nil
}

func (f *fakeDockerClient) Remove(_ context.Context, containerID string) error {
	f.events = append(f.events, "remove:"+containerID)
	return nil
}

func (f *fakeDockerClient) Inspect(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (f *fakeDockerClient) Logs(_ context.Context, _ string, _ int) (string, error) {
	return "", nil
}

func (f *fakeDockerClient) WaitHealthy(_ context.Context, containerID string, _ time.Duration) error {
	f.events = append(f.events, "healthy:"+containerID)
	if f.waitHealthyFn != nil {
		return f.waitHealthyFn(containerID)
	}
	return nil
}

func TestDeployerRedeployCutsOverAfterNewContainerIsHealthy(t *testing.T) {
	state := NewStateStore(t.TempDir())
	if err := state.Set("deploy-1", DeploymentState{ContainerID: "old-container", Port: PortRangeStart}); err != nil {
		t.Fatal(err)
	}

	docker := &fakeDockerClient{runID: "new-container"}
	deployer := NewDeployer(docker, state)

	if err := deployer.Deploy(context.Background(), deployPayload(t, "deploy-1"), func(OutboundMessage) {}); err != nil {
		t.Fatal(err)
	}

	healthyIndex := slices.Index(docker.events, "healthy:new-container")
	stopIndex := slices.Index(docker.events, "stop:old-container")
	if healthyIndex == -1 || stopIndex == -1 {
		t.Fatalf("missing expected events: %#v", docker.events)
	}
	if stopIndex < healthyIndex {
		t.Fatalf("old container stopped before new healthy: %#v", docker.events)
	}

	current, ok := state.Get("deploy-1")
	if !ok {
		t.Fatal("deployment state missing")
	}
	if current.ContainerID != "new-container" {
		t.Fatalf("state container = %q, want new-container", current.ContainerID)
	}
	if current.Port == PortRangeStart {
		t.Fatalf("redeploy reused old port %d before cutover", current.Port)
	}
}

func TestDeployerRedeployKeepsOldStateWhenNewContainerFailsHealth(t *testing.T) {
	state := NewStateStore(t.TempDir())
	if err := state.Set("deploy-1", DeploymentState{ContainerID: "old-container", Port: PortRangeStart}); err != nil {
		t.Fatal(err)
	}

	docker := &fakeDockerClient{
		runID: "new-container",
		waitHealthyFn: func(string) error {
			return errors.New("not healthy")
		},
	}
	deployer := NewDeployer(docker, state)

	if err := deployer.Deploy(context.Background(), deployPayload(t, "deploy-1"), func(OutboundMessage) {}); err == nil {
		t.Fatal("expected health error")
	}

	if slices.Contains(docker.events, "stop:old-container") {
		t.Fatalf("old container was stopped on failed cutover: %#v", docker.events)
	}
	if !slices.Contains(docker.events, "remove:new-container") {
		t.Fatalf("failed new container was not removed: %#v", docker.events)
	}

	current, ok := state.Get("deploy-1")
	if !ok {
		t.Fatal("deployment state missing")
	}
	if current.ContainerID != "old-container" {
		t.Fatalf("state container = %q, want old-container", current.ContainerID)
	}
}

func deployPayload(t *testing.T, deploymentID string) DeployPayload {
	t.Helper()

	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := json.Marshal(SourceRef{
		Provider: SourceProviderPath,
		Path:     sourceDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := json.Marshal(Plan{
		Runtime: RuntimePlan{
			Port: 8080,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return DeployPayload{
		DeploymentID: deploymentID,
		Source:       source,
		Plan:         plan,
		Env:          map[string]string{},
	}
}
