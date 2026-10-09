package resources

import (
	"context"
	"sync"
)

type Executor struct {
	DockerBin string
	StateDir  string
	mu        sync.Mutex
}

func (d *Executor) Prepare(ctx context.Context, plan *Plan) (Runtime, error) {
	stateDir := d.StateDir
	runtime := Runtime{Env: map[string]string{}, Mounts: map[string]string{}}
	if plan == nil {
		return runtime, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	execution := *plan
	store := resourceStateStore{stateDir: stateDir, key: execution.Key}
	previous, err := store.load()
	if err != nil {
		return runtime, err
	}
	if previous != nil {
		for _, id := range execution.Release {
			owner, exists := previous.Ownership[id]
			if !exists {
				continue
			}
			if err := d.cleanupResources(ctx, execution.Key, map[string]Cleanup{id: owner}, stateDir); err != nil {
				return runtime, err
			}
			if err := store.forget(id); err != nil {
				return runtime, err
			}
		}
	}
	if err := store.save(execution); err != nil {
		return runtime, err
	}
	if _, err := loadSecrets(store.directory(), execution.Secrets); err != nil {
		return runtime, err
	}
	resolved, err := resolveExecution(execution, stateDir)
	if err != nil {
		return runtime, err
	}
	if err := d.startResourceContainers(ctx, resolved, store.directory()); err != nil {
		return runtime, err
	}
	if err := d.resourceOperations(ctx, resolved, resolved.Prepare); err != nil {
		return runtime, err
	}
	return resolved.Runtime, nil
}

func (d *Executor) Delete(ctx context.Context, key string) error {
	stateDir := d.StateDir
	if key == "" {
		return nil
	}
	if err := validateResourceKey(key); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	store := resourceStateStore{stateDir: stateDir, key: key}
	plan, err := store.load()
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	if err := d.cleanupResources(ctx, key, plan.Ownership, stateDir); err != nil {
		return err
	}
	return store.remove()
}
