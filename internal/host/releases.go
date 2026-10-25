package host

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var releaseIdentity = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)
var containerIdentity = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
var serviceIdentity = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@:-]{0,255}$`)

type ReleaseSummary struct {
	ReleaseID       string   `json:"releaseId"`
	ImageRef        string   `json:"imageRef"`
	ContainerID     string   `json:"containerId"`
	RoleContainers  []string `json:"roleContainers,omitempty"`
	Port            int      `json:"port"`
	HealthPath      string   `json:"healthPath"`
	ShutdownSeconds *int     `json:"shutdownSeconds,omitempty"`
}

func (r ReleaseSummary) containers() ([]string, error) {
	ids := r.RoleContainers
	if len(ids) == 0 {
		ids = []string{r.ContainerID}
	}
	for _, id := range ids {
		if !containerIdentity.MatchString(id) {
			return nil, fmt.Errorf("retained release contains invalid container ID")
		}
	}
	if !slices.Contains(ids, r.ContainerID) {
		return nil, fmt.Errorf("retained web container is absent from role containers")
	}
	return ids, nil
}

func (r ReleaseSummary) identity() string {
	if r.ReleaseID != "" {
		return r.ReleaseID
	}
	return r.ContainerID
}

type UncertainOperation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type ReleaseInventory struct {
	CurrentReleaseID    string               `json:"currentReleaseId"`
	BindingPort         int                  `json:"bindingPort"`
	Releases            []ReleaseSummary     `json:"releases"`
	UncertainOperations []UncertainOperation `json:"uncertainOperations"`
	Fenced              bool                 `json:"fenced"`
}

type RestoreReleaseOptions struct {
	Deployment            string
	Release               string
	ExpectCurrent         string
	Service               string
	AcknowledgeOperations []string
}

type RecoveryVerification struct {
	ID         string `json:"id"`
	ReleaseID  string `json:"releaseId"`
	Port       int    `json:"port"`
	VerifiedAt string `json:"verifiedAt"`
}

type RestoreReleaseResult struct {
	RestoredReleaseID  string               `json:"restoredReleaseId"`
	LocalPort          int                  `json:"localPort"`
	Fenced             bool                 `json:"fenced"`
	Verification       RecoveryVerification `json:"verification"`
	RookServiceStarted bool                 `json:"rookServiceStarted"`
	DatabaseRestored   bool                 `json:"databaseRestored"`
	Next               string               `json:"next"`
}

// ReleaseStore operates on retained local state without connecting to the control plane.
type ReleaseStore struct {
	directory string
	command   func(context.Context, string, ...string) (string, error)
	healthy   func(context.Context, int, string) error
}

func NewReleaseStore(directory string) (*ReleaseStore, error) {
	directory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("state path is not a directory")
	}
	return &ReleaseStore{directory: directory, command: releaseCommand, healthy: waitReleaseHealthy}, nil
}

func (s *ReleaseStore) List(deployment string) (ReleaseInventory, error) {
	if !releaseIdentity.MatchString(deployment) {
		return ReleaseInventory{}, fmt.Errorf("invalid deployment ID")
	}
	bindings := NewBindings(s.directory)
	if err := bindings.Load(); err != nil {
		return ReleaseInventory{}, err
	}
	inventory := ReleaseInventory{Releases: []ReleaseSummary{}}
	if binding, ok := bindings.Get(deployment); ok {
		var current ReleaseSummary
		if err := json.Unmarshal(binding.Metadata, &current); err != nil {
			return inventory, err
		}
		inventory.CurrentReleaseID = current.identity()
		inventory.BindingPort = binding.Port
	}
	paths, err := filepath.Glob(filepath.Join(s.directory, "releases", deployment, "*.json"))
	if err != nil {
		return inventory, err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return inventory, err
		}
		var release ReleaseSummary
		if err := json.Unmarshal(data, &release); err != nil {
			return inventory, err
		}
		if _, err := release.containers(); err != nil {
			return inventory, err
		}
		if release.ReleaseID != strings.TrimSuffix(filepath.Base(path), ".json") {
			return inventory, fmt.Errorf("retained release identity differs from filename")
		}
		inventory.Releases = append(inventory.Releases, release)
	}
	inventory.UncertainOperations, err = s.uncertainOperations()
	if err != nil {
		return inventory, err
	}
	_, err = os.Stat(filepath.Join(s.directory, "recovery-fence"))
	inventory.Fenced = err == nil
	if err != nil && !os.IsNotExist(err) {
		return inventory, err
	}
	return inventory, nil
}

func (s *ReleaseStore) Inspect(deployment, release string) (ReleaseInventory, error) {
	if !releaseIdentity.MatchString(release) {
		return ReleaseInventory{}, fmt.Errorf("invalid release ID")
	}
	inventory, err := s.List(deployment)
	if err != nil {
		return inventory, err
	}
	for _, entry := range inventory.Releases {
		if entry.ReleaseID == release {
			inventory.Releases = []ReleaseSummary{entry}
			return inventory, nil
		}
	}
	return inventory, fmt.Errorf("retained release %q not found", release)
}

func (s *ReleaseStore) uncertainOperations() ([]UncertainOperation, error) {
	paths, err := filepath.Glob(filepath.Join(s.directory, "operations", "*.json"))
	if err != nil {
		return nil, err
	}
	uncertain := []UncertainOperation{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var result Result
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		if result.Status == "running" || result.Status == "interrupted" {
			uncertain = append(uncertain, UncertainOperation{ID: strings.TrimSuffix(filepath.Base(path), ".json"), Status: result.Status})
		}
	}
	return uncertain, nil
}

func (s *ReleaseStore) Restore(ctx context.Context, options RestoreReleaseOptions) (RestoreReleaseResult, error) {
	var result RestoreReleaseResult
	if !releaseIdentity.MatchString(options.Deployment) || !releaseIdentity.MatchString(options.Release) || !releaseIdentity.MatchString(options.ExpectCurrent) {
		return result, fmt.Errorf("valid deployment, release and expected current IDs are required")
	}
	if !serviceIdentity.MatchString(options.Service) {
		return result, fmt.Errorf("a valid Rook service name is required")
	}
	lock, err := os.OpenFile(filepath.Join(s.directory, "offline-recovery.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, fmt.Errorf("another offline restore holds the lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := s.prepareOfflineRestore(ctx, options); err != nil {
		return result, err
	}
	bindings := NewBindings(s.directory)
	if err := bindings.Load(); err != nil {
		return result, err
	}
	binding, ok := bindings.Get(options.Deployment)
	if !ok {
		return result, fmt.Errorf("deployment binding not found")
	}
	var current ReleaseSummary
	if err := json.Unmarshal(binding.Metadata, &current); err != nil {
		return result, err
	}
	if current.identity() != options.ExpectCurrent {
		return result, fmt.Errorf("current release changed; inspect again before restoring")
	}
	oldIDs, err := current.containers()
	if err != nil {
		return result, err
	}
	seconds := 60
	if current.ShutdownSeconds != nil {
		seconds = *current.ShutdownSeconds
	}
	if seconds < 1 || seconds > 600 {
		return result, fmt.Errorf("invalid current release shutdown timeout")
	}
	path := filepath.Join(s.directory, "releases", options.Deployment, options.Release+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	var target ReleaseSummary
	if err := json.Unmarshal(data, &target); err != nil {
		return result, err
	}
	if target.ReleaseID != options.Release {
		return result, fmt.Errorf("retained release identity differs from requested release")
	}
	if target.Port < 1 || target.Port > 65535 {
		return result, fmt.Errorf("retained release has an invalid local port")
	}
	healthURL, err := url.ParseRequestURI(target.HealthPath)
	if err != nil || !strings.HasPrefix(target.HealthPath, "/") || strings.HasPrefix(target.HealthPath, "//") || healthURL.Host != "" {
		return result, fmt.Errorf("retained release is missing its verified health path")
	}
	targetIDs, err := target.containers()
	if err != nil {
		return result, err
	}
	stopped, err := s.inspectRetainedContainers(ctx, targetIDs)
	if err != nil {
		return result, err
	}
	port, err := s.startRetained(ctx, target, targetIDs)
	if err != nil {
		// Cleanup must finish even when the operator canceled the original command.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 35*time.Second)
		defer cancel()
		if len(stopped) > 0 {
			_, cleanupErr := s.command(cleanupCtx, "docker", append([]string{"stop", "--time", "30"}, stopped...)...)
			if cleanupErr != nil {
				return result, errors.Join(err, fmt.Errorf("stop newly restarted containers: %w", cleanupErr))
			}
		}
		return result, err
	}
	verificationID, err := recoveryUUID()
	if err != nil {
		return result, err
	}
	verification := RecoveryVerification{ID: verificationID, ReleaseID: options.Release, Port: port, VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	metadata, err := verifiedReleaseMetadata(data, current, verification)
	if err != nil {
		return result, err
	}
	if err := writeFile(path, metadata, 0600); err != nil {
		return result, fmt.Errorf("save verified release; binding preserved: %w", err)
	}
	if err := bindings.Set(options.Deployment, Binding{Port: port, Metadata: metadata}); err != nil {
		return result, fmt.Errorf("save restored binding: %w", err)
	}
	oldIDs = slices.DeleteFunc(oldIDs, func(id string) bool { return slices.Contains(targetIDs, id) })
	if len(oldIDs) > 0 {
		if _, err := s.command(ctx, "docker", append([]string{"stop", "--time", strconv.Itoa(seconds)}, oldIDs...)...); err != nil {
			return result, fmt.Errorf("restored binding saved; prior processes may still be running: %w", err)
		}
	}
	return RestoreReleaseResult{RestoredReleaseID: options.Release, LocalPort: port, Fenced: true, Verification: verification,
		Next: "Verify workflows locally; start Rook when its authentication service is available. Reconcile control-plane state and uncertain operations before removing recovery-fence."}, nil
}

func (s *ReleaseStore) prepareOfflineRestore(ctx context.Context, options RestoreReleaseOptions) error {
	load, err := s.command(ctx, "systemctl", "show", options.Service, "--property=LoadState", "--value")
	if err != nil {
		return err
	}
	active, err := s.command(ctx, "systemctl", "show", options.Service, "--property=ActiveState", "--value")
	if err != nil {
		return err
	}
	if load != "loaded" || (active != "inactive" && active != "failed") {
		return fmt.Errorf("stop the configured Rook systemd service before offline restore")
	}
	fence, err := json.Marshal(map[string]string{"deploymentId": options.Deployment, "releaseId": options.Release})
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.directory, "recovery-fence"), fence, 0600); err != nil {
		return err
	}
	uncertain, err := s.uncertainOperations()
	if err != nil {
		return err
	}
	pending := make([]string, 0, len(uncertain))
	for _, entry := range uncertain {
		pending = append(pending, entry.ID)
	}
	acknowledged := slices.Clone(options.AcknowledgeOperations)
	slices.Sort(acknowledged)
	acknowledged = slices.Compact(acknowledged)
	if !slices.Equal(pending, acknowledged) {
		return fmt.Errorf("inspect and explicitly acknowledge each uncertain operation: %s", strings.Join(pending, ", "))
	}
	return nil
}

func (s *ReleaseStore) inspectRetainedContainers(ctx context.Context, targetIDs []string) ([]string, error) {
	stopped := []string{}
	for _, id := range targetIDs {
		image, err := s.command(ctx, "docker", "inspect", "--format", "{{.Image}}", id)
		if err != nil {
			return nil, err
		}
		if _, err := s.command(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image); err != nil {
			return nil, err
		}
		running, err := s.command(ctx, "docker", "inspect", "--format", "{{.State.Running}}", id)
		if err != nil {
			return nil, err
		}
		if running == "false" {
			stopped = append(stopped, id)
		} else if running != "true" {
			return nil, fmt.Errorf("invalid container running state")
		}
	}
	return stopped, nil
}

func verifiedReleaseMetadata(data []byte, current ReleaseSummary, verification RecoveryVerification) ([]byte, error) {
	// Preserve opaque metadata supplied by the control plane, including future fields.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	updates := map[string]any{"port": verification.Port, "recoveryVerification": verification}
	if current.identity() != verification.ReleaseID {
		updates["previousReleaseId"] = current.identity()
		updates["previousContainerId"] = current.ContainerID
	}
	for key, value := range updates {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	metadata, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return metadata, nil
}

func (s *ReleaseStore) startRetained(ctx context.Context, target ReleaseSummary, ids []string) (int, error) {
	if _, err := s.command(ctx, "docker", append([]string{"start"}, ids...)...); err != nil {
		return 0, err
	}
	data, err := s.command(ctx, "docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", target.ContainerID)
	if err != nil {
		return 0, err
	}
	var mappings map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(data), &mappings); err != nil {
		return 0, err
	}
	ports := []int{}
	for _, entries := range mappings {
		for _, mapping := range entries {
			if mapping.HostIP == "127.0.0.1" {
				port, err := strconv.Atoi(mapping.HostPort)
				if err != nil || port < 1 || port > 65535 {
					return 0, fmt.Errorf("invalid retained web port")
				}
				ports = append(ports, port)
			}
		}
	}
	if len(ports) != 1 {
		return 0, fmt.Errorf("retained web process does not have exactly one loopback port")
	}
	if err := s.healthy(ctx, ports[0], target.HealthPath); err != nil {
		return 0, err
	}
	for _, id := range ids {
		status, err := s.command(ctx, "docker", "inspect", "--format", "{{.State.Running}} {{.State.Restarting}}", id)
		if err != nil {
			return 0, err
		}
		if status != "true false" {
			return 0, fmt.Errorf("retained process is not running stably; binding preserved")
		}
	}
	return ports[0], nil
}

func recoveryUUID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func releaseCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 660*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	configureProcess(command)
	output, err := command.CombinedOutput()
	if err != nil {
		if command.Process != nil {
			_ = command.Cancel()
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func waitReleaseHealthy(ctx context.Context, port int, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("retained release did not pass readiness; binding preserved: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
