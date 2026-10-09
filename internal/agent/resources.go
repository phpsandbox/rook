package agent

import (
	"context"
)

func (d *DockerManager) PrepareResources(ctx context.Context, plan Plan, stateDir string) (ResourceRuntime, error) {
	runtime := ResourceRuntime{Env: map[string]string{}, Mounts: map[string]string{}}
	if plan.Execution == nil {
		return runtime, nil
	}
	d.resourcesMu.Lock()
	defer d.resourcesMu.Unlock()
	execution := *plan.Execution
	store := resourceStateStore{stateDir: stateDir, key: execution.Key}
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

func (d *DockerManager) DeleteResources(ctx context.Context, key, stateDir string) error {
	if key == "" {
		return nil
	}
	if err := validateResourceKey(key); err != nil {
		return err
	}
	d.resourcesMu.Lock()
	defer d.resourcesMu.Unlock()
	store := resourceStateStore{stateDir: stateDir, key: key}
	plan, err := store.load()
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	resolved, err := resolveExecution(*plan, stateDir)
	if err != nil {
		return err
	}
	if err := d.resourceOperations(ctx, resolved, resolved.Cleanup); err != nil {
		return err
	}
	if err := d.removeResourceContainers(ctx, resolved, store.directory()); err != nil {
		return err
	}
	return store.remove()
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
