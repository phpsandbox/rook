package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type DeploymentState struct {
	ContainerID string `json:"containerId"`
	Port        int    `json:"port"`
	ImageRef    string `json:"imageRef"`
	RouteKey    string `json:"routeKey"`
	StartedAt   string `json:"startedAt"`
}

type StateStore struct {
	mu   sync.Mutex
	dir  string
	data map[string]DeploymentState
}

func NewStateStore(dir string) *StateStore {
	return &StateStore{
		dir:  dir,
		data: map[string]DeploymentState{},
	}
}

func (s *StateStore) Load() error {
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

func (s *StateStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *StateStore) Get(deploymentID string) (DeploymentState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.data[deploymentID]
	return state, ok
}

func (s *StateStore) Set(deploymentID string, state DeploymentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[deploymentID] = state
	return s.saveLocked()
}

func (s *StateStore) Remove(deploymentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, deploymentID)
	return s.saveLocked()
}

func (s *StateStore) All() map[string]DeploymentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]DeploymentState, len(s.data))
	for k, v := range s.data {
		result[k] = v
	}
	return result
}

func (s *StateStore) DeploymentIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	return ids
}

func (s *StateStore) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	return os.WriteFile(s.filePath(), content, 0o644)
}

func (s *StateStore) filePath() string {
	return filepath.Join(s.dir, "deployments.json")
}
