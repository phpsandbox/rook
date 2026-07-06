package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sourceContextDir = "app"
const deployManifestSchemaVersion = 1

type Deployer struct {
	docker DockerClient
	state  *StateStore
}

type DockerClient interface {
	Build(ctx context.Context, contextDir string, tag string, onOutput func(string)) error
	RunBuildCommand(ctx context.Context, workspace string, image string, command string, env map[string]string, onOutput func(string)) error
	Run(ctx context.Context, opts RunOptions) (string, error)
	Stop(ctx context.Context, containerID string) error
	Remove(ctx context.Context, containerID string) error
	Inspect(ctx context.Context, containerID string) (bool, error)
	Logs(ctx context.Context, containerID string, tail int) (string, error)
	WaitHealthy(ctx context.Context, containerID string, hostPort int, healthPath string, timeout time.Duration) error
}

func NewDeployer(docker DockerClient, state *StateStore) *Deployer {
	return &Deployer{docker: docker, state: state}
}

func (d *Deployer) Deploy(ctx context.Context, payload DeployPayload, send func(OutboundMessage)) error {
	if payload.Manifest.SchemaVersion != deployManifestSchemaVersion {
		return fmt.Errorf("deploy payload requires manifest.schemaVersion %d", deployManifestSchemaVersion)
	}
	if strings.TrimSpace(payload.Plan.Build.Image) == "" {
		return fmt.Errorf("deploy payload requires build.image")
	}
	if payload.Plan.Runtime.Port <= 0 {
		return fmt.Errorf("deploy payload requires runtime.port")
	}
	if strings.TrimSpace(payload.Plan.Runtime.HealthPath) == "" {
		return fmt.Errorf("deploy payload requires runtime.healthPath")
	}

	commandID := payload.DeploymentID
	emitLog := func(stream, content string) {
		send(OutboundMessage{
			Type:      "log",
			CommandID: commandID,
			Stream:    stream,
			Content:   content,
		})
	}
	emitPhase := func(name string, data *DeployPhaseData) {
		send(OutboundMessage{
			Type:      "phase",
			CommandID: commandID,
			Phase:     name,
			Data:      data,
		})
	}

	emitPhase("build.started", nil)

	workDir := filepath.Join(os.TempDir(), "rook", payload.DeploymentID)
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("clean work dir: %w", err)
	}
	sourceDir := filepath.Join(workDir, "source")
	contextDir := filepath.Join(workDir, "context")
	if payload.Manifest.Build.KeepWorkspace {
		emitLog("build", "Keeping build workspace at "+workDir)
	} else {
		defer os.RemoveAll(workDir)
	}

	buildContextDir := sourceDir
	if payload.Bundle != nil {
		if payload.Bundle.Layout != DeployBundleLayoutDockerContextV1 {
			return fmt.Errorf("unsupported deploy bundle layout %q", payload.Bundle.Layout)
		}
		emitLog("build", "Applying deployment bundle...")
		if err := os.MkdirAll(contextDir, 0o755); err != nil {
			return fmt.Errorf("create build context dir: %w", err)
		}
		if err := ApplyDeployBundle(contextDir, *payload.Bundle); err != nil {
			return fmt.Errorf("apply deployment bundle: %w", err)
		}
		if exists(filepath.Join(contextDir, sourceContextDir)) {
			return fmt.Errorf("deploy bundle uses reserved build context path %q", sourceContextDir)
		}
		sourceDir = filepath.Join(contextDir, sourceContextDir)
		buildContextDir = contextDir
	} else if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return fmt.Errorf("create source dir: %w", err)
	}

	emitLog("build", "Cloning source...")
	if err := d.cloneSource(ctx, payload.Source, sourceDir); err != nil {
		return fmt.Errorf("clone source: %w", err)
	}

	if len(payload.Plan.Build.Commands) > 0 {
		emitLog("build", "Running build commands...")
		for _, cmd := range payload.Plan.Build.Commands {
			emitLog("build", "$ "+cmd)
			if err := d.runBuildCommand(ctx, sourceDir, payload.Plan.Build.Image, cmd, func(line string) {
				emitLog("build", line)
			}, payload.Env); err != nil {
				return fmt.Errorf("build command %q: %w", cmd, err)
			}
		}
	}

	imageTag := fmt.Sprintf("okra-%s:%s", payload.DeploymentID, time.Now().Format("20060102-150405"))
	emitLog("build", "Building Docker image...")
	if err := d.docker.Build(ctx, buildContextDir, imageTag, func(line string) {
		emitLog("build", line)
	}); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}

	emitPhase("build.completed", nil)
	emitPhase("deploy.started", nil)

	port, err := d.allocatePort()
	if err != nil {
		return fmt.Errorf("allocate port: %w", err)
	}

	containerName := fmt.Sprintf("rook-%s-%s", payload.DeploymentID, time.Now().UTC().Format("20060102-150405"))
	existing, hasExisting := d.state.Get(payload.DeploymentID)

	emitLog("deploy", "Starting container...")
	containerID, err := d.docker.Run(ctx, RunOptions{
		Name:          containerName,
		Image:         imageTag,
		Command:       payload.Plan.Runtime.Command,
		HostPort:      port,
		ContainerPort: payload.Plan.Runtime.Port,
		Env:           payload.Env,
	})
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	emitLog("deploy", "Waiting for container to become healthy...")
	if err := d.docker.WaitHealthy(ctx, containerID, port, payload.Plan.Runtime.HealthPath, 60*time.Second); err != nil {
		_ = d.docker.Remove(ctx, containerID)
		return fmt.Errorf("container health check: %w", err)
	}

	routeKey := fmt.Sprintf("deploy--%s", payload.DeploymentID)
	if err := d.state.Set(payload.DeploymentID, DeploymentState{
		ContainerID: containerID,
		Port:        port,
		ImageRef:    imageTag,
		RouteKey:    routeKey,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		_ = d.docker.Remove(ctx, containerID)
		return fmt.Errorf("save state: %w", err)
	}

	if hasExisting {
		emitLog("deploy", "Retiring previous container...")
		_ = d.docker.Stop(ctx, existing.ContainerID)
		_ = d.docker.Remove(ctx, existing.ContainerID)
	}

	emitPhase("deploy.completed", &DeployPhaseData{Port: port, ContainerID: containerID})
	return nil
}

