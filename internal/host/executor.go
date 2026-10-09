package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Executor struct {
	Directory string
	Bindings  *Bindings
	mu        sync.Mutex
}

func (e *Executor) operationPath(id string) (string, error) {
	if id == "." || !filepath.IsLocal(id) || filepath.Base(id) != id {
		return "", fmt.Errorf("operation ID escapes local directory")
	}
	return filepath.Join(e.Directory, "operations", id+".json"), nil
}

func (e *Executor) Status(id string) (Result, error) {
	path, err := e.operationPath(id)
	if err != nil {
		return Result{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = json.Unmarshal(data, &result)
	return result, err
}

// Recover marks unfinished effects as uncertain; it never replays them after a process restart.
func (e *Executor) Recover() error {
	paths, err := filepath.Glob(filepath.Join(e.Directory, "operations", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var result Result
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
		if result.Status == "running" {
			result.Status = "interrupted"
			result.Error = "host restarted before the operation result was recorded; inspect effects before retrying with a new ID"
			if err := saveResult(path, result); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, request Request, onOutput func(string)) (Result, error) {
	path, err := e.operationPath(request.ID)
	if err != nil {
		return Result{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Result{}, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	e.mu.Lock()
	defer e.mu.Unlock()
	previous, err := e.Status(request.ID)
	if err == nil {
		if previous.Digest != digest {
			return Result{}, fmt.Errorf("operation ID already belongs to a different request")
		}
		return previous, nil
	}
	if !os.IsNotExist(err) {
		return Result{}, err
	}
	result := Result{Status: "running", Digest: digest}
	if err := saveResult(path, result); err != nil {
		return Result{}, err
	}
	if request.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(request.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	err = nil
	resolved := request
	if request.ResolveSecrets {
		resolved, err = e.resolve(request)
	}
	if err == nil {
		result.Output, result.Data, err = e.apply(ctx, resolved, onOutput)
	}
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
	} else {
		result.Status = "completed"
	}
	if err := saveResult(path, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (e *Executor) apply(ctx context.Context, r Request, onOutput func(string)) (string, json.RawMessage, error) {
	switch r.Action {
	case "binding.read":
		binding, exists := e.Bindings.Get(r.BindingID)
		if !exists {
			return "", json.RawMessage(`null`), nil
		}
		data, err := json.Marshal(binding)
		return "", data, err
	case "binding.set":
		if r.Binding == nil {
			return "", nil, fmt.Errorf("binding is required")
		}
		return "", nil, e.Bindings.Set(r.BindingID, *r.Binding)
	case "binding.remove":
		return "", nil, e.Bindings.Remove(r.BindingID)
	case "info":
		data, err := json.Marshal(struct {
			Directory string `json:"directory"`
		}{e.Directory})
		return "", data, err
	case "exec":
		if len(r.Command) == 0 {
			return "", nil, fmt.Errorf("exec requires argv")
		}
		command := exec.CommandContext(ctx, r.Command[0], r.Command[1:]...)
		command.Dir = r.Directory
		command.Env = os.Environ()
		for name, value := range r.Env {
			command.Env = append(command.Env, name+"="+value)
		}
		command.Stdin = strings.NewReader(r.Input)
		var output bytes.Buffer
		writer := &streamWriter{output: &output, onOutput: onOutput}
		command.Stdout = writer
		command.Stderr = writer
		err := command.Run()
		return output.String(), nil, err
	case "write":
		mode := os.FileMode(0600)
		if r.Executable {
			mode = 0700
		}
		return "", nil, writeFile(r.Path, r.Data, mode)
	case "read":
		data, err := os.ReadFile(r.Path)
		if os.IsNotExist(err) {
			return "", json.RawMessage(`null`), nil
		}
		if err != nil {
			return "", nil, err
		}
		encoded, err := json.Marshal(data)
		return "", encoded, err
	case "remove":
		return "", nil, os.RemoveAll(r.Path)
	case "archive":
		return "", nil, extractTarGzip(r.Path, r.Data)
	case "secrets":
		for scope, names := range r.Secrets {
			if err := e.ensureSecrets(scope, names); err != nil {
				return "", nil, err
			}
		}
		return "", nil, nil
	default:
		return "", nil, fmt.Errorf("unsupported host action %q", r.Action)
	}
}

type streamWriter struct {
	mu       sync.Mutex
	output   *bytes.Buffer
	onOutput func(string)
}

func (w *streamWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.onOutput != nil {
		w.onOutput(string(data))
	}
	if w.output.Len()+len(data) > 16<<20 {
		return 0, fmt.Errorf("operation output exceeds limit")
	}
	return w.output.Write(data)
}

func saveResult(path string, result Result) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return writeFile(path, data, 0600)
}
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".host-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
