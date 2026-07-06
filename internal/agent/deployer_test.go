package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type fakeDockerClient struct {
	events                 []string
	buildContext           string
	buildCommandWorkspaces []string
	buildCommandImages     []string
	runID                  string
	waitHealthyHostPort    int
	waitHealthyPath        string
	waitHealthyFn          func(containerID string) error
}

func (f *fakeDockerClient) Build(_ context.Context, contextDir string, tag string, _ func(string)) error {
	f.buildContext = contextDir
	f.events = append(f.events, "build:"+tag)
	return nil
}

func (f *fakeDockerClient) RunBuildCommand(_ context.Context, workspace string, image string, command string, _ map[string]string, _ func(string)) error {
	f.buildCommandWorkspaces = append(f.buildCommandWorkspaces, workspace)
	f.buildCommandImages = append(f.buildCommandImages, image)
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

func (f *fakeDockerClient) WaitHealthy(_ context.Context, containerID string, hostPort int, healthPath string, _ time.Duration) error {
	f.events = append(f.events, "healthy:"+containerID)
	f.waitHealthyHostPort = hostPort
	f.waitHealthyPath = healthPath
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

func TestDeployerRequiresBuildImage(t *testing.T) {
	state := NewStateStore(t.TempDir())
	deployer := NewDeployer(&fakeDockerClient{}, state)
	payload := deployPayload(t, "deploy-missing-build-image")
	payload.Plan.Build.Image = ""

	if err := deployer.Deploy(context.Background(), payload, func(OutboundMessage) {}); err == nil {
		t.Fatal("expected missing build image error")
	}
}

func TestDeployerRequiresManifest(t *testing.T) {
	state := NewStateStore(t.TempDir())
	deployer := NewDeployer(&fakeDockerClient{}, state)
	payload := deployPayload(t, "deploy-missing-manifest")
	payload.Manifest.SchemaVersion = 0

	if err := deployer.Deploy(context.Background(), payload, func(OutboundMessage) {}); err == nil {
		t.Fatal("expected missing manifest error")
	}
}

func TestDeployerCanKeepBuildWorkspaceForInspection(t *testing.T) {
	state := NewStateStore(t.TempDir())
	docker := &fakeDockerClient{runID: "new-container"}
	deployer := NewDeployer(docker, state)
	payload := deployPayload(t, "deploy-keep")
	payload.Manifest.Build.KeepWorkspace = true

	if err := deployer.Deploy(context.Background(), payload, func(OutboundMessage) {}); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(os.TempDir(), "rook", payload.DeploymentID)
	t.Cleanup(func() {
		_ = os.RemoveAll(workDir)
	})
	if _, err := os.Stat(filepath.Join(workDir, "source", "Dockerfile")); err != nil {
		t.Fatalf("expected kept build workspace: %v", err)
	}
	if docker.buildContext != filepath.Join(workDir, "source") {
		t.Fatalf("build context = %q", docker.buildContext)
	}
}

func TestDeployerPreparesBundleBuildContextWithoutOverlayingSource(t *testing.T) {
	state := NewStateStore(t.TempDir())
	docker := &fakeDockerClient{runID: "new-container"}
	deployer := NewDeployer(docker, state)

	sourceDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceDir, ".phpsandbox", "runtime", "laravel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "Dockerfile"), []byte("FROM user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, ".phpsandbox", "runtime", "laravel", "Caddyfile"), []byte("user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "node_modules", "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}

	bundle := testDeployBundle(t, map[string]string{
		"Dockerfile":                                   "FROM generated\nCOPY app/ /app\n",
		".dockerignore":                                "app/.git\napp/node_modules\n",
		".phpsandbox/runtime/laravel/Caddyfile":        "generated\n",
		".phpsandbox/runtime/laravel/laravel-start.sh": "#!/bin/sh\n",
	})
	bundle.Layout = DeployBundleLayoutDockerContextV1
	payload := DeployPayload{
		DeploymentID: "deploy-bundle-context",
		Source: SourceRef{
			Provider: SourceProviderPath,
			Path:     sourceDir,
		},
		Manifest: DeployManifest{
			SchemaVersion: deployManifestSchemaVersion,
			Build: DeployManifestBuild{
				KeepWorkspace: true,
			},
		},
		Plan: Plan{
			Build: BuildPlan{
				Image:    "custom-build:latest",
				Commands: []string{"npm run build"},
			},
			Runtime: RuntimePlan{
				Port:       8080,
				HealthPath: "/health",
			},
		},
		Bundle: &bundle,
		Env:    map[string]string{},
	}

	if err := deployer.Deploy(context.Background(), payload, func(OutboundMessage) {}); err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(os.TempDir(), "rook", payload.DeploymentID)
	t.Cleanup(func() {
		_ = os.RemoveAll(workDir)
	})
	if docker.buildContext != filepath.Join(workDir, "context") {
		t.Fatalf("build context = %q", docker.buildContext)
	}
	if _, err := os.Stat(filepath.Join(workDir, "source")); !os.IsNotExist(err) {
		t.Fatalf("source should be cloned directly into context/app for bundled deploys: %v", err)
	}
	if len(docker.buildCommandWorkspaces) != 1 || docker.buildCommandWorkspaces[0] != filepath.Join(docker.buildContext, "app") {
		t.Fatalf("build command workspaces = %#v", docker.buildCommandWorkspaces)
	}
	if len(docker.buildCommandImages) != 1 || docker.buildCommandImages[0] != "custom-build:latest" {
		t.Fatalf("build command images = %#v", docker.buildCommandImages)
	}
	assertFileContent(t, filepath.Join(docker.buildContext, "Dockerfile"), "FROM generated\nCOPY app/ /app\n")
	assertFileContent(t, filepath.Join(docker.buildContext, ".phpsandbox", "runtime", "laravel", "Caddyfile"), "generated\n")
	assertFileContent(t, filepath.Join(docker.buildContext, "app", "Dockerfile"), "FROM user\n")
	assertFileContent(t, filepath.Join(docker.buildContext, "app", ".phpsandbox", "runtime", "laravel", "Caddyfile"), "user\n")
	assertFileContent(t, filepath.Join(docker.buildContext, ".dockerignore"), "app/.git\napp/node_modules\n")
}

func deployPayload(t *testing.T, deploymentID string) DeployPayload {
	t.Helper()

	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return DeployPayload{
		DeploymentID: deploymentID,
		Source: SourceRef{
			Provider: SourceProviderPath,
			Path:     sourceDir,
		},
		Manifest: DeployManifest{
			SchemaVersion: deployManifestSchemaVersion,
			Build:         DeployManifestBuild{},
		},
		Plan: Plan{
			Build: BuildPlan{
				Image: "phpsandbox/php:latest",
			},
			Runtime: RuntimePlan{
				Port:       8080,
				HealthPath: "/health",
			},
		},
		Env: map[string]string{},
	}
}

func assertFileContent(t *testing.T, path string, expected string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != expected {
		t.Fatalf("%s = %q, want %q", path, content, expected)
	}
}
