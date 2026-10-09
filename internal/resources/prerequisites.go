package resources

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

func (e *Executor) CheckPrerequisites(ctx context.Context) error {
	if err := exec.CommandContext(ctx, e.DockerBin, "compose", "version").Run(); err != nil {
		return fmt.Errorf("Docker Compose plugin is required: %w", err)
	}
	for command, required := range map[string][]string{"up": {"--wait", "--wait-timeout"}, "down": {"--remove-orphans"}} {
		out, err := exec.CommandContext(ctx, e.DockerBin, "compose", command, "--help").Output()
		if err != nil {
			return fmt.Errorf("inspect Docker Compose %s support: %w", command, err)
		}
		options := strings.Fields(string(out))
		for _, option := range required {
			if !slices.Contains(options, option) {
				return fmt.Errorf("Docker Compose %s must support %s", command, option)
			}
		}
	}
	return nil
}
