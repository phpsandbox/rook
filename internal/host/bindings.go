package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Binding struct {
	Port     int             `json:"port" msgpack:"port"`
	Metadata json.RawMessage `json:"metadata,omitempty" msgpack:"metadata,omitempty"`
}

type Bindings struct {
	mu   sync.Mutex
	dir  string
	data map[string]Binding
}

func NewBindings(dir string) *Bindings {
	return &Bindings{
		dir:  dir,
		data: map[string]Binding{},
	}
}

func (s *Bindings) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.filePath()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}

	return json.Unmarshal(content, &s.data)
}

func (s *Bindings) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Bindings) Get(deploymentID string) (Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.data[deploymentID]
	return state, ok
}

func (s *Bindings) Set(deploymentID string, state Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.data[deploymentID]
	s.data[deploymentID] = state
	if err := s.saveLocked(); err != nil {
		if exists {
			s.data[deploymentID] = previous
		} else {
			delete(s.data, deploymentID)
		}
		return err
	}
	return nil
}

func (s *Bindings) Remove(deploymentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.data[deploymentID]
	delete(s.data, deploymentID)
	if err := s.saveLocked(); err != nil {
		if exists {
			s.data[deploymentID] = previous
		}
		return err
	}
	return nil
}

func (s *Bindings) All() map[string]Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]Binding, len(s.data))
	for k, v := range s.data {
		result[k] = v
	}
	return result
}

func (s *Bindings) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	return ids
}

func (s *Bindings) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return writeFile(s.filePath(), content, 0600)
}

func (s *Bindings) filePath() string {
	return filepath.Join(s.dir, "bindings.json")
}
