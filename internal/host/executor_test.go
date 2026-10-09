package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func TestPrivateSecretReferencesPersistAndResolveWithoutSavingRequest(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{Directory: dir}
	if result, err := e.Execute(context.Background(), Request{ID: "ensure", Action: "secrets", Secrets: map[string][]string{"service": {"password"}}}, nil); err != nil || result.Status != "completed" {
		t.Fatalf("%+v %v", result, err)
	}
	secrets, err := loadSecrets(filepath.Join(dir, "secrets", "service"), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "private.json")
	request := Request{ID: "write", Action: "write", Path: path, Data: []byte(`{"password":"{{secret:service:password}}"}`), ResolveSecrets: true}
	if result, err := e.Execute(context.Background(), request, nil); err != nil || result.Status != "completed" {
		t.Fatalf("%+v %v", result, err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), secrets["password"]) {
		t.Fatal("file secret reference unresolved")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private file permissions")
	}
	resultPath, _ := e.operationPath("write")
	record, _ := os.ReadFile(resultPath)
	if strings.Contains(string(record), secrets["password"]) {
		t.Fatal("credential saved in operation journal")
	}
	request.ID = "literal"
	request.ResolveSecrets = false
	request.Path = filepath.Join(dir, "state.json")
	if _, err := e.Execute(context.Background(), request, nil); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(request.Path)
	if !strings.Contains(string(content), "{{secret:") {
		t.Fatal("state lost secret references")
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
