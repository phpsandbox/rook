package resources

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"path/filepath"
)

func (d *Executor) cleanupResources(ctx context.Context, key string, ownership map[string]Cleanup, stateDir string) error {
	// Allocations must detach before their projects, volumes, and networks are removed.
	for _, owner := range ownership {
		if len(owner.Operations) == 0 {
			continue
		}
		plan, err := resolveExecution(Plan{Key: key, Project: owner.Project, Dependencies: owner.Dependencies, Prepare: owner.Operations}, stateDir)
		if err != nil {
			return err
		}
		if err := d.resourceOperations(ctx, plan, plan.Prepare); err != nil {
			return err
		}
	}
	for id, owner := range ownership {
		if len(owner.Compose) == 0 {
			continue
		}
		plan, err := resolveExecution(Plan{Key: key, Compose: owner.Compose}, stateDir)
		if err != nil {
			return err
		}
		path := filepath.Join(stateDir, "resources", key, fmt.Sprintf("cleanup-%x.json", sha256.Sum256([]byte(id))))
		if err := writePrivateFile(path, plan.Compose); err != nil {
			return err
		}
		if err := d.compose(ctx, owner.Project, path, "down", "--remove-orphans"); err != nil {
			return err
		}
	}
	for _, kind := range []string{"volume", "network"} {
		for _, owner := range ownership {
			name := owner.Volume
			if kind == "network" {
				name = owner.Network
			}
			if name == "" {
				continue
			}
			exists, err := d.resourceObjectExists(ctx, kind, name)
			if err != nil {
				return err
			}
			if exists {
				if err := exec.CommandContext(ctx, d.DockerBin, kind, "rm", name).Run(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
