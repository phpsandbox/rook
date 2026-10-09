package resources

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

func (s resourceStateStore) load() (*Plan, error) {
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
	var plan Plan
	if err := json.Unmarshal(content, &plan); err != nil {
		return nil, err
	}
	if plan.Key != s.key {
		return nil, fmt.Errorf("resource identity differs from stored state")
	}
	if err := ValidatePaths(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s resourceStateStore) save(plan Plan) error {
	previous, err := s.load()
	if err != nil {
		return err
	}
	ownership := maps.Clone(plan.Ownership)
	if ownership == nil {
		ownership = map[string]Cleanup{}
	}
	if previous != nil {
		// Ownership is explicit and durable. Omitted IDs remain owned until released.
		for id, owner := range previous.Ownership {
			ownership[id] = owner
		}
	}
	plan.Ownership = ownership
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

func (s resourceStateStore) forget(id string) error {
	plan, err := s.load()
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	delete(plan.Ownership, id)
	content, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return writePrivateFile(filepath.Join(s.directory(), "execution.json"), content)
}

func writePrivateFile(path string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".resource-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func validateResourceKey(key string) error {
	if key == "." || !filepath.IsLocal(key) || filepath.Base(key) != key {
		return fmt.Errorf("resource key escapes local state directory")
	}
	return nil
}

func ValidatePaths(plan *Plan) error {
	if plan == nil {
		return nil
	}
	if err := validateResourceKey(plan.Key); err != nil {
		return err
	}
	for key := range plan.Dependencies {
		if err := validateResourceKey(key); err != nil {
			return err
		}
	}
	for _, owner := range plan.Ownership {
		for key := range owner.Dependencies {
			if err := validateResourceKey(key); err != nil {
				return err
			}
		}
	}
	return nil
}