func (d *Deployer) Stop(ctx context.Context, deploymentID string) error {
	state, ok := d.state.Get(deploymentID)
	if !ok {
		return fmt.Errorf("deployment %s not found", deploymentID)
	}
	if err := d.docker.Stop(ctx, state.ContainerID); err != nil {
		return err
	}
	return d.state.Remove(deploymentID)
}

func (d *Deployer) Delete(ctx context.Context, deploymentID string) error {
	state, ok := d.state.Get(deploymentID)
	if !ok {
		return fmt.Errorf("deployment %s not found", deploymentID)
	}
	_ = d.docker.Stop(ctx, state.ContainerID)
	if err := d.docker.Remove(ctx, state.ContainerID); err != nil {
		return err
	}
	return d.state.Remove(deploymentID)
}

func (d *Deployer) TailLogs(ctx context.Context, deploymentID string, lines int) (string, error) {
	state, ok := d.state.Get(deploymentID)
	if !ok {
		return "", fmt.Errorf("deployment %s not found", deploymentID)
	}
	return d.docker.Logs(ctx, state.ContainerID, lines)
}

func (d *Deployer) cloneSource(ctx context.Context, source SourceRef, dest string) error {
	return PrepareSource(ctx, source, dest)
}

func (d *Deployer) runBuildCommand(ctx context.Context, dir string, image string, command string, onOutput func(string), env map[string]string) error {
	return d.docker.RunBuildCommand(ctx, dir, image, command, env, onOutput)
}

func (d *Deployer) allocatePort() (int, error) {
	used := map[int]bool{}
	for _, state := range d.state.All() {
		used[state.Port] = true
	}
	for port := PortRangeStart; port <= PortRangeEnd; port++ {
		if !used[port] && hostPortAvailable(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available ports in range %d-%d", PortRangeStart, PortRangeEnd)
}

func hostPortAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
