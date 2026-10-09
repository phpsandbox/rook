package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
)

type resourceStateStore struct {
	stateDir string
	key      string
}

func (s resourceStateStore) directory() string {
	return filepath.Join(s.stateDir, "resources", s.key)
}

func (s resourceStateStore) load() (*ResourceExecution, error) {
	if err := validateResourceKey(s.key); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(filepath.Join(s.directory(), "execution.json"))
	if os.IsNotExist(err) {
		if _, statError := os.Stat(s.directory()); statError == nil {
			return nil, fmt.Errorf("resource state is incomplete: execution plan is missing")
		} else if !os.IsNotExist(statError) {
			return nil, statError
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plan ResourceExecution
	if err := json.Unmarshal(content, &plan); err != nil {
		return nil, err
	}
	if plan.Key != s.key {
		return nil, fmt.Errorf("resource identity differs from stored state")
	}
	if err := validateResourcePaths(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s resourceStateStore) save(plan ResourceExecution) error {
	previous, err := s.load()
	if err != nil {
		return err
	}
	if previous != nil && (previous.Project != plan.Project || !maps.Equal(previous.Dependencies, plan.Dependencies)) {
		return fmt.Errorf("retained resource dependencies changed; clean up the previous allocation first")
	}
	if previous != nil {
		before, err := resourceInventory(*previous)
		if err != nil {
			return err
		}
		after, err := resourceInventory(plan)
		if err != nil {
			return err
		}
		for name := range before {
			if !after[name] {
				return fmt.Errorf("retained resource %s was omitted; clean up the previous resources first", name)
			}
		}
	}
	if err := os.MkdirAll(s.directory(), 0o700); err != nil {
		return err
	}
	content, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	// Persist cleanup instructions before executing operations, including partial failures.
	return writePrivateFile(filepath.Join(s.directory(), "execution.json"), content)
}

func (s resourceStateStore) remove() error {
	return os.RemoveAll(s.directory())
}

func resourceInventory(plan ResourceExecution) (map[string]bool, error) {
	var doc ComposeDocument
	if err := json.Unmarshal(plan.Compose, &doc); err != nil {
		return nil, err
	}
	inventory := map[string]bool{}
	for service := range doc.Services {
		inventory["service:"+service] = true
	}
	for _, volume := range doc.Volumes {
		inventory["volume:"+volume.Name] = true
	}
	for volume := range plan.Runtime.Mounts {
		inventory["volume:"+volume] = true
	}
	return inventory, nil
}
