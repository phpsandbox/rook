package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
)

const ResourceExecutionCapability = "resource-execution.v1"

var resourceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var secretNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{0,63}$`)
var secretReferencePattern = regexp.MustCompile(`\{\{secret:([a-z0-9-]+):([a-zA-Z0-9]+)\}\}`)

func resourceKey(plan *ResourceExecution) string {
	if plan == nil {
		return ""
	}
	return plan.Key
}

func validateResourceExecution(plan *ResourceExecution) error {
	if plan == nil {
		return nil
	}
	if !resourceKeyPattern.MatchString(plan.Key) || !strings.HasPrefix(plan.Project, "rook-") || !strings.HasSuffix(plan.Project, "-"+plan.Key) || !resourceKeyPattern.MatchString(plan.Project) {
		return fmt.Errorf("invalid resource identity")
	}
	for key, project := range plan.Dependencies {
		if key == plan.Key || !resourceKeyPattern.MatchString(key) || !strings.HasPrefix(project, "rook-") || !strings.HasSuffix(project, "-"+key) || !resourceKeyPattern.MatchString(project) {
			return fmt.Errorf("invalid resource dependency")
		}
	}
	for _, name := range plan.Secrets {
		if !secretNamePattern.MatchString(name) {
			return fmt.Errorf("invalid secret name")
		}
	}
	var doc ComposeDocument
	if err := json.Unmarshal(plan.Compose, &doc); err != nil {
		return fmt.Errorf("invalid Compose document: %w", err)
	}
	for _, network := range doc.Networks {
		if network.Name != plan.Project {
			return fmt.Errorf("network is outside the resource scope")
		}
	}
	if plan.Runtime.Network != plan.Project {
		return fmt.Errorf("runtime network is outside the resource scope")
	}
	for _, volume := range doc.Volumes {
		if !strings.HasPrefix(volume.Name, plan.Project+"-") || !resourceKeyPattern.MatchString(volume.Name) {
			return fmt.Errorf("volume is outside the resource scope")
		}
	}
	for volume, target := range plan.Runtime.Mounts {
		if !strings.HasPrefix(volume, plan.Project+"-") || !resourceKeyPattern.MatchString(volume) || !filepath.IsAbs(target) || filepath.Clean(target) != target {
			return fmt.Errorf("invalid resource mount")
		}
	}
	for _, operations := range [][]ResourceOperation{plan.Prepare, plan.Cleanup} {
		for _, op := range operations {
			project := plan.Project
			if op.Scope != plan.Key {
				var ok bool
				project, ok = plan.Dependencies[op.Scope]
				if !ok {
					return fmt.Errorf("operation references an undeclared resource")
				}
			}
			if !strings.HasPrefix(op.Container, project+"-") || !resourceKeyPattern.MatchString(op.Container) {
				return fmt.Errorf("container is outside the resource scope")
			}
			switch op.Action {
			case "exec":
				if len(op.Command) == 0 {
					return fmt.Errorf("container operation requires a command")
				}
			case "connect", "disconnect":
				if op.Network != plan.Project {
					return fmt.Errorf("operation network is outside the resource scope")
				}
			default:
				return fmt.Errorf("unsupported resource operation %q", op.Action)
			}
		}
	}
	return nil
}

func loadSecrets(dir string, names []string) (map[string]string, error) {
	path := filepath.Join(dir, "credentials.json")
	secrets := map[string]string{}
	content, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(content, &secrets); err != nil {
			return nil, err
		}
		if secrets == nil {
			return nil, fmt.Errorf("invalid local secret store")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	changed := false
	for _, name := range names {
		if secrets[name] != "" {
			continue
		}
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return nil, err
		}
		secrets[name] = hex.EncodeToString(value)
		changed = true
	}
	if changed {
		content, err := json.Marshal(secrets)
		if err != nil {
			return nil, err
		}
		// Atomic replacement keeps the previous credential file intact after an interrupted write.
		temp, err := os.CreateTemp(dir, "credentials-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(temp.Name())
		if _, err := temp.Write(content); err != nil {
			temp.Close()
			return nil, err
		}
		if err := temp.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(temp.Name(), path); err != nil {
			return nil, err
		}
	}
	return secrets, nil
}

func resolveExecution(plan ResourceExecution, stateDir string) (ResourceExecution, error) {
	content, err := json.Marshal(plan)
	if err != nil {
		return plan, err
	}
	var resolveError error
	content = secretReferencePattern.ReplaceAllFunc(content, func(reference []byte) []byte {
		parts := secretReferencePattern.FindSubmatch(reference)
		scope, name := string(parts[1]), string(parts[2])
		if scope != plan.Key {
			if _, ok := plan.Dependencies[scope]; !ok {
				resolveError = fmt.Errorf("secret references an undeclared resource")
				return reference
			}
		}
		secrets, err := loadSecrets(filepath.Join(stateDir, "resources", scope), nil)
		if err != nil {
			resolveError = err
			return reference
		}
		value, ok := secrets[name]
		if !ok || value == "" {
			resolveError = fmt.Errorf("required local secret is unavailable")
			return reference
		}
		encoded, _ := json.Marshal(value)
		return encoded[1 : len(encoded)-1]
	})
	if resolveError != nil {
		return plan, resolveError
	}
	var resolved ResourceExecution
	if err := json.Unmarshal(content, &resolved); err != nil {
		return plan, err
	}
	return resolved, nil
}

func (d *DockerManager) PrepareResources(ctx context.Context, plan Plan, stateDir string) (ResourceRuntime, error) {
	runtime := ResourceRuntime{Env: map[string]string{}, Mounts: map[string]string{}}
	if plan.Execution == nil {
		return runtime, nil
	}
	d.resourcesMu.Lock()
	defer d.resourcesMu.Unlock()
	execution := *plan.Execution
	if err := validateResourceExecution(&execution); err != nil {
		return runtime, err
	}
	dir := filepath.Join(stateDir, "resources", execution.Key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return runtime, err
	}
	path := filepath.Join(dir, "execution.json")
	if content, err := os.ReadFile(path); err == nil {
		var previous ResourceExecution
		if err := json.Unmarshal(content, &previous); err != nil {
			return runtime, err
		}
		if previous.Project != execution.Project || !reflect.DeepEqual(previous.Dependencies, execution.Dependencies) {
			return runtime, fmt.Errorf("retained resource dependencies changed; clean up the previous allocation first")
		}
	} else if !os.IsNotExist(err) {
		return runtime, err
	}
	if _, err := loadSecrets(dir, execution.Secrets); err != nil {
		return runtime, err
	}
	content, err := json.Marshal(execution)
	if err != nil {
		return runtime, err
	}
	// Save the unresolved cleanup plan before executing any operation, including partial failures.
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return runtime, err
	}
	resolved, err := resolveExecution(execution, stateDir)
	if err != nil {
		return runtime, err
	}
	composePath := filepath.Join(dir, "compose.json")
	if err := os.WriteFile(composePath, resolved.Compose, 0o600); err != nil {
		return runtime, err
	}
	var doc ComposeDocument
	if err := json.Unmarshal(resolved.Compose, &doc); err != nil {
		return runtime, err
	}
	if len(doc.Services) > 0 {
		if err := d.compose(ctx, resolved.Project, composePath, "up", "-d", "--wait", "--wait-timeout", "120"); err != nil {
			return runtime, err
		}
	} else if _, err := exec.CommandContext(ctx, d.bin, "network", "inspect", resolved.Runtime.Network).Output(); err != nil {
		if err := exec.CommandContext(ctx, d.bin, "network", "create", resolved.Runtime.Network).Run(); err != nil {
			return runtime, fmt.Errorf("create resource network: %w", err)
		}
	}
	for volume := range resolved.Runtime.Mounts {
		if err := exec.CommandContext(ctx, d.bin, "volume", "create", volume).Run(); err != nil {
			return runtime, err
		}
	}
	if err := d.resourceOperations(ctx, resolved, resolved.Prepare); err != nil {
		return runtime, err
	}
	return resolved.Runtime, nil
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

func (d *DockerManager) DeleteResources(ctx context.Context, key, stateDir string) error {
	if key == "" {
		return nil
	}
	if !resourceKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid resource key")
	}
	d.resourcesMu.Lock()
	defer d.resourcesMu.Unlock()
	dir := filepath.Join(stateDir, "resources", key)
	content, err := os.ReadFile(filepath.Join(dir, "execution.json"))
	if os.IsNotExist(err) {
		if _, statError := os.Stat(dir); statError == nil {
			return fmt.Errorf("retained resources have no saved execution plan; republish before deleting them")
		} else if !os.IsNotExist(statError) {
			return statError
		}
		return nil
	}
	if err != nil {
		return err
	}
	var plan ResourceExecution
	if err := json.Unmarshal(content, &plan); err != nil {
		return err
	}
	if plan.Key != key {
		return fmt.Errorf("resource identity differs from cleanup request")
	}
	if err := validateResourceExecution(&plan); err != nil {
		return err
	}
	plan, err = resolveExecution(plan, stateDir)
	if err != nil {
		return err
	}
	if err := d.resourceOperations(ctx, plan, plan.Cleanup); err != nil {
		return err
	}
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
	return os.RemoveAll(dir)
}

func mergeResourceEnvironment(production, resources map[string]string) map[string]string {
	result := make(map[string]string, len(production)+len(resources))
	for key, value := range production {
		result[key] = value
	}
	for key, value := range resources {
		result[key] = value
	}
	return result
}
