package agent

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type DockerManager struct {
	bin string
}

func NewDockerManager() *DockerManager {
	return &DockerManager{bin: "docker"}
}

func (d *DockerManager) Available() error {
	out, err := exec.Command(d.bin, "info").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker not available: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *DockerManager) Build(ctx context.Context, contextDir string, tag string, onOutput func(string)) error {
	cmd := exec.CommandContext(ctx, d.bin, "build", "-t", tag, contextDir)
	return streamCommand(cmd, onOutput)
}

func (d *DockerManager) RunBuildCommand(ctx context.Context, workspace string, command string, env map[string]string, onOutput func(string)) error {
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}

	args := []string{
		"run", "--rm",
		"-v", absWorkspace + ":/app",
		"-w", "/app",
		"-e", "COMPOSER_ALLOW_SUPERUSER=1",
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-e", key+"="+env[key])
	}
	args = append(args, "phpsandbox/php:latest", "sh", "-lc", command)

	cmd := exec.CommandContext(ctx, d.bin, args...)
	return streamCommand(cmd, onOutput)
}

func (d *DockerManager) Run(ctx context.Context, opts RunOptions) (string, error) {
	args := []string{"run", "-d", "--restart=unless-stopped",
		"--name", opts.Name,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", opts.HostPort, opts.ContainerPort),
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, opts.Image)
	args = append(args, opts.Command...)

	out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (d *DockerManager) Stop(ctx context.Context, containerID string) error {
	out, err := exec.CommandContext(ctx, d.bin, "stop", containerID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *DockerManager) Remove(ctx context.Context, containerID string) error {
	out, err := exec.CommandContext(ctx, d.bin, "rm", "-f", containerID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *DockerManager) Inspect(ctx context.Context, containerID string) (bool, error) {
	err := exec.CommandContext(ctx, d.bin, "inspect", containerID).Run()
	return err == nil, nil
}

func (d *DockerManager) Logs(ctx context.Context, containerID string, tail int) (string, error) {
	out, err := exec.CommandContext(ctx, d.bin, "logs", "--tail", strconv.Itoa(tail), containerID).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker logs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (d *DockerManager) WaitHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, _ := d.Inspect(ctx, containerID)
		if running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("container %s did not become healthy within %v", containerID, timeout)
}

type RunOptions struct {
	Name          string
	Image         string
	Command       []string
	HostPort      int
	ContainerPort int
	Env           map[string]string
}

func streamCommand(cmd *exec.Cmd, onOutput func(string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return err
	}

	buf := make([]byte, 4096)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 && onOutput != nil {
			onOutput(string(buf[:n]))
		}
		if readErr != nil {
			break
		}
	}
	return cmd.Wait()
}
