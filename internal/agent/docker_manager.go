package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

func (d *DockerManager) RunBuildCommand(ctx context.Context, workspace string, image string, command string, env map[string]string, onOutput func(string)) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("build image is required")
	}

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
	args = append(args, image, "sh", "-lc", command)

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

func (d *DockerManager) WaitHealthy(ctx context.Context, containerID string, hostPort int, healthPath string, timeout time.Duration) error {
	if hostPort <= 0 {
		return fmt.Errorf("host port is required")
	}

	healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, normalizeHealthPath(healthPath))
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		running, inspectErr := d.Inspect(ctx, containerID)
		if inspectErr != nil {
			lastErr = inspectErr
		} else if !running {
			lastErr = fmt.Errorf("container is not running")
		} else {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				return fmt.Errorf("create health request: %w", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest {
					return nil
				}
				lastErr = fmt.Errorf("health endpoint returned %d", resp.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("container %s did not become healthy within %v: %w", containerID, timeout, lastErr)
	}
	return fmt.Errorf("container %s did not become healthy within %v", containerID, timeout)
}

func normalizeHealthPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
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
