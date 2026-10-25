package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryFenceSurvivesRestartAndRejectsNewEffects(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recovery-fence"), []byte("operator recovery\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Directory: dir, Bindings: NewBindings(dir)}
	if err := executor.Recover(); err != nil {
		t.Fatal(err)
	}
	request := Request{ID: "blocked-effect", Action: "write", Path: filepath.Join(dir, "effect"), Data: []byte("changed")}

	_, err := executor.Execute(context.Background(), request, nil)
	if err == nil || !strings.Contains(err.Error(), "host mutations are fenced") {
		t.Fatalf("fenced execution error: %v", err)
	}
	if _, err := os.Stat(request.Path); !os.IsNotExist(err) {
		t.Fatalf("fenced effect was executed: %v", err)
	}
	if _, err := executor.Status(request.ID); !os.IsNotExist(err) {
		t.Fatalf("rejected request acquired an operation ID: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "recovery-fence")); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), request, nil)
	if err != nil || result.Status != "completed" {
		t.Fatalf("reconciled execution result=%+v error=%v", result, err)
	}
}

func TestRecoveryFenceAllowsPersistedInspection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recovery-fence"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	bindings := NewBindings(dir)
	if err := bindings.Set("application", Binding{Port: 1234}); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{Directory: dir, Bindings: bindings}

	result, err := executor.Execute(context.Background(), Request{ID: "inspect", Action: "binding.read", BindingID: "application"}, nil)
	if err != nil || result.Status != "completed" || !strings.Contains(string(result.Data), "1234") {
		t.Fatalf("persisted inspection result=%+v error=%v", result, err)
	}
}
