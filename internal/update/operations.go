package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

var operationID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Operation struct {
	ID            string `json:"id"`
	TargetVersion string `json:"targetVersion"`
	Status        string `json:"status"`
	UpdatedAt     string `json:"updatedAt"`
}

func (o Operation) Pending() bool {
	return o.Status == "requested" || o.Status == "waiting" || o.Status == "verifying" || o.Status == "installing"
}

func (o Operation) valid() bool {
	_, versionOK := parseVersion(o.TargetVersion)
	return operationID.MatchString(o.ID) && versionOK && (o.Pending() || o.Status == "succeeded" || o.Status == "failed" || o.Status == "rolled_back")
}

// Operations persists the most recent request across daemon restarts. The
// privileged helper still independently verifies the selected release.
type Operations struct {
	mu      sync.Mutex
	path    string
	current *Operation
}

func OpenOperations(directory string) (*Operations, error) {
	o := &Operations{path: filepath.Join(directory, "agent-update.json")}
	data, err := os.ReadFile(o.path)
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	var current Operation
	if len(data) > 4096 || json.Unmarshal(data, &current) != nil || !current.valid() {
		return nil, errors.New("invalid saved agent update")
	}
	o.current = &current
	return o, nil
}

func (o *Operations) Snapshot() *Operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current == nil {
		return nil
	}
	copy := *o.current
	return &copy
}

func (o *Operations) Request(id, target, installed string) (Operation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	request := Operation{ID: id, TargetVersion: target, Status: "waiting"}
	if !request.valid() {
		return Operation{}, errors.New("invalid update request")
	}
	if o.current != nil && o.current.ID == id {
		if o.current.TargetVersion != target {
			return Operation{}, errors.New("update request identity changed")
		}
		return *o.current, nil
	}
	if o.current != nil && o.current.Pending() {
		return Operation{}, errors.New("another update is pending")
	}
	next, _ := parseVersion(target)
	current, ok := parseVersion(installed)
	if !ok || next[0] != current[0] || next[1] != current[1] || !newer(next, current) {
		return Operation{}, errors.New("target must be a newer stable patch in the installed release line")
	}
	if err := o.save(request); err != nil {
		return Operation{}, err
	}
	return *o.current, nil
}

func (o *Operations) Report(result Operation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !result.valid() || o.current == nil || result.ID != o.current.ID || result.TargetVersion != o.current.TargetVersion {
		return errors.New("update result does not match the pending request")
	}
	if !o.current.Pending() {
		if result.Status == o.current.Status {
			return nil
		}
		return errors.New("update already completed")
	}
	return o.save(result)
}

func (o *Operations) save(operation Operation) error {
	operation.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	if err := durableWrite(o.path, data, 0600); err != nil {
		return fmt.Errorf("save agent update: %w", err)
	}
	o.current = &operation
	return nil
}

func newer(a, b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
