package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RuntimeBindings resolves the active container's port instead of trusting a
// host port saved before Docker last restarted. It never starts containers.
type RuntimeBindings struct {
	bindings *Bindings
	client   *http.Client
	socketMu sync.Mutex
	socket   string
}

func NewRuntimeBindings(bindings *Bindings) *RuntimeBindings {
	resolver := &RuntimeBindings{bindings: bindings}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			socket, err := resolver.dockerSocket(ctx)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
	}
	resolver.client = &http.Client{
		Transport: transport,
		Timeout:   3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return resolver
}

// Discover the same local daemon used by publishing's Docker CLI. Do this lazily
// so an unavailable Docker installation does not prevent the agent connecting.
func (r *RuntimeBindings) dockerSocket(ctx context.Context) (string, error) {
	r.socketMu.Lock()
	defer r.socketMu.Unlock()
	if r.socket != "" {
		return r.socket, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" || os.Getenv("DOCKER_CONTEXT") != "" {
		var err error
		endpoint, err = releaseCommand(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return "", fmt.Errorf("resolve Docker context: %w", err)
		}
	}
	if !strings.HasPrefix(endpoint, "unix:///") {
		return "", fmt.Errorf("deployment forwarding requires a local Docker Unix socket")
	}
	r.socket = strings.TrimPrefix(endpoint, "unix://")
	return r.socket, nil
}

func (r *RuntimeBindings) Close() {
	r.client.CloseIdleConnections()
}

func (r *RuntimeBindings) Resolve(ctx context.Context, deploymentID string) (Binding, error) {
	binding, exists := r.bindings.Get(deploymentID)
	if !exists {
		return Binding{}, fmt.Errorf("deployment %s not found", deploymentID)
	}
	var release ReleaseSummary
	if err := json.Unmarshal(binding.Metadata, &release); err != nil {
		return Binding{}, fmt.Errorf("decode active binding: %w", err)
	}
	if !containerIdentity.MatchString(release.ContainerID) {
		return Binding{}, fmt.Errorf("active binding has an invalid container ID")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+release.ContainerID+"/json", nil)
	if err != nil {
		return Binding{}, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return Binding{}, fmt.Errorf("inspect active container: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Binding{}, fmt.Errorf("inspect active container: Docker returned HTTP %d", response.StatusCode)
	}
	var container struct {
		ID    string `json:"Id"`
		State struct {
			Running    bool
			Restarting bool
			Paused     bool
		}
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string
			}
		}
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&container); err != nil {
		return Binding{}, fmt.Errorf("decode active container: %w", err)
	}
	if !containerIdentity.MatchString(container.ID) || !strings.HasPrefix(container.ID, release.ContainerID) {
		return Binding{}, fmt.Errorf("Docker returned a different container identity")
	}
	if !container.State.Running || container.State.Restarting || container.State.Paused {
		return Binding{}, fmt.Errorf("active container is not running stably")
	}
	port := 0
	for containerPort, entries := range container.NetworkSettings.Ports {
		if !strings.HasSuffix(containerPort, "/tcp") {
			continue
		}
		for _, mapping := range entries {
			if mapping.HostIP != "127.0.0.1" {
				continue
			}
			if port != 0 {
				return Binding{}, fmt.Errorf("active container has multiple loopback TCP ports")
			}
			port, err = strconv.Atoi(mapping.HostPort)
			if err != nil || port < 1 || port > 65535 {
				return Binding{}, fmt.Errorf("active container has an invalid loopback TCP port")
			}
		}
	}
	if port == 0 {
		return Binding{}, fmt.Errorf("active container has no loopback TCP port")
	}
	updated := binding
	if port != binding.Port || port != release.Port {
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(binding.Metadata, &metadata); err != nil {
			return Binding{}, err
		}
		metadata["port"] = json.RawMessage(strconv.Itoa(port))
		updated.Port = port
		updated.Metadata, err = json.Marshal(metadata)
		if err != nil {
			return Binding{}, err
		}
	}

	// A publication or unpublish may finish while Docker is being inspected.
	// Never overwrite or route using the superseded binding in that case.
	r.bindings.mu.Lock()
	defer r.bindings.mu.Unlock()
	current, exists := r.bindings.data[deploymentID]
	if exists && current.Port == updated.Port && bytes.Equal(current.Metadata, updated.Metadata) {
		return current, nil
	}
	if !exists || current.Port != binding.Port || !bytes.Equal(current.Metadata, binding.Metadata) {
		return Binding{}, fmt.Errorf("active binding changed during container inspection")
	}
	r.bindings.data[deploymentID] = updated
	if err := r.bindings.saveLocked(); err != nil {
		r.bindings.data[deploymentID] = binding
		return Binding{}, fmt.Errorf("persist reconciled binding: %w", err)
	}
	return updated, nil
}
