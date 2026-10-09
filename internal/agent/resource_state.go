package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

type resourceStateStore struct {
	stateDir string
	key      string
}

func (s resourceStateStore) directory() string {
	return filepath.Join(s.stateDir, "resources", s.key)
}

func (s resourceStateStore) load() (*ResourceExecution, error) {
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
	if err := validateResourceExecution(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s resourceStateStore) save(plan ResourceExecution) error {
	previous, err := s.load()
	if err != nil {
		return err
	}
	if previous != nil && (previous.Project != plan.Project || !reflect.DeepEqual(previous.Dependencies, plan.Dependencies)) {
		return fmt.Errorf("retained resource dependencies changed; clean up the previous allocation first")
	}
	if err := os.MkdirAll(s.directory(), 0o700); err != nil {
		return err
	}
	content, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	// Persist cleanup instructions before executing operations, including partial failures.
	return os.WriteFile(filepath.Join(s.directory(), "execution.json"), content, 0o600)
}

func (s resourceStateStore) remove() error {
	return os.RemoveAll(s.directory())
}
