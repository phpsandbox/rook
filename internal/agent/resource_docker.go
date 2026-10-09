package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (d *DockerManager) startResourceContainers(ctx context.Context, plan ResourceExecution, dir string) error {
	composePath := filepath.Join(dir, "compose.json")
	if err := os.WriteFile(composePath, plan.Compose, 0o600); err != nil {
		return err
	}
	var doc ComposeDocument
	if err := json.Unmarshal(plan.Compose, &doc); err != nil {
		return err
	}
	if len(doc.Services) > 0 {
		if err := d.compose(ctx, plan.Project, composePath, "up", "-d", "--wait", "--wait-timeout", "120"); err != nil {
			return err
		}
	} else if _, err := exec.CommandContext(ctx, d.bin, "network", "inspect", plan.Runtime.Network).Output(); err != nil {
		if err := exec.CommandContext(ctx, d.bin, "network", "create", plan.Runtime.Network).Run(); err != nil {
			return fmt.Errorf("create resource network: %w", err)
		}
	}
	for volume := range plan.Runtime.Mounts {
		if err := exec.CommandContext(ctx, d.bin, "volume", "create", volume).Run(); err != nil {
			return err
		}
	}
	return nil
}

func (d *DockerManager) compose(ctx context.Context, project, path string, args ...string) error {
	commandArgs := append([]string{"compose", "--project-name", project, "--file", path}, args...)
	if err := exec.CommandContext(ctx, d.bin, commandArgs...).Run(); err != nil {
		return fmt.Errorf("resource Compose operation failed: %w", err)
	}
	return nil
}

func (d *DockerManager) resourceOperations(ctx context.Context, plan ResourceExecution, operations []ResourceOperation) error {
	for _, op := range operations {
		project := plan.Project
		if op.Scope != plan.Key {
			project = plan.Dependencies[op.Scope]
		}
		out, err := exec.CommandContext(ctx, d.bin, "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, op.Container).Output()
		if err != nil || strings.TrimSpace(string(out)) != project {
			return fmt.Errorf("resource container is unavailable or outside its managed scope")
		}
		if op.Action == "exec" {
			args := []string{"exec", "-i"}
			env := os.Environ()
			for name, value := range op.Env {
				args = append(args, "-e", name)
				env = append(env, name+"="+value)
			}
			args = append(args, op.Container)
			args = append(args, op.Command...)
			command := exec.CommandContext(ctx, d.bin, args...)
			command.Env = env
			command.Stdin = strings.NewReader(op.Input)
			if err := command.Run(); err != nil {
				return fmt.Errorf("resource container operation failed: %w", err)
			}
		} else {
			out, err := exec.CommandContext(ctx, d.bin, "inspect", "--format", "{{json .NetworkSettings.Networks}}", op.Container).Output()
			if err != nil {
				return err
			}
			var networks map[string]json.RawMessage
			if err := json.Unmarshal(out, &networks); err != nil {
				return err
			}
			_, connected := networks[op.Network]
			if (op.Action == "connect" && connected) || (op.Action == "disconnect" && !connected) {
				continue
			}
			args := []string{"network", op.Action}
			if op.Action == "connect" && op.Alias != "" {
				args = append(args, "--alias", op.Alias)
			}
			args = append(args, op.Network, op.Container)
			if err := exec.CommandContext(ctx, d.bin, args...).Run(); err != nil {
				return fmt.Errorf("resource network operation failed: %w", err)
			}
		}
	}
	return nil
}

func (d *DockerManager) removeResourceContainers(ctx context.Context, plan ResourceExecution, dir string) error {
	var doc ComposeDocument
	if err := json.Unmarshal(plan.Compose, &doc); err != nil {
		return err
	}
	if len(doc.Services) > 0 {
		if err := d.compose(ctx, plan.Project, filepath.Join(dir, "compose.json"), "down", "--volumes", "--remove-orphans"); err != nil {
			return err
		}
	}
	volumes := map[string]bool{}
	for _, volume := range doc.Volumes {
		volumes[volume.Name] = true
	}
	for volume := range plan.Runtime.Mounts {
		volumes[volume] = true
	}
	for volume := range volumes {
		if err := exec.CommandContext(ctx, d.bin, "volume", "inspect", volume).Run(); err == nil {
			if err := exec.CommandContext(ctx, d.bin, "volume", "rm", volume).Run(); err != nil {
				return err
			}
		}
	}
	if err := exec.CommandContext(ctx, d.bin, "network", "inspect", plan.Project).Run(); err == nil {
		if err := exec.CommandContext(ctx, d.bin, "network", "rm", plan.Project).Run(); err != nil {
			return err
		}
	}
	return nil
}
