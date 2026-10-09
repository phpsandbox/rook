package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var resourceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

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
