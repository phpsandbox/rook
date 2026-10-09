package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecutionReplayAndRestartNeverRepeatEffects(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{Directory: dir}
	path := filepath.Join(dir, "count")
	request := Request{ID: "effect", Action: "exec", Command: []string{"sh", "-c", `printf x >> "$1"; printf done`, "sh", path}}
	result, err := e.Execute(context.Background(), request, nil)
	if err != nil || result.Status != "completed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	next := &Executor{Directory: dir}
	if err := next.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Execute(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "x" {
		t.Fatal("replayed effect")
	}
	request.Command = []string{"false"}
	if _, err := next.Execute(context.Background(), request, nil); err == nil {
		t.Fatal("ID reused for different effect")
	}
	operationPath, _ := next.operationPath("uncertain")
	if err := saveResult(operationPath, Result{Status: "running", Digest: "digest"}); err != nil {
		t.Fatal(err)
	}
	if err := next.Recover(); err != nil {
		t.Fatal(err)
	}
	interrupted, err := next.Status("uncertain")
	if err != nil || interrupted.Status != "interrupted" {
		t.Fatalf("%+v %v", interrupted, err)
	}
}
func TestPrivateSuppliedValuesAreWrittenWithoutSavingRequest(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{Directory: dir}
	path := filepath.Join(dir, "private.json")
	value := "supplied-credential"
	request := Request{ID: "write", Action: "write", Path: path, Data: []byte(`{"password":"` + value + `"}`)}
	if result, err := e.Execute(context.Background(), request, nil); err != nil || result.Status != "completed" {
		t.Fatalf("%+v %v", result, err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), value) {
		t.Fatal("supplied value missing")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private file permissions")
	}
	resultPath, _ := e.operationPath("write")
	record, _ := os.ReadFile(resultPath)
	if strings.Contains(string(record), value) {
		t.Fatal("credential saved in operation journal")
	}
}
func TestBindingsPersistAtomicSwitchAndRemoval(t *testing.T) {
	dir := t.TempDir()
	bindings := NewBindings(dir)
	e := &Executor{Directory: dir, Bindings: bindings}
	for _, value := range []string{"first", "second"} {
		result, err := e.Execute(context.Background(), Request{ID: value, Action: "binding.set", BindingID: "app", Binding: &Binding{Metadata: json.RawMessage(`"` + value + `"`), Port: 1234}}, nil)
		if err != nil || result.Status != "completed" {
			t.Fatalf("%+v %v", result, err)
		}
	}
	reloaded := NewBindings(dir)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	binding, exists := reloaded.Get("app")
	if !exists || string(binding.Metadata) != `"second"` {
		t.Fatal("switch not persisted")
	}
	result, err := e.Execute(context.Background(), Request{ID: "read", Action: "binding.read", BindingID: "app"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Binding
	if err := json.Unmarshal(result.Data, &decoded); err != nil || string(decoded.Metadata) != `"second"` {
		t.Fatal("binding wire result")
	}
}
func TestUnsafeArchiveTargetsAreRejected(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", ".."} {
		if _, err := safeBundleTarget(t.TempDir(), name); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestLongEffectAllowsIndependentOperationsAndSuppressesConcurrentReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := &Executor{Directory: t.TempDir(), Bindings: NewBindings(t.TempDir())}
	path := filepath.Join(e.Directory, "effects")
	request := Request{ID: "long", Action: "exec", Command: []string{"sh", "-c", `printf started; sleep 2; printf x >> "$1"`, "sh", path}}
	started := make(chan struct{})
	completed := make(chan Result, 1)
	var once sync.Once
	go func() {
		result, err := e.Execute(ctx, request, func(string) { once.Do(func() { close(started) }) })
		if err != nil {
			t.Error(err)
		}
		completed <- result
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("command did not start")
	}
	short, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	for _, action := range []Request{
		{ID: "info", Action: "info"},
		{ID: "write", Action: "write", Path: filepath.Join(e.Directory, "other"), Data: []byte("value")},
		{ID: "read", Action: "read", Path: filepath.Join(e.Directory, "other")},
		{ID: "binding", Action: "binding.set", BindingID: "app", Binding: &Binding{Port: 1234}},
	} {
		result, err := e.Execute(short, action, nil)
		if err != nil || result.Status != "completed" {
			t.Fatalf("independent %s: %+v %v", action.Action, result, err)
		}
	}
	replay, err := e.Execute(short, request, nil)
	if err != nil || replay.Status != "running" {
		t.Fatalf("concurrent replay: %+v %v", replay, err)
	}
	select {
	case result := <-completed:
		if result.Status != "completed" {
			t.Fatalf("long effect: %+v", result)
		}
	case <-ctx.Done():
		t.Fatal("command did not finish")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "x" {
		t.Fatalf("effect count: %q %v", data, err)
	}
}

func TestCanceledRequestDoesNotStartHostEffect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &Executor{Directory: t.TempDir()}
	path := filepath.Join(e.Directory, "effect")
	_, err := e.Execute(ctx, Request{ID: "expired", Action: "write", Path: path, Data: []byte("bad")}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canceled effect ran: %v", err)
	}
}

func TestCommandTimeoutStopsDescendantsBeforeLateEffects(t *testing.T) {
	e := &Executor{Directory: t.TempDir()}
	path := filepath.Join(e.Directory, "late")
	start := time.Now()
	result, err := e.Execute(context.Background(), Request{
		ID: "timeout", Action: "exec", TimeoutSeconds: 1,
		Command: []string{"sh", "-c", `(sleep 3; printf late > "$1") & wait`, "sh", path},
	}, nil)
	if err != nil || result.Status != "failed" || result.Error != context.DeadlineExceeded.Error() {
		t.Fatalf("timeout: %+v %v", result, err)
	}
	if time.Since(start) >= 2*time.Second {
		t.Fatal("timeout waited for descendant output")
	}
	time.Sleep(time.Until(start.Add(3200 * time.Millisecond)))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("descendant modified host after timeout: %v", err)
	}
	saved, err := e.Status("timeout")
	if err != nil || saved.Status != "failed" {
		t.Fatalf("persisted timeout: %+v %v", saved, err)
	}
}

func TestExitedLeaderDoesNotWaitIndefinitelyForInheritedOutput(t *testing.T) {
	e := &Executor{Directory: t.TempDir()}
	start := time.Now()
	result, err := e.Execute(context.Background(), Request{ID: "pipes", Action: "exec", Command: []string{"sh", "-c", "sleep 30 &"}}, nil)
	if err != nil || result.Status != "failed" || result.Error != exec.ErrWaitDelay.Error() {
		t.Fatalf("inherited output: %+v %v", result, err)
	}
	if time.Since(start) >= 2*time.Second {
		t.Fatal("output waiting was not bounded")
	}
}
