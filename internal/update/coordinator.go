package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const SocketPath = "/run/rook/update.sock"

var errUpdateDeferred = errors.New("agent busy or not connected")

// Coordinator excludes updates and host commands from one another and exposes
// owner requests and the automatic-update policy to the privileged helper.
type Coordinator struct {
	mu                      sync.Mutex
	version                 string
	control, data, draining bool
	active                  int
	drain                   func(context.Context) error
	resume                  func()
	lease                   *time.Timer
	operations              *Operations
	autoUpdate              bool
}

type Status struct {
	Version    string     `json:"version"`
	Ready      bool       `json:"ready"`
	Busy       bool       `json:"busy"`
	AutoUpdate bool       `json:"autoUpdate"`
	Operation  *Operation `json:"operation,omitempty"`
}

func NewCoordinator(version string, drain func(context.Context) error, resume func()) *Coordinator {
	return &Coordinator{version: version, drain: drain, resume: resume}
}

func (c *Coordinator) BeginCommand() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining {
		return false
	}
	c.active++
	return true
}

func (c *Coordinator) EndCommand()             { c.mu.Lock(); c.active--; c.mu.Unlock() }
func (c *Coordinator) ControlReady(ready bool) { c.mu.Lock(); c.control = ready; c.mu.Unlock() }
func (c *Coordinator) DataReady(ready bool)    { c.mu.Lock(); c.data = ready; c.mu.Unlock() }

func (c *Coordinator) abort() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease != nil {
		c.lease.Stop()
	}
	c.resume()
	c.draining = false
}

func (c *Coordinator) Serve(ctx context.Context, path string) (func(), error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("update socket path is not a socket")
		}
		if connection, err := net.DialTimeout("unix", path, time.Second); err == nil {
			connection.Close()
			return nil, fmt.Errorf("another update coordinator is already listening")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("open update socket (use the systemd installer): %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		status := Status{Version: c.version, Ready: c.control && c.data && !c.draining, AutoUpdate: c.autoUpdate, Busy: c.active != 0}
		if c.operations != nil {
			status.Operation = c.operations.Snapshot()
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("POST /result", func(w http.ResponseWriter, r *http.Request) {
		var result Operation
		if c.operations == nil || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&result) != nil {
			http.Error(w, "invalid update result", http.StatusBadRequest)
			return
		}
		if err := c.operations.Report(result); err != nil {
			http.Error(w, "update result rejected", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /prepare", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		if c.active != 0 || c.draining || !c.control || !c.data {
			c.mu.Unlock()
			http.Error(w, "agent busy or not connected; update deferred", http.StatusConflict)
			return
		}
		c.draining = true
		// Resume automatically if the privileged helper dies before restarting us.
		c.lease = time.AfterFunc(3*time.Minute, c.abort)
		c.mu.Unlock()
		drainCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := c.drain(drainCtx); err != nil || r.Context().Err() != nil {
			c.abort()
			http.Error(w, "could not drain agent", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /resume", func(w http.ResponseWriter, r *http.Request) { c.abort(); w.WriteHeader(http.StatusNoContent) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 40 * time.Second, MaxHeaderBytes: 4096}
	go func() { _ = server.Serve(listener) }()
	stopContext := context.AfterFunc(ctx, func() { _ = server.Close() })
	return func() { stopContext(); _ = server.Close(); c.abort() }, nil
}

func socketClient(path string) *http.Client {
	return &http.Client{
		Timeout: 40 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}},
	}
}

func daemonStatus(ctx context.Context, client *http.Client) (Status, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://rook/status", nil)
	response, err := client.Do(request)
	if err != nil {
		return Status{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("agent status: %s", response.Status)
	}
	var status Status
	err = json.NewDecoder(http.MaxBytesReader(nil, response.Body, 4096)).Decode(&status)
	return status, err
}

func daemonAction(ctx context.Context, client *http.Client, action string) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://rook/"+action, nil)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return errUpdateDeferred
	}
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("agent %s: %s", action, response.Status)
	}
	return nil
}

// Enable must run before serving the socket or starting the connection loops.
func (c *Coordinator) Enable(directory string, automatic bool) error {
	operations, err := OpenOperations(directory)
	if err != nil {
		return err
	}
	c.operations = operations
	c.autoUpdate = automatic
	return nil
}

func (c *Coordinator) Request(id, target string) (Operation, error) {
	if c.operations == nil {
		return Operation{}, errors.New("managed updates are disabled")
	}
	return c.operations.Request(id, target, c.version)
}

func (c *Coordinator) Operation() *Operation {
	if c.operations == nil {
		return nil
	}
	return c.operations.Snapshot()
}

func reportResult(ctx context.Context, client *http.Client, operation Operation, status string) error {
	operation.Status = status
	data, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://rook/result", bytes.NewReader(data))
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("report update result: %s", response.Status)
	}
	return nil
}
