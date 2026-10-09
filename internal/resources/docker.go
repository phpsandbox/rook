package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (d *Executor) startResourceContainers(ctx context.Context, plan Plan, dir string) error {
	composePath := filepath.Join(dir, "compose.json")
	if err := writePrivateFile(composePath, plan.Compose); err != nil {
		return err
	}
	var doc struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(plan.Compose, &doc); err != nil {
		return err
	}
	if len(doc.Services) > 0 {
		if err := d.compose(ctx, plan.Project, composePath, "up", "-d", "--wait", "--wait-timeout", "120"); err != nil {
			return err
		}
	} else {
		exists, err := d.resourceObjectExists(ctx, "network", plan.Runtime.Network)
		if err != nil {
			return err
		}
		if !exists {
			if err := exec.CommandContext(ctx, d.DockerBin, "network", "create", plan.Runtime.Network).Run(); err != nil {
				return fmt.Errorf("create resource network: %w", err)
			}
		}
	}
	for volume := range plan.Runtime.Mounts {
		if err := exec.CommandContext(ctx, d.DockerBin, "volume", "create", volume).Run(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Executor) compose(ctx context.Context, project, path string, args ...string) error {
	commandArgs := append([]string{"compose", "--project-name", project, "--file", path}, args...)
	if err := exec.CommandContext(ctx, d.DockerBin, commandArgs...).Run(); err != nil {
		return fmt.Errorf("resource Compose operation failed: %w", err)
	}
	return nil
}

func (d *Executor) resourceOperations(ctx context.Context, plan Plan, operations []Operation) error {
	for _, op := range operations {
		project := plan.Project
		if op.Scope != plan.Key {
			project = plan.Dependencies[op.Scope]
		}
		out, err := exec.CommandContext(ctx, d.DockerBin, "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, op.Container).Output()
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
			command := exec.CommandContext(ctx, d.DockerBin, args...)
			command.Env = env
			command.Stdin = strings.NewReader(op.Input)
			if err := command.Run(); err != nil {
				return fmt.Errorf("resource container operation failed: %w", err)
			}
		} else {
			out, err := exec.CommandContext(ctx, d.DockerBin, "inspect", "--format", "{{json .NetworkSettings.Networks}}", op.Container).Output()
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
			if err := exec.CommandContext(ctx, d.DockerBin, args...).Run(); err != nil {
				return fmt.Errorf("resource network operation failed: %w", err)
			}
		}
	}
	return nil
}

func (d *Executor) resourceObjectExists(ctx context.Context, kind, name string) (bool, error) {
	out, err := exec.CommandContext(ctx, d.DockerBin, kind, "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		return false, fmt.Errorf("list resource %s objects: %w", kind, err)
	}
	for _, existing := range strings.Fields(string(out)) {
		if existing == name {
			return true, nil
		}
	}
	return false, nil
}
